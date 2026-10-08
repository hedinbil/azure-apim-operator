/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs replay the 25-28 Sep 2026 incident against the fixed operator. Then, one
// APIMAPIDeployment re-imported a 1.75 MB OpenAPI document about 800 times in 69 hours:
// the async wait gave up after 3 minutes, every error path requeued after a fixed 60 s
// with no cap, and the overlapping imports answered 412 PreconditionFailed, 422
// "Management API timed out" and, from the async operation, InternalServerError
// "DeadOperationMonitor".
//
// The replay drives the real reconcilers against envtest and a fake ARM that behaves the
// same way, on a fake clock, for the same 69 hours. The fixed operator must send at most
// five writes, end Stalled and say so exactly once with the line the Datadog monitor
// matches. What 0.30.0 would have sent against the same fake is worked out, not run, in
// incident_replay_helpers_test.go (oldImportPUTsFloor, oldImportPUTsForScript).

// stalledLogMessage is the Datadog monitor's query text, spelled out here rather than
// taken from msgWriteStalled, so renaming the constant's value fails this file.
const stalledLogMessage = "🛑 APIM write stalled"

// incidentDocSize is the size of the OpenAPI document that was being imported.
const incidentDocSize = 1835008 // 1.75 MiB

// incidentDoc is a valid OpenAPI document of incidentDocSize bytes.
var incidentDoc = func() string {
	head := `{"openapi":"3.0.0","info":{"title":"incident","version":"1.0.0","description":"`
	tail := `"},"paths":{}}`
	return head + strings.Repeat("x", incidentDocSize-len(head)-len(tail)) + tail
}()

// incidentSmallDoc is the document every other spec imports: the 1.75 MB one costs a
// download and a hash on every one of a replay's ~130 reconciles, which only the main
// replay needs to pay.
const incidentSmallDoc = `{"openapi":"3.0.0","info":{"title":"incident","version":"1.0.0"},"paths":{}}`

// incidentCounter keeps resource names unique across specs.
var incidentCounter atomic.Int32

// incidentRetryPolicy is the production retry policy on the replay's clock. A zero seed turns
// the jitter off so the schedule is exact; any other seed keeps production's +/-20 %
// jitter with a deterministic random source.
func incidentRetryPolicy(clock *incidentClock, seed uint64) *retryPolicy {
	p := productionRetryPolicy
	p.Now = clock.now
	if seed == 0 {
		p.Jitter = 0
		p.Random = nil
	} else {
		p.Random = rand.New(rand.NewPCG(seed, seed*7919)).Float64
	}
	return &p
}

// backoffBase is the un-jittered production delay after the n-th failure.
func backoffBase(n int32) time.Duration {
	d := productionRetryPolicy.BaseDelay
	for i := int32(1); i < n && d < productionRetryPolicy.MaxDelay; i++ {
		d *= 2
	}
	return min(d, productionRetryPolicy.MaxDelay)
}

// expectJitteredDelay checks a scheduled delay against the production policy: exactly
// the base without jitter, otherwise within +/-20 % of it (plus the one second the next
// attempt time is rounded up by).
func expectJitteredDelay(delay time.Duration, n int32, jitter bool) {
	GinkgoHelper()
	base := backoffBase(n)
	if !jitter {
		Expect(delay).To(Equal(base), "delay after failure %d", n)
		return
	}
	low := time.Duration(float64(base) * (1 - productionRetryPolicy.Jitter))
	high := time.Duration(float64(base)*(1+productionRetryPolicy.Jitter)) + time.Second
	Expect(delay).To(And(BeNumerically(">=", low), BeNumerically("<=", high)), "delay after failure %d", n)
}

// incidentScenario is one way APIM misbehaved, and what the fixed operator must do.
type incidentScenario struct {
	// opDuration is how long an accepted import keeps running in APIM (fake time).
	opDuration time.Duration
	// busyFor makes APIM busy with an earlier import for this long before the replay.
	busyFor time.Duration
	// every3rd422 answers every third import PUT with 422 Management API timed out.
	every3rd422 bool
	// retryAfter is a Retry-After header on every 202 and InProgress poll.
	retryAfter string
	// jitterSeed seeds the production jitter; zero turns it off.
	jitterSeed uint64
	// recoverAfter makes APIM accept imports again after this many; zero never.
	recoverAfter int
	// fullDoc imports the 1.75 MB document instead of incidentSmallDoc.
	fullDoc bool

	wantAnswers  []int
	wantPhase    string
	wantLastErr  string
	stallsWithin time.Duration
}

