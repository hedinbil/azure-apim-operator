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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs run the APIMAPIDeployment write path end to end against a fake ARM
// (httptest behind apim.UseEndpoint), an injected token and a fake clock, and check
// that every APIM call goes through the shared retry handling: Backoff with a
// nextAttemptAt, no APIM call before it, Stalled after five transient failures, Invalid
// at once on a permanent error, and a reset on a spec change or the retry annotation.

// ARM steps of one deployment reconcile, as deploymentFakeARM names them.
const (
	armStepGetAPI               = "get API"
	armStepImport               = "import"
	armStepWebSocket            = "websocket upsert"
	armStepServiceURL           = "serviceUrl"
	armStepSubscriptionRequired = "subscriptionRequired"
	armStepProduct              = "product"
	armStepTag                  = "tag"
	armStepServiceDetails       = "service details"
	armStepPoll                 = "poll"
)

// armResponder answers one request in place of the fake's default success.
type armResponder func(w http.ResponseWriter, r *http.Request)

// deploymentFakeARM stands in for the Azure Management API of one APIM service. Every
// request is recorded by step; a step answers with success unless an override is set.
type deploymentFakeARM struct {
	server      *httptest.Server
	servicePath string
	apiID       string

	mu        sync.Mutex
	steps     []string
	overrides map[string]armResponder
	// onImport runs inside the import (or websocket) PUT, before it is answered, so a
	// spec can look at the status the controller persisted before calling APIM.
	onImport func()
	// auth is the Authorization header of the last request.
	auth string
}

func newDeploymentFakeARM(subscription, resourceGroup, service, apiID string) *deploymentFakeARM {
	f := &deploymentFakeARM{
		servicePath: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s",
			subscription, resourceGroup, service),
		apiID:     apiID,
		overrides: map[string]armResponder{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *deploymentFakeARM) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	step := f.classify(r, body)

	f.mu.Lock()
	f.steps = append(f.steps, step)
	f.auth = r.Header.Get("Authorization")
	override := f.overrides[step]
	onImport := f.onImport
	f.mu.Unlock()

	if (step == armStepImport || step == armStepWebSocket) && onImport != nil {
		onImport()
	}
	if override != nil {
		override(w, r)
		return
	}

	switch step {
	case armStepGetAPI:
		writeARMError(w, http.StatusNotFound, "ResourceNotFound", "API not found")
	case armStepImport, armStepWebSocket:
		writeARMJSON(w, http.StatusCreated, `{"name":"`+f.apiID+`"}`)
	case armStepServiceDetails:
		writeARMJSON(w, http.StatusOK, `{"properties":{"hostnameConfigurations":[`+
			`{"type":"Proxy","hostName":"gw.example.net"},{"type":"DeveloperPortal","hostName":"portal.example.net"}]}}`)
	case armStepPoll:
		writeARMJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
	case "unknown":
		writeARMError(w, http.StatusTeapot, "NotRoutedByFake", r.Method+" "+r.URL.Path)
	default:
		writeARMJSON(w, http.StatusOK, `{}`)
	}
}

// classify names the step a request belongs to.
func (f *deploymentFakeARM) classify(r *http.Request, body []byte) string {
	apiPath := f.servicePath + "/apis/" + f.apiID
	switch {
	case strings.HasPrefix(r.URL.Path, "/asyncops/"):
		return armStepPoll
	case r.URL.Path == f.servicePath && r.Method == http.MethodGet:
		return armStepServiceDetails
	case r.URL.Path == apiPath && r.Method == http.MethodGet:
		return armStepGetAPI
	case r.URL.Path == apiPath && r.Method == http.MethodPut && isImportEnvelope(body):
		return armStepImport
	case r.URL.Path == apiPath && r.Method == http.MethodPut:
		return armStepWebSocket
	case r.URL.Path == apiPath && r.Method == http.MethodPatch && strings.Contains(string(body), "serviceUrl"):
		return armStepServiceURL
	case r.URL.Path == apiPath && r.Method == http.MethodPatch && strings.Contains(string(body), "subscriptionRequired"):
		return armStepSubscriptionRequired
	case strings.HasPrefix(r.URL.Path, f.servicePath+"/products/") && strings.HasSuffix(r.URL.Path, "/apis/"+f.apiID) &&
		r.Method == http.MethodPut:
		return armStepProduct
	case strings.HasPrefix(r.URL.Path, apiPath+"/tags/") && r.Method == http.MethodPut:
		return armStepTag
	}
	return "unknown"
}