var _ = Describe("Incident replay 25-28 Sep 2026: APIMAPIDeployment import loop", func() {
	const (
		subscription  = "00000000-0000-0000-0000-0000000000cc"
		resourceGroup = "rg-incident"
	)

	var (
		ctx         context.Context
		key         types.NamespacedName
		name        string
		apiID       string
		apimService string
		docServer   *httptest.Server
		doc         atomic.Value
		arm         *incidentImportARM
		clock       *incidentClock
		reconciler  *APIMAPIDeploymentReconciler
		tee         *incidentLogTee
		cleanups    []client.Object
	)

	newReconciler := func(seed uint64) *APIMAPIDeploymentReconciler {
		return &APIMAPIDeploymentReconciler{
			Client:  k8sClient,
			Scheme:  k8sClient.Scheme(),
			fetcher: testOpenAPIFetcher(),
			getToken: func(context.Context, string, string) (string, error) {
				return "incident-token", nil
			},
			retry: incidentRetryPolicy(clock, seed),
		}
	}

	// reconcileOnce runs one reconcile. An APIM failure must never come back as an
	// error: that would hand the retries to controller-runtime's rate limiter.
	reconcileOnce := func() ctrl.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	getDeployment := func() *apimv1.APIMAPIDeployment {
		GinkgoHelper()
		deployment := &apimv1.APIMAPIDeployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		return deployment
	}

	deploymentStatus := func() (string, apimv1.RetryStatus) {
		GinkgoHelper()
		d := getDeployment()
		return d.Status.Phase, d.Status.RetryStatus
	}

	replay := func() []incidentStep {
		GinkgoHelper()
		return replayIncident(clock, incidentHorizon, incidentNoise, reconcileOnce, arm.importCount, deploymentStatus)
	}

	setRetryAnnotation := func(value string) {
		GinkgoHelper()
		d := getDeployment()
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[retryAnnotation] = value
		Expect(k8sClient.Update(ctx, d)).To(Succeed())
	}

	// sep2026 makes the fake ARM answer the way APIM did in Sep 2026: an accepted import
	// keeps running for 10 minutes, a PUT meanwhile is a 412, every third PUT is a 422.
	sep2026 := func() {
		arm.opDuration = 10 * time.Minute
		arm.every3rd422 = true
	}

	BeforeEach(func() {
		ctx = context.Background()
		n := incidentCounter.Add(1)
		name = fmt.Sprintf("incident-api-%d", n)
		apiID = fmt.Sprintf("incident-api-id-%d", n)
		apimService = fmt.Sprintf("incident-apim-%d", n)
		key = types.NamespacedName{Name: name, Namespace: "default"}
		cleanups = make([]client.Object, 0, 5)

		By("reading back the operator's log lines")
		tee = &incidentLogTee{}
		GinkgoWriter.TeeTo(tee)
		DeferCleanup(GinkgoWriter.ClearTeeWriters)

		By("serving the OpenAPI document and a fake ARM")
		doc.Store(incidentSmallDoc)
		docServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, doc.Load().(string))
		}))
		DeferCleanup(docServer.Close)
		clock = newIncidentClock()
		arm = newIncidentImportARM(clock, subscription, resourceGroup, apimService, apiID)
		DeferCleanup(arm.server.Close)
		DeferCleanup(apim.UseEndpoint(arm.server.URL, arm.server.Client()))

		DeferCleanup(stubAzureIdentityEnv())

		By("creating the APIMService, APIMAPI, APIMAPIDeployment and a ready pod")
		service := &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: apimService, Namespace: "default"},
			Spec:       apimv1.APIMServiceSpec{Name: apimService, Subscription: subscription, ResourceGroup: resourceGroup},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		api := &apimv1.APIMAPI{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: apimv1.APIMAPISpec{
				APIID:                apiID,
				APIMService:          apimService,
				RoutePrefix:          "/incident",
				ServiceURL:           "https://backend.example.net",
				OpenAPIDefinitionURL: docServer.URL,
			},
		}
		Expect(k8sClient.Create(ctx, api)).To(Succeed())
		deployment := &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: apimv1.APIMAPIDeploymentSpec{
				APIID:                apiID,
				APIMService:          apimService,
				APIMAPIName:          name,
				Subscription:         subscription,
				ResourceGroup:        resourceGroup,
				RoutePrefix:          "/incident",
				ServiceURL:           "https://backend.example.net",
				OpenAPIDefinitionURL: docServer.URL,
				SubscriptionRequired: true,
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		rs := createReplicaSet(ctx, name+"-rs", map[string]string{"app.kubernetes.io/name": name}, map[string]string{"app": name})
		createReadyPodForReplicaSet(ctx, rs, name+"-pod")
		cleanups = append(cleanups, deployment, api, service, rs,
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: "default"}})

		reconciler = newReconciler(0)
	})

	AfterEach(func() {
		for _, obj := range cleanups {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	DescribeTable("replaying 69 hours of a failing import",
		func(sc incidentScenario) {
			arm.opDuration = sc.opDuration
			arm.every3rd422 = sc.every3rd422
			arm.retryAfter = sc.retryAfter
			arm.recoverAfter = sc.recoverAfter
			if sc.busyFor > 0 {
				arm.preset(clock.now().Add(sc.busyFor))
			}
			reconciler = newReconciler(sc.jitterSeed)
			jitter := sc.jitterSeed != 0
			if sc.fullDoc {
				doc.Store(incidentDoc)
			}
			docSize := len(doc.Load().(string))

			steps := replay()

			By("sending at most five imports, answered the way APIM answered them")
			answers := arm.importAnswers()
			statuses := make([]int, len(answers))
			for i, a := range answers {
				statuses[i] = a.status
				Expect(a.bytes).To(Equal(docSize), "import %d carries the whole document", i+1)
			}
			Expect(len(answers)).To(BeNumerically("<=", 5))
			Expect(statuses).To(Equal(sc.wantAnswers))
			etagGets, polls, others := arm.counts()
			Expect(etagGets).To(Equal(len(answers)), "the API is read only right before an import, never while held back")
			Expect(arm.overlapCount()).To(BeZero(), "no import may be sent while one APIM accepted is still running")
			if sc.busyFor == 0 {
				Expect(statuses).NotTo(ContainElement(http.StatusPreconditionFailed),
					"a 412 here would mean an import overlapped a running one")
			}
			accepted := 0
			for _, s := range statuses {
				if s == http.StatusAccepted {
					accepted++
				}
			}
			Expect(polls).To(BeNumerically(">=", accepted), "every accepted import is read")

			By("being far below what 0.30.0 sent against the same APIM")
			// Analytic, see oldImportPUTsFloor and oldImportPUTsForScript: at least 1035
			// imports (1.8 GB of OpenAPI document) in 69 hours, 2956 against this script.
			Expect(oldImportPUTsFloor).To(Equal(1035))
			Expect(len(answers) * 200).To(BeNumerically("<", oldImportPUTsFloor))
			Expect(len(answers) * 500).To(BeNumerically("<", oldImportPUTsForScript))

			By("never writing before nextAttemptAt, and requeueing for exactly the time left")
			var notBefore time.Time
			var stalledAt time.Time
			// A failure is recorded by the reconcile that writes, or by the one that learns
			// that an import it was waiting for (status.pendingImport) has failed.
			failedSteps, skipped, held := 0, 0, 0
			previousFailures := int32(0)
			for i, s := range steps {
				Expect(s.realDuration).To(BeNumerically("<", 5*time.Second),
					"reconcile %d: a reconcile never waits for an accepted import", i)
				if s.wrote {
					Expect(s.at.Before(notBefore)).To(BeFalse(), "reconcile %d wrote at %s, before %s", i, s.at, notBefore)
				}
				failed := s.retry.ConsecutiveFailures > previousFailures
				previousFailures = s.retry.ConsecutiveFailures
				switch {
				case failed && s.phase == phaseBackoff:
					failedSteps++
					next, err := time.Parse(time.RFC3339, s.retry.NextAttemptAt)
					Expect(err).NotTo(HaveOccurred())
					Expect(s.result.RequeueAfter).To(Equal(next.Sub(s.after)))
					expectJitteredDelay(next.Sub(s.after), s.retry.ConsecutiveFailures, jitter)
					notBefore = next
				case s.phase == phaseBackoff:
					skipped++
					Expect(s.wrote).To(BeFalse(), "reconcile %d wrote without recording a failure", i)
					next, err := time.Parse(time.RFC3339, s.retry.NextAttemptAt)
					Expect(err).NotTo(HaveOccurred())
					Expect(s.at.Before(next)).To(BeTrue(), "reconcile %d held back although due", i)
					Expect(s.result).To(Equal(ctrl.Result{RequeueAfter: next.Sub(s.at)}))
				case s.phase == phaseStalled:
					Expect(s.result).To(BeZero(), "a Stalled deployment is not requeued")
					if failed {
						stalledAt = s.after
					} else {
						held++
					}
					// Nothing may reach APIM once Stalled.
					notBefore = incidentStart.Add(1000 * time.Hour)
				case s.phase == apimDeploymentPhaseImporting:
					// Waiting for an import APIM is still running: it is read, never written over.
					Expect(s.result.RequeueAfter).To(And(
						BeNumerically(">=", minPendingImportPoll), BeNumerically("<=", maxPendingImportPoll)),
						"reconcile %d waits for the running import", i)
				}
			}

			deployment := getDeployment()
			Expect(deployment.Status.Phase).To(Equal(sc.wantPhase))
			stalled := tee.entries(stalledLogMessage, name)
			if sc.wantPhase == phaseStalled {
				By("ending Stalled, with the reason in the status")
				Expect(deployment.Status.Status).To(Equal(phaseError))
				Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(5)))
				Expect(deployment.Status.NextAttemptAt).To(BeEmpty())
				Expect(deployment.Status.AppliedHash).To(BeEmpty())
				Expect(deployment.Status.Message).To(HavePrefix(
					"Failed to import API into APIM: APIM write stalled after 5 failures in a row"))
				Expect(deployment.Status.Message).To(ContainSubstring(retryAnnotation))
				Expect(deployment.Status.LastError).To(ContainSubstring(sc.wantLastErr))
				Expect(others).To(BeEmpty(), "nothing after the import may run while it fails")
				Expect(stalledAt.IsZero()).To(BeFalse())
				Expect(stalledAt.Sub(incidentStart)).To(BeNumerically("<=", sc.stallsWithin))

				By("logging the stalled line exactly once, with the keys the monitor reads")
				Expect(stalled).To(HaveLen(1))
				Expect(stalled[0]).To(HaveKeyWithValue("kind", "APIMAPIDeployment"))
				Expect(stalled[0]).To(HaveKeyWithValue("namespace", "default"))
				Expect(stalled[0]).To(HaveKeyWithValue("name", name))
				Expect(stalled[0]).To(HaveKeyWithValue("apiID", apiID))
				Expect(stalled[0]).To(HaveKeyWithValue("attempts", float64(5)))
				Expect(stalled[0]).To(HaveKeyWithValue("lastError", deployment.Status.LastError))
				Expect(tee.countContaining("🛑", name)).To(Equal(1), "no other line may carry the stalled marker")
			} else {
				By("recovering without ever stalling")
				Expect(stalled).To(BeEmpty())
				Expect(tee.countContaining("🛑", name)).To(BeZero())
				Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
				Expect(deployment.Status.NextAttemptAt).To(BeEmpty())
				Expect(deployment.Status.AppliedHash).To(Equal(deployment.Status.DesiredHash))
				Expect(others).To(Equal([]string{"service details"}), "no PATCH of the API: the import sets serviceUrl and subscriptionRequired")
				last := 0
				for i, s := range steps {
					if s.wrote {
						last = i
					}
				}
				Expect(steps[last].phase).To(Equal(apimDeploymentPhaseSucceeded))
				Expect(len(steps)-1-last).To(BeNumerically(">", 60), "the in-sync deployment kept being reconciled without writing")
			}

			By("logging every attempt with the shared emoji lines")
			var imports []map[string]any
			for _, kv := range tee.entries(msgWriteStarting, name) {
				if kv["step"] == "import API" {
					imports = append(imports, kv)
				}
			}
			Expect(imports).To(HaveLen(len(answers)))
			for i, kv := range imports {
				Expect(kv).To(HaveKeyWithValue("attempt", fmt.Sprintf("%d/5", i+1)))
				Expect(kv).To(HaveKeyWithValue("bytes", float64(docSize)))
			}
			failedLines := tee.entries(msgWriteFailed, name)
			Expect(failedLines).To(HaveLen(failedSteps))
			for i, kv := range failedLines {
				Expect(kv).To(HaveKeyWithValue("class", string(errorClassTransient)))
				Expect(kv).To(HaveKeyWithValue("attempt", fmt.Sprintf("%d/5", i+1)))
				Expect(kv).To(HaveKey("nextAttemptAt"))
			}
			Expect(skipped).To(BeNumerically(">", 0), "the noise must have hit the backoff windows")
			Expect(tee.entries(msgWriteBackingOff, name)).To(HaveLen(skipped))
			Expect(tee.entries(msgWriteHeld, name)).To(HaveLen(held))
			Expect(tee.entries(msgWriteRejected, name)).To(BeEmpty(), "nothing in the incident was permanent")
			if sc.wantPhase == phaseStalled {
				Expect(held).To(BeNumerically(">=", 60), "the stall held through the 60 s noise and the hourly events after")
				Expect(tee.entries(msgWriteSucceeded, name)).To(BeEmpty())
			} else {
				Expect(tee.entries(msgWriteSucceeded, name)).To(HaveLen(1))
			}
		},
		// Before status.pendingImport every import after the first overlapped a running one
		// and drew a 412; now each 202 is recorded at once and followed on later reconciles,
		// and the next import waits for the one before it to end.
		Entry("the Sep 2026 mix: 1.75 MB import, 202 beyond the wait, 412 on overlap, 422 timeouts", incidentScenario{
			opDuration: 10 * time.Minute, every3rd422: true, fullDoc: true,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 80 * time.Minute,
		}),
		Entry("the Sep 2026 mix with production jitter, seed 1", incidentScenario{
			opDuration: 10 * time.Minute, every3rd422: true, jitterSeed: 1,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 80 * time.Minute,
		}),
		Entry("the Sep 2026 mix with production jitter, seed 42", incidentScenario{
			opDuration: 10 * time.Minute, every3rd422: true, jitterSeed: 42,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 80 * time.Minute,
		}),
		Entry("the Sep 2026 mix with production jitter, seed 2026", incidentScenario{
			opDuration: 10 * time.Minute, every3rd422: true, jitterSeed: 2026,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 80 * time.Minute,
		}),
		Entry("imports outliving the wait, without 422s", incidentScenario{
			opDuration: 10 * time.Minute,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusAccepted,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 80 * time.Minute,
		}),
		Entry("every accepted import dying with DeadOperationMonitor", incidentScenario{
			opDuration: 4 * time.Minute,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusAccepted,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 40 * time.Minute,
		}),
		Entry("DeadOperationMonitor mixed with 422 timeouts", incidentScenario{
			opDuration: 4 * time.Minute, every3rd422: true,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 40 * time.Minute,
		}),
		Entry("an import from before the restart still running: 412 only", incidentScenario{
			busyFor: incidentHorizon + time.Hour,
			wantAnswers: []int{http.StatusPreconditionFailed, http.StatusPreconditionFailed, http.StatusPreconditionFailed,
				http.StatusPreconditionFailed, http.StatusPreconditionFailed},
			wantPhase: phaseStalled, wantLastErr: "PreconditionFailed", stallsWithin: 15 * time.Minute,
		}),
		Entry("Retry-After: 60 on every 202 and reading, followed for the first reading", incidentScenario{
			opDuration: 10 * time.Minute, every3rd422: true, retryAfter: "60",
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusAccepted, http.StatusAccepted},
			wantPhase: phaseStalled, wantLastErr: "DeadOperationMonitor", stallsWithin: 80 * time.Minute,
		}),
		Entry("APIM recovering after the third import", incidentScenario{
			opDuration: 10 * time.Minute, every3rd422: true, recoverAfter: 3,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusCreated},
			wantPhase: apimDeploymentPhaseSucceeded,
		}),
		Entry("APIM recovering after the fourth import, the last one before Stalled", incidentScenario{
			opDuration: 10 * time.Minute, every3rd422: true, recoverAfter: 4,
			wantAnswers: []int{http.StatusAccepted, http.StatusAccepted, http.StatusUnprocessableEntity,
				http.StatusAccepted, http.StatusCreated},
			wantPhase: apimDeploymentPhaseSucceeded,
		}),
	)

	// failOnce reconciles until one more failure is recorded, moving the clock on while an
	// accepted import is still running in APIM, as the replay's noise would.
	failOnce := func() {
		GinkgoHelper()
		want := getDeployment().Status.ConsecutiveFailures + 1
		for range 100 {
			reconcileOnce()
			deployment := getDeployment()
			if deployment.Status.ConsecutiveFailures >= want {
				return
			}
			Expect(deployment.Status.PendingImport).NotTo(BeNil(), "neither failed nor waiting for an import")
			clock.advance(time.Minute)
		}
		Fail("no failure was recorded")
	}

	It("keeps the stall across an operator restart and writes no status while held", func() {
		sep2026()

		By("failing twice, then restarting the operator in the middle of the backoff")
		failOnce()
		clock.set(mustParseRFC3339(getDeployment().Status.NextAttemptAt))
		failOnce()
		Expect(arm.importCount()).To(Equal(2))
		reconciler = newReconciler(0)
		clock.advance(30 * time.Second)
		next := mustParseRFC3339(getDeployment().Status.NextAttemptAt)
		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: next.Sub(clock.now())}))
		Expect(arm.importCount()).To(Equal(2), "a fresh process must honour the persisted nextAttemptAt")

		By("running out the rest of the 69 hours on the new process")
		replay()
		Expect(arm.importCount()).To(Equal(5))
		Expect(getDeployment().Status.Phase).To(Equal(phaseStalled))
		Expect(tee.entries(stalledLogMessage, name)).To(HaveLen(1))

		By("restarting once more while Stalled")
		reconciler = newReconciler(0)
		before := getDeployment().ResourceVersion
		for range 3 {
			clock.advance(10 * time.Hour) // controller-runtime's default resync period
			Expect(reconcileOnce()).To(BeZero())
		}
		Expect(arm.importCount()).To(Equal(5))
		Expect(getDeployment().ResourceVersion).To(Equal(before), "a held reconcile must not write the status")
		Expect(tee.entries(stalledLogMessage, name)).To(HaveLen(1), "a restart must not re-announce the stall")
	})

	It("imports once more on the retry annotation once APIM has recovered, and only once per value", func() {
		sep2026()
		replay()
		Expect(arm.importCount()).To(Equal(5))
		Expect(getDeployment().Status.Phase).To(Equal(phaseStalled))

		By("APIM recovering, and someone setting the retry annotation the morning after")
		arm.recoverAfter = 5
		clock.set(incidentStart.Add(incidentHorizon))
		setRetryAnnotation("2026-09-28T07:00:00Z")
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.importCount()).To(Equal(6))
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(deployment.Status.AppliedHash).To(Equal(deployment.Status.DesiredHash))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
		Expect(deployment.Status.LastRetryAnnotation).To(Equal("2026-09-28T07:00:00Z"))
		Expect(tee.entries(msgWriteReset, name)).To(HaveLen(1))
		Expect(tee.entries(msgWriteSucceeded, name)).To(HaveLen(1))

		By("reconciling for another 69 hours with the same annotation value")
		replay()
		Expect(arm.importCount()).To(Equal(6), "an applied hash and an already handled annotation write nothing")
		Expect(tee.entries(stalledLogMessage, name)).To(HaveLen(1))
	})

	It("stalls again within five more imports when the retry annotation meets a still-broken APIM", func() {
		sep2026()
		replay()
		Expect(arm.importCount()).To(Equal(5))

		By("setting the retry annotation while APIM is still overloaded")
		clock.set(incidentStart.Add(incidentHorizon))
		setRetryAnnotation("too-early")
		replay()

		Expect(arm.importCount()).To(Equal(10), "one fresh set of five, not another loop")
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseStalled))
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(5)))
		Expect(deployment.Status.LastRetryAnnotation).To(Equal("too-early"))
		stalled := tee.entries(stalledLogMessage, name)
		Expect(stalled).To(HaveLen(2), "one stalled line per stall, so the monitor fires again")
		for _, kv := range stalled {
			Expect(kv).To(HaveKeyWithValue("attempts", float64(5)))
		}
	})

	It("starts over on a new OpenAPI document, the fix a team would ship", func() {
		sep2026()
		replay()
		stalledHash := getDeployment().Status.DesiredHash

		By("publishing a smaller document while APIM has recovered")
		arm.recoverAfter = 5
		doc.Store(`{"openapi":"3.0.0","info":{"title":"incident","version":"1.0.1"},"paths":{}}`)
		clock.set(incidentStart.Add(incidentHorizon))
		Expect(reconcileOnce()).To(BeZero())

		Expect(arm.importCount()).To(Equal(6))
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(deployment.Status.DesiredHash).NotTo(Equal(stalledHash))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
		Expect(tee.entries(msgWriteReset, name)).To(HaveLen(1))
		Expect(tee.entries(msgWriteReset, name)[0]).To(HaveKeyWithValue("reason", "spec changed"))
	})

	It("does not fetch a token or touch APIM in any held reconcile", func() {
		sep2026()
		var tokens atomic.Int32
		reconciler.getToken = func(context.Context, string, string) (string, error) {
			tokens.Add(1)
			return "incident-token", nil
		}

		steps := replay()

		wrote, read := 0, 0
		previousFailures := int32(0)
		for _, s := range steps {
			failed := s.retry.ConsecutiveFailures > previousFailures
			previousFailures = s.retry.ConsecutiveFailures
			switch {
			case s.wrote:
				wrote++
			case s.phase == apimDeploymentPhaseImporting, failed:
				// Read the import APIM was still running, or learned that it failed.
				read++
			}
		}
		Expect(wrote).To(Equal(5))
		Expect(read).To(BeNumerically(">", 0))
		Expect(len(steps)).To(BeNumerically(">", 100), "the replay reconciled far more often than it touched APIM")
		Expect(tokens.Load()).To(Equal(int32(wrote+read)), "one token per write or reading, none for a held reconcile")
	})
})

// incidentKind is one of the other three kinds that write to APIM.
type incidentKind string

const (
	incidentProduct incidentKind = "APIMProduct"
	incidentTag     incidentKind = "APIMTag"
	incidentPolicy  incidentKind = "APIMInboundPolicy"
)

var _ = Describe("Incident replay 25-28 Sep 2026: product, tag and policy writes", func() {
	var (
		ctx         context.Context
		key         types.NamespacedName
		apimService string
		writeID     string
		arm         *incidentWriteARM
		clock       *incidentClock
		logLines    func() []logLine
		reconciler  reconcile.Reconciler
	)

	token := func(context.Context, string, string) (string, error) { return "incident-token", nil }

	newReconciler := func(kind incidentKind) reconcile.Reconciler {
		policy := incidentRetryPolicy(clock, 7)
		switch kind {
		case incidentProduct:
			return &APIMProductReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: policy}
		case incidentTag:
			return &APIMTagReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: policy}
		default:
			return &APIMInboundPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: policy}
		}
	}

	create := func(kind incidentKind, deletionPolicy apimv1.DeletionPolicy) {
		GinkgoHelper()
		meta := metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}
		switch kind {
		case incidentProduct:
			Expect(k8sClient.Create(ctx, &apimv1.APIMProduct{ObjectMeta: meta, Spec: apimv1.APIMProductSpec{
				APIMService: apimService, ProductID: writeID, DisplayName: "Incident product", DeletionPolicy: deletionPolicy,
			}})).To(Succeed())
		case incidentTag:
			Expect(k8sClient.Create(ctx, &apimv1.APIMTag{ObjectMeta: meta, Spec: apimv1.APIMTagSpec{
				APIMService: apimService, TagID: writeID, DisplayName: "Incident tag",
			}})).To(Succeed())
		default:
			Expect(k8sClient.Create(ctx, &apimv1.APIMInboundPolicy{ObjectMeta: meta, Spec: apimv1.APIMInboundPolicySpec{
				APIMService: apimService, APIID: writeID,
				PolicyContent: `<policies><inbound><base /></inbound><backend><base /></backend><outbound><base /></outbound></policies>`,
			}})).To(Succeed())
		}
	}

	// statusOf reads the phase, retry state, generation and observed generation back.
	statusOf := func(kind incidentKind) (string, apimv1.RetryStatus, int64, int64) {
		GinkgoHelper()
		switch kind {
		case incidentProduct:
			o := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, key, o)).To(Succeed())
			return o.Status.Phase, o.Status.RetryStatus, o.Generation, o.Status.ObservedGeneration
		case incidentTag:
			o := &apimv1.APIMTag{}
			Expect(k8sClient.Get(ctx, key, o)).To(Succeed())
			return o.Status.Phase, o.Status.RetryStatus, o.Generation, o.Status.ObservedGeneration
		default:
			o := &apimv1.APIMInboundPolicy{}
			Expect(k8sClient.Get(ctx, key, o)).To(Succeed())
			return o.Status.Phase, o.Status.RetryStatus, o.Generation, o.Status.ObservedGeneration
		}
	}

	reconcileOnce := func() ctrl.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	linesWith := func(msg string) []logLine {
		var out []logLine
		for _, l := range logLines() {
			if l.msg == msg {
				out = append(out, l)
			}
		}
		return out
	}

	setup := func(failMethod string) {
		n := incidentCounter.Add(1)
		key = types.NamespacedName{Name: fmt.Sprintf("incident-write-%d", n), Namespace: "default"}
		apimService = fmt.Sprintf("incident-write-apim-%d", n)
		writeID = fmt.Sprintf("incident-id-%d", n)

		var logger logr.Logger
		logger, logLines = captureLogs()
		ctx = log.IntoContext(context.Background(), logger)
		clock = newIncidentClock()
		arm = newIncidentWriteARM(failMethod)
		DeferCleanup(arm.server.Close)
		DeferCleanup(apim.UseEndpoint(arm.server.URL, arm.server.Client()))
		DeferCleanup(stubAzureIdentityEnv())

		service := &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: apimService, Namespace: "default"},
			Spec: apimv1.APIMServiceSpec{Name: apimService, Subscription: "00000000-0000-0000-0000-0000000000dd",
				ResourceGroup: "rg-incident"},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), service))).To(Succeed()) })
	}

	DescribeTable("replaying 69 hours of transient write failures",
		func(kind incidentKind, idKey string) {
			setup(http.MethodPut)
			create(kind, "")
			DeferCleanup(func() {
				if kind == incidentProduct {
					forceDeleteProduct(context.Background(), key)
					return
				}
				var obj client.Object = &apimv1.APIMTag{}
				if kind == incidentPolicy {
					obj = &apimv1.APIMInboundPolicy{}
				}
				obj.SetName(key.Name)
				obj.SetNamespace(key.Namespace)
				Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj))).To(Succeed())
			})
			reconciler = newReconciler(kind)

			steps := replayIncident(clock, incidentHorizon, incidentNoise, reconcileOnce, arm.writeCount,
				func() (string, apimv1.RetryStatus) {
					phase, retry, _, _ := statusOf(kind)
					return phase, retry
				})

			By("writing at most five times, through the incident's mix of transient answers")
			writes, statuses := arm.answered()
			Expect(len(writes)).To(BeNumerically("<=", 5))
			Expect(statuses).To(Equal([]int{http.StatusPreconditionFailed, http.StatusUnprocessableEntity,
				http.StatusConflict, http.StatusTooManyRequests, http.StatusInternalServerError}))
			Expect(len(writes) * 200).To(BeNumerically("<", oldImportPUTsFloor))

			By("never writing early and requeueing for the time left")
			var notBefore time.Time
			for i, s := range steps {
				if s.wrote {
					Expect(s.at.Before(notBefore)).To(BeFalse(), "write %d at %s, before %s", i, s.at, notBefore)
				}
				switch s.phase {
				case phaseBackoff:
					next := mustParseRFC3339(s.retry.NextAttemptAt)
					Expect(s.result).To(Equal(ctrl.Result{RequeueAfter: next.Sub(s.after)}))
					if s.wrote {
						expectJitteredDelay(next.Sub(s.after), s.retry.ConsecutiveFailures, true)
						notBefore = next
					}
				case phaseStalled:
					Expect(s.result).To(BeZero())
					notBefore = incidentStart.Add(1000 * time.Hour)
				default:
					Fail(fmt.Sprintf("reconcile %d left phase %q", i, s.phase))
				}
			}

			By("ending Stalled with the spec version the failures belong to")
			phase, retry, generation, observed := statusOf(kind)
			Expect(phase).To(Equal(phaseStalled))
			Expect(retry.ConsecutiveFailures).To(Equal(int32(5)))
			Expect(retry.NextAttemptAt).To(BeEmpty())
			Expect(observed).To(Equal(generation))

			By("logging the stalled line exactly once")
			stalled := linesWith(stalledLogMessage)
			Expect(stalled).To(HaveLen(1))
			Expect(stalled[0].err).To(BeTrue(), "the stalled line is logged at error level")
			Expect(stalled[0].kv).To(HaveKeyWithValue("kind", string(kind)))
			Expect(stalled[0].kv).To(HaveKeyWithValue("namespace", "default"))
			Expect(stalled[0].kv).To(HaveKeyWithValue("name", key.Name))
			Expect(stalled[0].kv).To(HaveKeyWithValue(idKey, writeID))
			Expect(stalled[0].kv).To(HaveKeyWithValue("attempts", int32(5)))
			Expect(stalled[0].kv["lastError"]).To(And(ContainSubstring("500"), ContainSubstring("InternalServerError")))
			Expect(linesWith(msgWriteStarting)).To(HaveLen(5))
			Expect(linesWith(msgWriteFailed)).To(HaveLen(4))
			Expect(linesWith(msgWriteRejected)).To(BeEmpty())
			Expect(linesWith(msgWriteBackingOff)).NotTo(BeEmpty())
			Expect(len(linesWith(msgWriteHeld))).To(BeNumerically(">=", 60))
		},
		Entry("APIMProduct upsert", incidentProduct, "productID"),
		Entry("APIMTag upsert", incidentTag, "tagID"),
		Entry("APIMInboundPolicy upsert", incidentPolicy, "apiID"),
	)

	It("holds a product's finalizer after five failed deletes instead of looping", func() {
		setup(http.MethodDelete)
		create(incidentProduct, apimv1.DeletionPolicyDelete)
		DeferCleanup(func() { forceDeleteProduct(context.Background(), key) })
		reconciler = newReconciler(incidentProduct)

		By("creating the product in APIM")
		Expect(reconcileOnce()).To(BeZero())
		phase, _, _, _ := statusOf(incidentProduct)
		Expect(phase).To(Equal(phaseCreated))

		By("deleting the resource while APIM fails every delete")
		product := &apimv1.APIMProduct{}
		Expect(k8sClient.Get(ctx, key, product)).To(Succeed())
		Expect(k8sClient.Delete(ctx, product)).To(Succeed())
		replayIncident(clock, incidentHorizon, incidentNoise, reconcileOnce, arm.writeCount,
			func() (string, apimv1.RetryStatus) {
				phase, retry, _, _ := statusOf(incidentProduct)
				return phase, retry
			})

		writes, _ := arm.answered()
		Expect(writes).To(HaveLen(5))
		for _, w := range writes {
			Expect(w).To(HavePrefix(http.MethodDelete + " "))
		}
		Expect(k8sClient.Get(ctx, key, product)).To(Succeed())
		Expect(product.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
		Expect(product.Status.Phase).To(Equal(phaseStalled))
		Expect(product.Status.Message).To(ContainSubstring(string(apimv1.DeletionPolicyRetain)))
		stalled := linesWith(stalledLogMessage)
		Expect(stalled).To(HaveLen(1))
		Expect(stalled[0].kv).To(HaveKeyWithValue("operation", "delete"))
		Expect(stalled[0].kv).To(HaveKeyWithValue("productID", writeID))
	})
})