// set makes step answer with responder; nil restores the default success.
func (f *deploymentFakeARM) set(step string, responder armResponder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if responder == nil {
		delete(f.overrides, step)
		return
	}
	f.overrides[step] = responder
}

func (f *deploymentFakeARM) setOnImport(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onImport = hook
}

// count is the number of requests so far.
func (f *deploymentFakeARM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.steps)
}

// stepsSince lists the steps requested after the first n requests.
func (f *deploymentFakeARM) stepsSince(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.steps[n:]...)
}

func (f *deploymentFakeARM) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth
}

func writeARMJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeARMError(w http.ResponseWriter, status int, code, message string) {
	writeARMJSON(w, status, fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message))
}

// armFails answers with an ARM error body.
func armFails(status int, code string) armResponder {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeARMError(w, status, code, "injected by the test")
	}
}

// armAccepted answers 202 with a relative Azure-AsyncOperation URL, which the poll
// then resolves against the fake.
func armAccepted() armResponder {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Azure-AsyncOperation", "/asyncops/op-1?api-version=2021-08-01")
		w.WriteHeader(http.StatusAccepted)
	}
}

// deploymentTestClock is a settable clock for the retry policy.
type deploymentTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *deploymentTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *deploymentTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// setTo moves the clock to an RFC3339 time, e.g. status.nextAttemptAt.
func (c *deploymentTestClock) setTo(rfc3339 string) {
	t, err := time.Parse(time.RFC3339, rfc3339)
	Expect(err).NotTo(HaveOccurred())
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// deploymentRetryCounter keeps the resource names of every spec unique, so nothing
// left over from one spec (a terminating pod, a ReplicaSet) can match the next.
var deploymentRetryCounter atomic.Int32

var _ = Describe("APIMAPIDeployment APIM write retries", func() {
	const (
		subscription  = "00000000-0000-0000-0000-0000000000aa"
		resourceGroup = "rg-retry"
		apimService   = "deployment-retry-apim-service"
		firstDoc      = `{"openapi":"3.0.0","info":{"title":"retry","version":"1.0.0"},"paths":{}}`
	)

	var (
		ctx        context.Context
		key        types.NamespacedName
		apiID      string
		arm        *deploymentFakeARM
		openAPIDoc atomic.Value
		docServer  *httptest.Server
		clock      *deploymentTestClock
		reconciler *APIMAPIDeploymentReconciler
		cleanups   []client.Object
	)

	// reconcileOnce runs one reconcile and fails the spec on an error: every APIM
	// failure must come back as a result, never as an error.
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

	// updateDeployment re-reads the deployment, applies mutate and writes it back.
	updateDeployment := func(mutate func(*apimv1.APIMAPIDeployment)) {
		GinkgoHelper()
		deployment := getDeployment()
		mutate(deployment)
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
	}

	setRetryAnnotation := func(value string) {
		GinkgoHelper()
		updateDeployment(func(d *apimv1.APIMAPIDeployment) {
			if d.Annotations == nil {
				d.Annotations = map[string]string{}
			}
			d.Annotations[retryAnnotation] = value
		})
	}

	// expectNoAPIMCall reconciles and checks that neither APIM nor the status was
	// touched, and returns the result.
	expectNoAPIMCall := func() ctrl.Result {
		GinkgoHelper()
		before := getDeployment()
		calls := arm.count()
		result := reconcileOnce()
		Expect(arm.stepsSince(calls)).To(BeEmpty(), "no APIM request may be sent")
		after := getDeployment()
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "the status must not be written")
		return result
	}

	// failUntilStalled lets the import fail transiently until the deployment is Stalled,
	// moving the clock to each nextAttemptAt in between.
	failUntilStalled := func() {
		GinkgoHelper()
		arm.set(armStepImport, armFails(http.StatusPreconditionFailed, "PreconditionFailed"))
		for i := 0; i < 5; i++ {
			if next := getDeployment().Status.NextAttemptAt; next != "" {
				clock.setTo(next)
			}
			reconcileOnce()
		}
		Expect(getDeployment().Status.Phase).To(Equal(phaseStalled))
	}

	BeforeEach(func() {
		ctx = context.Background()
		n := deploymentRetryCounter.Add(1)
		name := fmt.Sprintf("retry-api-%d", n)
		key = types.NamespacedName{Name: name, Namespace: "default"}
		apiID = fmt.Sprintf("retry-api-id-%d", n)
		cleanups = make([]client.Object, 0, 4)

		By("serving the OpenAPI document and a fake ARM")
		openAPIDoc.Store(firstDoc)
		docServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, openAPIDoc.Load().(string))
		}))
		DeferCleanup(docServer.Close)
		arm = newDeploymentFakeARM(subscription, resourceGroup, apimService, apiID)
		DeferCleanup(arm.server.Close)
		DeferCleanup(apim.UseEndpoint(arm.server.URL, arm.server.Client()))

		By("setting placeholder workload identity variables")
		restoreEnv := unsetAzureIdentityEnvVars()
		DeferCleanup(restoreEnv)
		Expect(os.Setenv("AZURE_CLIENT_ID", "test-client-id")).To(Succeed())
		Expect(os.Setenv("AZURE_TENANT_ID", "test-tenant-id")).To(Succeed())

		By("creating the APIMService, APIMAPI, APIMAPIDeployment and a ready pod")
		service := &apimv1.APIMService{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: apimService, Namespace: "default"}, service)
		if apierrors.IsNotFound(err) {
			service = &apimv1.APIMService{
				ObjectMeta: metav1.ObjectMeta{Name: apimService, Namespace: "default"},
				Spec: apimv1.APIMServiceSpec{
					Name:          apimService,
					Subscription:  subscription,
					ResourceGroup: resourceGroup,
				},
			}
			Expect(k8sClient.Create(ctx, service)).To(Succeed())
		} else {
			Expect(err).NotTo(HaveOccurred())
		}

		api := &apimv1.APIMAPI{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: apimv1.APIMAPISpec{
				APIID:                apiID,
				APIMService:          apimService,
				RoutePrefix:          "/retry",
				ServiceURL:           "https://backend.example.net",
				OpenAPIDefinitionURL: docServer.URL,
			},
		}
		Expect(k8sClient.Create(ctx, api)).To(Succeed())
		cleanups = append(cleanups, api)

		deployment := &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: apimv1.APIMAPIDeploymentSpec{
				APIID:                apiID,
				APIMService:          apimService,
				APIMAPIName:          name,
				Subscription:         subscription,
				ResourceGroup:        resourceGroup,
				RoutePrefix:          "/retry",
				ServiceURL:           "https://backend.example.net",
				OpenAPIDefinitionURL: docServer.URL,
				SubscriptionRequired: true,
				ProductIDs:           []string{"retry-product"},
				TagIDs:               []string{"retry-tag"},
			},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		cleanups = append(cleanups, deployment)

		rs := createReplicaSet(ctx, name+"-rs", map[string]string{"app.kubernetes.io/name": name}, map[string]string{"app": name})
		createReadyPodForReplicaSet(ctx, rs, name+"-pod")
		cleanups = append(cleanups, rs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: "default"}})

		By("building a reconciler with an injected token and a fake clock")
		clock = &deploymentTestClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
		reconciler = &APIMAPIDeploymentReconciler{
			Client:  k8sClient,
			Scheme:  k8sClient.Scheme(),
			fetcher: testOpenAPIFetcher(),
			getToken: func(context.Context, string, string) (string, error) {
				return "fake-token", nil
			},
			// Production delays and attempts without jitter, on a clock the specs move.
			retry: &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now},
		}
	})

	AfterEach(func() {
		for _, obj := range cleanups {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("reaches Succeeded through every write step and clears earlier failures", func() {
		By("seeding two earlier failures for the same desired state, already due")
		deployment := getDeployment()
		desiredHash, err := buildDesiredAPIMStateHash(&deployment.Spec, subscription, resourceGroup, sha256Hex([]byte(firstDoc)))
		Expect(err).NotTo(HaveOccurred())
		deployment.Status.DesiredHash = desiredHash
		deployment.Status.Phase = phaseBackoff
		deployment.Status.ConsecutiveFailures = 2
		deployment.Status.NextAttemptAt = clock.now().Add(-time.Second).Format(time.RFC3339)
		Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

		result := reconcileOnce()

		Expect(result).To(BeZero())
		Expect(arm.stepsSince(0)).To(Equal([]string{
			armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired,
			armStepProduct, armStepTag, armStepServiceDetails,
		}))
		Expect(arm.lastAuth()).To(Equal("Bearer fake-token"))

		updated := getDeployment()
		Expect(updated.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(updated.Status.Status).To(Equal("OK"))
		Expect(updated.Status.LastError).To(BeEmpty())
		Expect(updated.Status.DesiredHash).To(Equal(desiredHash))
		Expect(updated.Status.AppliedHash).To(Equal(desiredHash))
		Expect(updated.Status.OpenAPIHash).To(Equal(sha256Hex([]byte(firstDoc))))
		Expect(updated.Status.ConsecutiveFailures).To(BeZero())
		Expect(updated.Status.NextAttemptAt).To(BeEmpty())

		api := &apimv1.APIMAPI{}
		Expect(k8sClient.Get(ctx, key, api)).To(Succeed())
		Expect(api.Status.ApiHost).To(Equal("https://gw.example.net/retry"))
		Expect(api.Status.DeveloperPortalHost).To(Equal("https://portal.example.net"))

		By("reconciling again with nothing changed")
		calls := arm.count()
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.stepsSince(calls)).To(BeEmpty(), "an applied hash must not be imported again")
	})

	It("backs off on a transient failure and does not call APIM before nextAttemptAt", func() {
		arm.set(armStepImport, armFails(http.StatusPreconditionFailed, "PreconditionFailed"))

		result := reconcileOnce()

		Expect(result).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseBackoff))
		Expect(deployment.Status.Status).To(Equal(phaseError))
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(deployment.Status.NextAttemptAt).To(Equal("2026-09-25T10:01:00Z"))
		Expect(deployment.Status.Message).To(HavePrefix("Failed to import API into APIM: APIM write failed (transient, attempt 1/5)"))
		Expect(deployment.Status.LastError).To(ContainSubstring("412"))
		Expect(deployment.Status.LastError).To(ContainSubstring("PreconditionFailed"))
		Expect(deployment.Status.DesiredHash).NotTo(BeEmpty())
		Expect(deployment.Status.AppliedHash).To(BeEmpty())
		Expect(arm.stepsSince(0)).To(Equal([]string{armStepGetAPI, armStepImport}), "the chain stops at the failing step")

		By("reconciling halfway through the backoff")
		clock.advance(30 * time.Second)
		Expect(expectNoAPIMCall()).To(Equal(ctrl.Result{RequeueAfter: 30 * time.Second}))

		By("reconciling once nextAttemptAt has come, still failing")
		clock.advance(30 * time.Second)
		calls := arm.count()
		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
		Expect(arm.stepsSince(calls)).To(ContainElement(armStepImport))
		deployment = getDeployment()
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(2)))
		Expect(deployment.Status.NextAttemptAt).To(Equal("2026-09-25T10:03:00Z"))
		Expect(deployment.Status.Message).To(ContainSubstring("attempt 2/5"))
	})

	It("stalls after five transient failures and stops calling APIM", func() {
		By("failing the async import the way APIM did in Sep 2026")
		previousTimeout, previousInterval := apim.AsyncWaitTimeout, apim.AsyncPollInterval
		apim.AsyncWaitTimeout, apim.AsyncPollInterval = 5*time.Second, time.Millisecond
		DeferCleanup(func() { apim.AsyncWaitTimeout, apim.AsyncPollInterval = previousTimeout, previousInterval })
		arm.set(armStepImport, armAccepted())
		arm.set(armStepPoll, func(w http.ResponseWriter, _ *http.Request) {
			writeARMJSON(w, http.StatusOK, `{"status":"Failed","error":{"code":"InternalServerError",`+
				`"message":"DeadOperationMonitor"}}`)
		})

		var requeues []time.Duration
		for i := 0; i < 5; i++ {
			if next := getDeployment().Status.NextAttemptAt; next != "" {
				clock.setTo(next)
			}
			requeues = append(requeues, reconcileOnce().RequeueAfter)
		}

		Expect(requeues).To(Equal([]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}))
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseStalled))
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(5)))
		Expect(deployment.Status.NextAttemptAt).To(BeEmpty())
		Expect(deployment.Status.Message).To(HavePrefix("Failed to import API into APIM: APIM write stalled after 5 failures"))
		Expect(deployment.Status.LastError).To(ContainSubstring("InternalServerError"))

		By("reconciling a Stalled deployment with an unchanged spec, much later")
		clock.advance(24 * time.Hour)
		for i := 0; i < 3; i++ {
			Expect(expectNoAPIMCall()).To(BeZero())
		}
	})

	It("goes Invalid at once on a permanent error and stays there", func() {
		arm.set(armStepServiceURL, armFails(http.StatusBadRequest, "ValidationError"))

		Expect(reconcileOnce()).To(BeZero())

		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseInvalid))
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(deployment.Status.NextAttemptAt).To(BeEmpty())
		Expect(deployment.Status.Message).To(HavePrefix("Failed to patch service URL in APIM: APIM rejected the write"))
		Expect(deployment.Status.LastError).To(ContainSubstring("ValidationError"))
		Expect(arm.stepsSince(0)).To(Equal([]string{armStepGetAPI, armStepImport, armStepServiceURL}))

		clock.advance(time.Hour)
		Expect(expectNoAPIMCall()).To(BeZero())
	})

	DescribeTable("every APIM call goes through the shared failure handling",
		func(step string, status int, code, wantPhase, wantMessage string, wantSteps []string) {
			arm.set(step, armFails(status, code))

			result := reconcileOnce()

			deployment := getDeployment()
			Expect(deployment.Status.Phase).To(Equal(wantPhase))
			Expect(deployment.Status.Message).To(HavePrefix(wantMessage + ": "))
			Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(deployment.Status.AppliedHash).To(BeEmpty())
			Expect(deployment.Status.LastError).To(ContainSubstring(fmt.Sprint(status)))
			Expect(arm.stepsSince(0)).To(Equal(wantSteps))
			if wantPhase == phaseBackoff {
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
				Expect(deployment.Status.NextAttemptAt).To(Equal("2026-09-25T10:01:00Z"))
			} else {
				Expect(result).To(BeZero())
				Expect(deployment.Status.NextAttemptAt).To(BeEmpty())
			}
		},
		Entry("import 409 Conflict", armStepImport, http.StatusConflict, "Conflict", phaseBackoff,
			"Failed to import API into APIM", []string{armStepGetAPI, armStepImport}),
		Entry("import 422 Management API timed out", armStepImport, http.StatusUnprocessableEntity, "ManagementApiRequestFailed", phaseBackoff,
			"Failed to import API into APIM", []string{armStepGetAPI, armStepImport}),
		Entry("import 400 with a transient Azure code", armStepImport, http.StatusBadRequest, "PreconditionFailed", phaseBackoff,
			"Failed to import API into APIM", []string{armStepGetAPI, armStepImport}),
		Entry("import 400 ValidationError", armStepImport, http.StatusBadRequest, "ValidationError", phaseInvalid,
			"Failed to import API into APIM", []string{armStepGetAPI, armStepImport}),
		Entry("import 401", armStepImport, http.StatusUnauthorized, "InvalidAuthenticationToken", phaseInvalid,
			"Failed to import API into APIM", []string{armStepGetAPI, armStepImport}),
		Entry("serviceUrl 429", armStepServiceURL, http.StatusTooManyRequests, "TooManyRequests", phaseBackoff,
			"Failed to patch service URL in APIM", []string{armStepGetAPI, armStepImport, armStepServiceURL}),
		Entry("subscriptionRequired 403", armStepSubscriptionRequired, http.StatusForbidden, "AuthorizationFailed", phaseInvalid,
			"Failed to patch subscription requirement in APIM",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired}),
		Entry("subscriptionRequired 500", armStepSubscriptionRequired, http.StatusInternalServerError, "InternalServerError", phaseBackoff,
			"Failed to patch subscription requirement in APIM",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired}),
		// The product is not in APIM yet (its APIMProduct is new or backing off): retried.
		Entry("product 404, product not written yet", armStepProduct, http.StatusNotFound, "ResourceNotFound", phaseBackoff,
			"Failed to assign API to products",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct}),
		Entry("product 403 on a write", armStepProduct, http.StatusForbidden, "AuthorizationFailed", phaseInvalid,
			"Failed to assign API to products",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct}),
		Entry("product 412", armStepProduct, http.StatusPreconditionFailed, "PreconditionFailed", phaseBackoff,
			"Failed to assign API to products",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct}),
		Entry("tag 503", armStepTag, http.StatusServiceUnavailable, "ServiceUnavailable", phaseBackoff,
			"Failed to assign API to tags",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct, armStepTag}),
		Entry("tag 404, tag not written yet", armStepTag, http.StatusNotFound, "ResourceNotFound", phaseBackoff,
			"Failed to assign API to tags",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct, armStepTag}),
		Entry("serviceUrl 404 on the imported API", armStepServiceURL, http.StatusNotFound, "ResourceNotFound", phaseBackoff,
			"Failed to patch service URL in APIM", []string{armStepGetAPI, armStepImport, armStepServiceURL}),
		Entry("tag 400", armStepTag, http.StatusBadRequest, "ValidationError", phaseInvalid,
			"Failed to assign API to tags",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct, armStepTag}),
		Entry("service details 500", armStepServiceDetails, http.StatusInternalServerError, "InternalServerError", phaseBackoff,
			"Failed to fetch APIM service details",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct, armStepTag, armStepServiceDetails}),
		Entry("service details 404 on a read", armStepServiceDetails, http.StatusNotFound, "ResourceNotFound", phaseBackoff,
			"Failed to fetch APIM service details",
			[]string{armStepGetAPI, armStepImport, armStepServiceURL, armStepSubscriptionRequired, armStepProduct, armStepTag, armStepServiceDetails}),
	)

	It("waits for an import that outlives the async wait instead of counting it as a failure", func() {
		previousTimeout, previousInterval := apim.AsyncWaitTimeout, apim.AsyncPollInterval
		apim.AsyncWaitTimeout, apim.AsyncPollInterval = 50*time.Millisecond, 5*time.Millisecond
		DeferCleanup(func() { apim.AsyncWaitTimeout, apim.AsyncPollInterval = previousTimeout, previousInterval })
		arm.set(armStepImport, armAccepted())
		arm.set(armStepPoll, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"InProgress"}`)
		})

		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))

		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseImporting))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
		Expect(deployment.Status.PendingImport).NotTo(BeNil())
		Expect(deployment.Status.AppliedHash).To(BeEmpty())
		Expect(arm.stepsSince(0)).NotTo(ContainElement(armStepServiceURL), "nothing after the import may run")
	})

	It("sends a websocket upsert through the same handling", func() {
		updateDeployment(func(d *apimv1.APIMAPIDeployment) {
			d.Spec.Type = apimv1.APITypeWebSocket
			d.Spec.ServiceURL = "wss://backend.example.net/hub"
		})
		arm.set(armStepWebSocket, armFails(http.StatusConflict, "Conflict"))

		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseBackoff))
		Expect(deployment.Status.Message).To(HavePrefix("Failed to create WebSocket API in APIM: APIM write failed (transient, attempt 1/5)"))

		By("succeeding once due")
		arm.set(armStepWebSocket, nil)
		clock.setTo(deployment.Status.NextAttemptAt)
		Expect(reconcileOnce()).To(BeZero())
		deployment = getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
		api := &apimv1.APIMAPI{}
		Expect(k8sClient.Get(ctx, key, api)).To(Succeed())
		Expect(api.Status.ApiHost).To(Equal("wss://gw.example.net/retry"))
	})

	It("keeps today's behaviour when the token cannot be obtained", func() {
		reconciler.getToken = func(context.Context, string, string) (string, error) {
			return "", errors.New("workload identity unavailable")
		}

		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: 30 * time.Second}))

		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseError))
		Expect(deployment.Status.Message).To(Equal(errMsgFailedToGetAzureToken))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero(), "a token failure is not an APIM write failure")
		Expect(arm.count()).To(BeZero())
	})

	It("does not fetch a token while backing off", func() {
		arm.set(armStepImport, armFails(http.StatusTooManyRequests, "TooManyRequests"))
		reconcileOnce()

		var tokenCalls atomic.Int32
		reconciler.getToken = func(context.Context, string, string) (string, error) {
			tokenCalls.Add(1)
			return "fake-token", nil
		}
		clock.advance(10 * time.Second)
		Expect(expectNoAPIMCall()).To(Equal(ctrl.Result{RequeueAfter: 50 * time.Second}))
		Expect(tokenCalls.Load()).To(BeZero())
	})

	It("clears the failures when the spec changes", func() {
		failUntilStalled()

		By("changing the backend URL while APIM still fails")
		updateDeployment(func(d *apimv1.APIMAPIDeployment) { d.Spec.ServiceURL = "https://backend-v2.example.net" })
		var persisted apimv1.APIMAPIDeploymentStatus
		arm.setOnImport(func() {
			current := &apimv1.APIMAPIDeployment{}
			if err := k8sClient.Get(ctx, key, current); err == nil {
				persisted = current.Status
			}
		})

		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

		Expect(persisted.Phase).To(Equal(apimDeploymentPhaseImporting), "the Importing patch lands before the import")
		Expect(persisted.ConsecutiveFailures).To(BeZero(), "the reset is persisted with the new desired hash")
		Expect(persisted.NextAttemptAt).To(BeEmpty())
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseBackoff))
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(1)), "counting starts over for the new spec")

		By("succeeding once APIM recovers")
		arm.set(armStepImport, nil)
		clock.setTo(deployment.Status.NextAttemptAt)
		Expect(reconcileOnce()).To(BeZero())
		deployment = getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
		Expect(deployment.Status.AppliedHash).To(Equal(deployment.Status.DesiredHash))
	})

	It("counts a new OpenAPI document as a spec change", func() {
		failUntilStalled()
		stalledHash := getDeployment().Status.DesiredHash

		openAPIDoc.Store(`{"openapi":"3.0.0","info":{"title":"retry","version":"2.0.0"},"paths":{}}`)
		arm.set(armStepImport, nil)

		Expect(reconcileOnce()).To(BeZero())
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(deployment.Status.DesiredHash).NotTo(Equal(stalledHash))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
	})

	It("lets a backing-off deployment through on a new desired hash before nextAttemptAt", func() {
		arm.set(armStepImport, armFails(http.StatusConflict, "Conflict"))
		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

		updateDeployment(func(d *apimv1.APIMAPIDeployment) { d.Spec.RoutePrefix = "/retry-v2" })
		arm.set(armStepImport, nil)
		calls := arm.count()

		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.stepsSince(calls)).To(ContainElement(armStepImport))
		Expect(getDeployment().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
	})

	It("resets once per value of the retry annotation", func() {
		arm.set(armStepProduct, armFails(http.StatusForbidden, "AuthorizationFailed"))
		Expect(reconcileOnce()).To(BeZero())
		Expect(getDeployment().Status.Phase).To(Equal(phaseInvalid))

		By("setting the annotation while the operator still may not assign the product")
		setRetryAnnotation("1")
		var persisted apimv1.APIMAPIDeploymentStatus
		arm.setOnImport(func() {
			current := &apimv1.APIMAPIDeployment{}
			if err := k8sClient.Get(ctx, key, current); err == nil {
				persisted = current.Status
			}
		})
		calls := arm.count()
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.stepsSince(calls)).To(ContainElement(armStepProduct))
		Expect(persisted.ConsecutiveFailures).To(BeZero(), "the Importing patch persists the reset")
		Expect(persisted.LastRetryAnnotation).To(Equal("1"))
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseInvalid))
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(deployment.Status.LastRetryAnnotation).To(Equal("1"))

		By("reconciling again with the same value")
		Expect(expectNoAPIMCall()).To(BeZero())

		By("setting a new value once the operator may assign it")
		arm.set(armStepProduct, nil)
		setRetryAnnotation("2")
		Expect(reconcileOnce()).To(BeZero())
		deployment = getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
		Expect(deployment.Status.LastRetryAnnotation).To(Equal("2"))
	})

	It("retries a Stalled deployment on the annotation and stalls again after five more failures", func() {
		failUntilStalled()

		setRetryAnnotation("after-incident")
		calls := arm.count()
		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
		Expect(arm.stepsSince(calls)).To(ContainElement(armStepImport))
		Expect(getDeployment().Status.ConsecutiveFailures).To(Equal(int32(1)))

		for i := 0; i < 4; i++ {
			clock.setTo(getDeployment().Status.NextAttemptAt)
			reconcileOnce()
		}
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(phaseStalled))
		Expect(deployment.Status.ConsecutiveFailures).To(Equal(int32(5)))
		Expect(expectNoAPIMCall()).To(BeZero())
	})

	It("clears the failures without calling APIM when the spec returns to the applied state", func() {
		By("applying the first spec")
		Expect(reconcileOnce()).To(BeZero())
		applied := getDeployment().Status.AppliedHash

		By("failing a change")
		updateDeployment(func(d *apimv1.APIMAPIDeployment) { d.Spec.ServiceURL = "https://broken.example.net" })
		arm.set(armStepServiceURL, armFails(http.StatusBadRequest, "ValidationError"))
		Expect(reconcileOnce()).To(BeZero())
		Expect(getDeployment().Status.Phase).To(Equal(phaseInvalid))

		By("reverting the change")
		updateDeployment(func(d *apimv1.APIMAPIDeployment) { d.Spec.ServiceURL = "https://backend.example.net" })
		calls := arm.count()
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.stepsSince(calls)).To(BeEmpty())
		deployment := getDeployment()
		Expect(deployment.Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(deployment.Status.DesiredHash).To(Equal(applied))
		Expect(deployment.Status.ConsecutiveFailures).To(BeZero())
		Expect(deployment.Status.NextAttemptAt).To(BeEmpty())
	})

	It("treats an unreadable nextAttemptAt as due", func() {
		arm.set(armStepImport, armFails(http.StatusConflict, "Conflict"))
		reconcileOnce()
		deployment := getDeployment()
		deployment.Status.NextAttemptAt = "not-a-time"
		Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

		calls := arm.count()
		Expect(reconcileOnce()).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
		Expect(arm.stepsSince(calls)).To(ContainElement(armStepImport))
		Expect(getDeployment().Status.ConsecutiveFailures).To(Equal(int32(2)))
	})
})