// mustParseRFC3339 parses a status time.
func mustParseRFC3339(s string) time.Time {
	GinkgoHelper()
	t, err := time.Parse(time.RFC3339, s)
	Expect(err).NotTo(HaveOccurred())
	return t
}

// incidentErrors are the failures seen during the incident, in the shapes the apim
// package returns them, plus the transport errors a loaded APIM produces.
var incidentErrors = []error{
	&apim.Error{Operation: "import API", Method: http.MethodGet, Message: "operation still running after 5m0s",
		Err: apim.ErrImportWaitTimeout},
	&apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusPreconditionFailed,
		Code: "PreconditionFailed"},
	&apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusUnprocessableEntity,
		Code: "ManagementApiRequestFailed", DetailCode: "Timeout", Message: "Management API request timed out."},
	&apim.Error{Operation: "import API", Method: http.MethodGet, Code: "InternalServerError",
		Message: "operation status Failed: DeadOperationMonitor", Err: apim.ErrAsyncOperationFailed},
	&apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusConflict, Code: "Conflict"},
	&apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusTooManyRequests},
	&apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusServiceUnavailable},
	&apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusBadRequest,
		Code: "PreconditionFailed"},
	fmt.Errorf("import API: %w", &url.Error{Op: "Put", URL: "https://management.azure.com", Err: errors.New("connection reset by peer")}),
	fmt.Errorf("import API: %w", context.DeadlineExceeded),
}

// incidentCountingSink counts log lines by message.
type incidentCountingSink struct{ counts map[string]int }

func newIncidentCountingSink() *incidentCountingSink {
	return &incidentCountingSink{counts: map[string]int{}}
}

func (s *incidentCountingSink) Init(logr.RuntimeInfo)               {}
func (s *incidentCountingSink) Enabled(int) bool                    { return true }
func (s *incidentCountingSink) Info(_ int, msg string, _ ...any)    { s.counts[msg]++ }
func (s *incidentCountingSink) Error(_ error, msg string, _ ...any) { s.counts[msg]++ }
func (s *incidentCountingSink) WithValues(...any) logr.LogSink      { return s }
func (s *incidentCountingSink) WithName(string) logr.LogSink        { return s }
func (s *incidentCountingSink) count(msg string) int                { return s.counts[msg] }

// TestIncidentErrorsAreTransient checks that every failure seen in the incident is
// retried rather than given up on at once: Stalled, not Invalid, is the right end state.
func TestIncidentErrorsAreTransient(t *testing.T) {
	for _, err := range incidentErrors {
		if got := classifyAPIMError(err); got != errorClassTransient {
			t.Errorf("classifyAPIMError(%v) = %s, want transient", err, got)
		}
	}
}

// TestIncidentReplayAcrossJitterSeeds plays the retry state machine alone, without
// envtest, through the full 69 hours with a reconcile every 60 s (0.30.0's requeue
// cadence, as the worst case of outside events) for many jitter seeds: every seed must
// write exactly five times, stall within the jittered 1+2+4+8 minutes, and log the
// stalled line once. 0.30.0 would have written once a minute: 4140 times.
func TestIncidentReplayAcrossJitterSeeds(t *testing.T) {
	const tick = time.Minute
	oldWrites := int(incidentHorizon / tick)
	if oldWrites != 4140 {
		t.Fatalf("old write count = %d, want 4140", oldWrites)
	}
	for seed := uint64(1); seed <= 64; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			clock := newIncidentClock()
			p := incidentRetryPolicy(clock, seed)
			sink := newIncidentCountingSink()
			logger := logr.New(sink)
			tag := &apimv1.APIMTag{ObjectMeta: metav1.ObjectMeta{Name: "incident", Namespace: "default"}}
			st := &tag.Status.RetryStatus

			writes, phase := 0, ""
			var stalledAt time.Time
			end := incidentStart.Add(incidentHorizon)
			for clock.now().Before(end) {
				w := p.begin(logger, "APIMAPIDeployment", tag, "apiID", "incident")
				if proceed, _ := w.gate(*st, false); proceed {
					err := incidentErrors[writes%len(incidentErrors)]
					writes++
					w.starting()
					out := w.failed(st, err)
					phase = out.Phase
					if phase == phaseStalled {
						stalledAt = clock.now()
					}
				}
				clock.advance(tick)
			}

			if writes != 5 || phase != phaseStalled {
				t.Fatalf("writes = %d, phase = %q; want 5 writes and Stalled", writes, phase)
			}
			// 1+2+4+8 = 15 minutes of backoff, +20 % jitter, each rounded up to the next
			// second and then to the next tick.
			if elapsed := stalledAt.Sub(incidentStart); elapsed > 18*time.Minute+4*tick || elapsed < 12*time.Minute {
				t.Errorf("stalled after %s; want within the jittered 15 minutes", elapsed)
			}
			if n := sink.count(stalledLogMessage); n != 1 {
				t.Errorf("stalled lines = %d, want exactly 1", n)
			}
			if n := sink.count(msgWriteFailed); n != 4 {
				t.Errorf("failed lines = %d, want 4", n)
			}
			if n := sink.count(msgWriteStarting); n != 5 {
				t.Errorf("starting lines = %d, want 5", n)
			}
			if n := sink.count(msgWriteRejected); n != 0 {
				t.Errorf("rejected lines = %d, want 0", n)
			}
			held := sink.count(msgWriteHeld)
			skipped := sink.count(msgWriteBackingOff)
			if held+skipped+writes != oldWrites {
				t.Errorf("held %d + skipped %d + writes %d != %d reconciles", held, skipped, writes, oldWrites)
			}
			if writes*500 >= oldWrites {
				t.Errorf("writes = %d, not three orders of magnitude below 0.30.0's %d", writes, oldWrites)
			}
		})
	}
}

// TestIncidentAnalyticOldCounts pins the arithmetic the envtest replay compares against,
// so a change to the constants has to be argued again.
func TestIncidentAnalyticOldCounts(t *testing.T) {
	if oldImportPUTsFloor != 1035 {
		t.Errorf("oldImportPUTsFloor = %d, want 69 h / (3 min + 60 s) = 1035", oldImportPUTsFloor)
	}
	low := int(incidentHorizon/time.Minute) * 7 / 10
	high := int(incidentHorizon/time.Minute) * 8 / 11
	if oldImportPUTsForScript < low || oldImportPUTsForScript > high {
		t.Errorf("oldImportPUTsForScript = %d, outside the analytic bounds [%d, %d]", oldImportPUTsForScript, low, high)
	}
}
