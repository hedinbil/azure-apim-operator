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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs run the APIMInboundPolicy write path end to end against envtest and an
// httptest server standing in for ARM: the real UpsertInboundPolicy request, the shared
// retry handling, and the status patch. The clock is fake, so the whole backoff and stall
// sequence runs in milliseconds.

// inboundPolicyFakeARM answers every request with the configured status and body and
// records what it was sent.
type inboundPolicyFakeARM struct {
	mu       sync.Mutex
	status   int
	body     string
	requests []inboundPolicyARMRequest
}

// inboundPolicyARMRequest is one request the fake ARM received.
type inboundPolicyARMRequest struct {
	method  string
	path    string
	query   string
	auth    string
	ifMatch string
	// format and value are properties.format and properties.value of the JSON body.
	format string
	value  string
}

func (f *inboundPolicyFakeARM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Properties struct {
			Format string `json:"format"`
			Value  string `json:"value"`
		} `json:"properties"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, inboundPolicyARMRequest{
		method:  r.Method,
		path:    r.URL.Path,
		query:   r.URL.RawQuery,
		auth:    r.Header.Get("Authorization"),
		ifMatch: r.Header.Get("If-Match"),
		format:  body.Properties.Format,
		value:   body.Properties.Value,
	})
	status, respBody := f.status, f.body
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, respBody)
}

// respond sets what the next requests are answered with.
func (f *inboundPolicyFakeARM) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// calls is how many requests reached the fake ARM.
func (f *inboundPolicyFakeARM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// last is the most recent request.
func (f *inboundPolicyFakeARM) last() inboundPolicyARMRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	Expect(f.requests).NotTo(BeEmpty())
	return f.requests[len(f.requests)-1]
}

// inboundPolicyARMError is an ARM error body with an Azure code.
func inboundPolicyARMError(code, message string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
}

// inboundPolicyClock is a settable clock for the retry policy.
type inboundPolicyClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *inboundPolicyClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *inboundPolicyClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *inboundPolicyClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var _ = Describe("APIMInboundPolicy APIM writes", func() {
	const (
		apimServiceName = "policy-retry-apim"
		namespace       = "default"
		subscription    = "00000000-0000-0000-0000-0000000000aa"
		resourceGroup   = "rg-policy-retry"
		apiID           = "policy-retry-api"
		policyXML       = "<policies><inbound><base /></inbound></policies>"
		apiPolicyPath   = "/subscriptions/" + subscription + "/resourceGroups/" + resourceGroup +
			"/providers/Microsoft.ApiManagement/service/" + apimServiceName + "/apis/" + apiID + "/policies/policy"
	)

	var (
		ctx        context.Context
		arm        *inboundPolicyFakeARM
		restoreARM func()
		server     *httptest.Server
		clock      *inboundPolicyClock
		tokenCalls int
		reconciler *APIMInboundPolicyReconciler
		key        types.NamespacedName
		restoreEnv func()
		nameSeq    int
	)

	// setEnv sets an environment variable for one spec and returns its undo.
	setEnv := func(name, value string) func() {
		previous, had := os.LookupEnv(name)
		Expect(os.Setenv(name, value)).To(Succeed())
		return func() {
			if had {
				_ = os.Setenv(name, previous)
			} else {
				_ = os.Unsetenv(name)
			}
		}
	}

	reconcileOnce := func() reconcile.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "failed APIM writes must never return the error")
		return result
	}

	getPolicy := func() *apimv1.APIMInboundPolicy {
		GinkgoHelper()
		var policy apimv1.APIMInboundPolicy
		Expect(k8sClient.Get(ctx, key, &policy)).To(Succeed())
		return &policy
	}

	// mutatePolicy applies fn to the live object and updates it (spec or metadata).
	mutatePolicy := func(fn func(*apimv1.APIMInboundPolicy)) {
		GinkgoHelper()
		policy := getPolicy()
		fn(policy)
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
	}

	// nextAttempt parses status.nextAttemptAt.
	nextAttempt := func(policy *apimv1.APIMInboundPolicy) time.Time {
		GinkgoHelper()
		at, err := time.Parse(time.RFC3339, policy.Status.NextAttemptAt)
		Expect(err).NotTo(HaveOccurred())
		return at
	}

	// failUntilStalled drives the policy through five transient failures, waiting out
	// each backoff on the fake clock.
	failUntilStalled := func() {
		GinkgoHelper()
		arm.respond(http.StatusPreconditionFailed, inboundPolicyARMError("PreconditionFailed", "busy"))
		for i := 1; i <= 5; i++ {
			before := arm.calls()
			result := reconcileOnce()
			Expect(arm.calls()).To(Equal(before+1), "attempt %d must reach APIM", i)
			policy := getPolicy()
			Expect(policy.Status.ConsecutiveFailures).To(Equal(int32(i)))
			if i < 5 {
				Expect(policy.Status.Phase).To(Equal(phaseBackoff))
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				clock.set(nextAttempt(policy))
			} else {
				Expect(policy.Status.Phase).To(Equal(phaseStalled))
				Expect(result).To(BeZero())
			}
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		nameSeq++
		key = types.NamespacedName{Name: fmt.Sprintf("policy-retry-%d", nameSeq), Namespace: namespace}

		arm = &inboundPolicyFakeARM{status: http.StatusOK, body: `{}`}
		server = httptest.NewServer(arm)
		restoreARM = apim.UseEndpoint(server.URL, server.Client())

		undoClient := setEnv("AZURE_CLIENT_ID", "client-id")
		undoTenant := setEnv("AZURE_TENANT_ID", "tenant-id")
		restoreEnv = func() { undoClient(); undoTenant() }

		clock = &inboundPolicyClock{t: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}
		tokenCalls = 0
		reconciler = &APIMInboundPolicyReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			getToken: func(_ context.Context, clientID, tenantID string) (string, error) {
				tokenCalls++
				Expect(clientID).To(Equal("client-id"))
				Expect(tenantID).To(Equal("tenant-id"))
				return "fake-token", nil
			},
			// The production shape without jitter, on the fake clock.
			retry: &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now},
		}

		service := &apimv1.APIMService{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: apimServiceName, Namespace: namespace}, service); err != nil {
			service = &apimv1.APIMService{
				ObjectMeta: metav1.ObjectMeta{Name: apimServiceName, Namespace: namespace},
				Spec: apimv1.APIMServiceSpec{
					Name:          apimServiceName,
					ResourceGroup: resourceGroup,
					Subscription:  subscription,
				},
			}
			Expect(k8sClient.Create(ctx, service)).To(Succeed())
		}

		Expect(k8sClient.Create(ctx, &apimv1.APIMInboundPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec: apimv1.APIMInboundPolicySpec{
				APIMService:   apimServiceName,
				APIID:         apiID,
				PolicyContent: policyXML,
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		restoreARM()
		server.Close()
		restoreEnv()
		policy := &apimv1.APIMInboundPolicy{}
		if err := k8sClient.Get(ctx, key, policy); err == nil {
			Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		}
	})

	It("writes the policy, records the generation and leaves no retry state", func() {
		result := reconcileOnce()
		Expect(result).To(BeZero())

		Expect(arm.calls()).To(Equal(1))
		req := arm.last()
		Expect(req.method).To(Equal(http.MethodPut))
		Expect(req.path).To(Equal(apiPolicyPath))
		Expect(req.query).To(Equal("api-version=2021-08-01"))
		Expect(req.auth).To(Equal("Bearer fake-token"))
		Expect(req.ifMatch).To(Equal("*"))
		Expect(req.format).To(Equal("xml"))
		Expect(req.value).To(Equal(policyXML))

		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseCreated))
		Expect(policy.Status.Message).To(Equal("APIM Inbound Policy created or updated"))
		Expect(policy.Status.ObservedGeneration).To(Equal(policy.Generation))
		Expect(policy.Status.ConsecutiveFailures).To(BeZero())
		Expect(policy.Status.NextAttemptAt).To(BeEmpty())
	})

	It("writes an operation-level policy to the operation's path", func() {
		mutatePolicy(func(p *apimv1.APIMInboundPolicy) { p.Spec.OperationID = "get-orders" })

		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.last().path).To(Equal("/subscriptions/" + subscription + "/resourceGroups/" + resourceGroup +
			"/providers/Microsoft.ApiManagement/service/" + apimServiceName + "/apis/" + apiID +
			"/operations/get-orders/policies/policy"))
		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseCreated))
		Expect(policy.Status.Message).To(Equal("APIM Inbound Policy created or updated for operation get-orders"))
	})

	It("clears earlier failures once a write succeeds", func() {
		arm.respond(http.StatusInternalServerError, inboundPolicyARMError("InternalServerError", "boom"))
		reconcileOnce()
		policy := getPolicy()
		Expect(policy.Status.ConsecutiveFailures).To(Equal(int32(1)))

		clock.set(nextAttempt(policy))
		arm.respond(http.StatusOK, `{}`)
		Expect(reconcileOnce()).To(BeZero())

		policy = getPolicy()
		Expect(arm.calls()).To(Equal(2))
		Expect(policy.Status.Phase).To(Equal(phaseCreated))
		Expect(policy.Status.ConsecutiveFailures).To(BeZero())
		Expect(policy.Status.NextAttemptAt).To(BeEmpty())
	})

	It("backs off after a transient failure, with nextAttemptAt one base delay out", func() {
		arm.respond(http.StatusPreconditionFailed, inboundPolicyARMError("PreconditionFailed", "the resource was modified"))

		result := reconcileOnce()
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(arm.calls()).To(Equal(1))

		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseBackoff))
		Expect(policy.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(policy.Status.NextAttemptAt).To(Equal("2026-09-28T10:01:00Z"))
		Expect(policy.Status.Message).To(ContainSubstring("transient, attempt 1/5"))
		Expect(policy.Status.ObservedGeneration).To(Equal(policy.Generation))
	})

	It("does not call APIM or fetch a token before nextAttemptAt", func() {
		arm.respond(http.StatusTooManyRequests, inboundPolicyARMError("TooManyRequests", "slow down"))
		reconcileOnce()
		Expect(arm.calls()).To(Equal(1))
		Expect(tokenCalls).To(Equal(1))
		before := getPolicy()

		clock.advance(20 * time.Second)
		result := reconcileOnce()
		Expect(result.RequeueAfter).To(Equal(40 * time.Second))
		Expect(arm.calls()).To(Equal(1), "a backing-off policy must not reach APIM")
		Expect(tokenCalls).To(Equal(1), "a backing-off policy must not fetch a token")

		after := getPolicy()
		Expect(after.Status).To(Equal(before.Status), "skipping must not change the status")
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))

		By("writing again once the backoff has passed, with the doubled delay next")
		clock.advance(40 * time.Second)
		result = reconcileOnce()
		Expect(arm.calls()).To(Equal(2))
		Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
		Expect(getPolicy().Status.ConsecutiveFailures).To(Equal(int32(2)))
	})

	It("stalls after five transient failures and stops calling APIM", func() {
		failUntilStalled()

		policy := getPolicy()
		Expect(policy.Status.NextAttemptAt).To(BeEmpty())
		Expect(policy.Status.Message).To(ContainSubstring("stalled after 5 failures"))
		Expect(policy.Status.Message).To(ContainSubstring(retryAnnotation))

		By("staying put however long it waits")
		clock.advance(24 * time.Hour)
		for range 3 {
			Expect(reconcileOnce()).To(BeZero())
		}
		Expect(arm.calls()).To(Equal(5))
		Expect(getPolicy().Status.Phase).To(Equal(phaseStalled))
	})

	DescribeTable("classifies the APIM answer",
		func(status int, code string, wantPhase string) {
			arm.respond(status, inboundPolicyARMError(code, "answer"))
			result := reconcileOnce()
			policy := getPolicy()
			Expect(policy.Status.Phase).To(Equal(wantPhase))
			Expect(policy.Status.ConsecutiveFailures).To(Equal(int32(1)))
			if wantPhase == phaseInvalid {
				Expect(result).To(BeZero())
				Expect(policy.Status.NextAttemptAt).To(BeEmpty())
			} else {
				Expect(result.RequeueAfter).To(Equal(time.Minute))
				Expect(policy.Status.NextAttemptAt).NotTo(BeEmpty())
			}
		},
		Entry("409 Conflict is transient", http.StatusConflict, "Conflict", phaseBackoff),
		Entry("412 PreconditionFailed is transient", http.StatusPreconditionFailed, "PreconditionFailed", phaseBackoff),
		Entry("422 ManagementApiRequestFailed is transient", http.StatusUnprocessableEntity, "ManagementApiRequestFailed", phaseBackoff),
		Entry("429 is transient", http.StatusTooManyRequests, "TooManyRequests", phaseBackoff),
		Entry("500 is transient", http.StatusInternalServerError, "InternalServerError", phaseBackoff),
		Entry("503 is transient", http.StatusServiceUnavailable, "ServiceUnavailable", phaseBackoff),
		Entry("400 with a transient Azure code is transient", http.StatusBadRequest, "ManagementApiRequestFailed", phaseBackoff),
		Entry("400 ValidationError is permanent", http.StatusBadRequest, "ValidationError", phaseInvalid),
		Entry("401 is permanent", http.StatusUnauthorized, "InvalidAuthenticationToken", phaseInvalid),
		Entry("403 is permanent", http.StatusForbidden, "LinkedAuthorizationFailed", phaseInvalid),
		// The API is not imported yet, typically because its APIMAPIDeployment waits for a
		// ready pod while the policy applied in the same sync is reconciled at once. It
		// clears up when the import lands, so the policy retries rather than gives up.
		Entry("404 on the PUT (API not imported yet) is transient", http.StatusNotFound, "ResourceNotFound", phaseBackoff),
	)

	It("backs off on a transport failure", func() {
		server.Close() // ARM unreachable: connection refused.

		result := reconcileOnce()
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseBackoff))
		Expect(policy.Status.ConsecutiveFailures).To(Equal(int32(1)))
	})

	It("marks a rejected policy Invalid at once and does not retry it", func() {
		arm.respond(http.StatusBadRequest, inboundPolicyARMError("ValidationError", "'set-header' is not a valid element"))

		Expect(reconcileOnce()).To(BeZero())
		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseInvalid))
		Expect(policy.Status.NextAttemptAt).To(BeEmpty())
		Expect(policy.Status.Message).To(ContainSubstring("APIM rejected the write"))

		clock.advance(24 * time.Hour)
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.calls()).To(Equal(1))
		Expect(tokenCalls).To(Equal(1))
	})

	It("starts a Stalled policy over when the spec changes", func() {
		failUntilStalled()

		mutatePolicy(func(p *apimv1.APIMInboundPolicy) {
			p.Spec.PolicyContent = "<policies><inbound><base /><set-header name=\"x\" exists-action=\"override\"><value>1</value></set-header></inbound></policies>"
		})
		arm.respond(http.StatusOK, `{}`)

		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.calls()).To(Equal(6))
		Expect(arm.last().value).To(ContainSubstring("set-header"))
		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseCreated))
		Expect(policy.Status.ConsecutiveFailures).To(BeZero())
		Expect(policy.Status.ObservedGeneration).To(Equal(policy.Generation))
	})

	It("counts from one again when a spec change fails during a backoff", func() {
		arm.respond(http.StatusConflict, inboundPolicyARMError("Conflict", "busy"))
		reconcileOnce()
		clock.set(nextAttempt(getPolicy()))
		reconcileOnce()
		Expect(getPolicy().Status.ConsecutiveFailures).To(Equal(int32(2)))

		By("changing the spec while the second backoff is still running")
		mutatePolicy(func(p *apimv1.APIMInboundPolicy) { p.Spec.PolicyContent = "<policies><inbound /></policies>" })
		result := reconcileOnce()
		Expect(arm.calls()).To(Equal(3), "a spec change must not wait out the old backoff")
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		policy := getPolicy()
		Expect(policy.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(policy.Status.Phase).To(Equal(phaseBackoff))
	})

	It("retries an Invalid policy once per new value of the retry annotation", func() {
		arm.respond(http.StatusForbidden, inboundPolicyARMError("LinkedAuthorizationFailed", "no access"))
		reconcileOnce()
		Expect(getPolicy().Status.Phase).To(Equal(phaseInvalid))

		By("setting the annotation, which retries even though the spec is unchanged")
		mutatePolicy(func(p *apimv1.APIMInboundPolicy) {
			p.Annotations = map[string]string{retryAnnotation: "1"}
		})
		reconcileOnce()
		Expect(arm.calls()).To(Equal(2))
		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseInvalid))
		Expect(policy.Status.ConsecutiveFailures).To(Equal(int32(1)), "the reset makes this attempt 1 again")
		Expect(policy.Status.LastRetryAnnotation).To(Equal("1"))

		By("the same value does not retry again")
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.calls()).To(Equal(2))

		By("a new value retries, and this time APIM accepts it")
		arm.respond(http.StatusOK, `{}`)
		mutatePolicy(func(p *apimv1.APIMInboundPolicy) { p.Annotations[retryAnnotation] = "2" })
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.calls()).To(Equal(3))
		policy = getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseCreated))
		Expect(policy.Status.ConsecutiveFailures).To(BeZero())
		Expect(policy.Status.LastRetryAnnotation).To(Equal("2"))
	})

	It("retries a Stalled policy when the retry annotation is set", func() {
		failUntilStalled()

		mutatePolicy(func(p *apimv1.APIMInboundPolicy) {
			p.Annotations = map[string]string{retryAnnotation: "2026-09-28T12:00:00Z"}
		})
		arm.respond(http.StatusOK, `{}`)
		Expect(reconcileOnce()).To(BeZero())
		Expect(arm.calls()).To(Equal(6))
		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseCreated))
		Expect(policy.Status.ConsecutiveFailures).To(BeZero())
	})

	It("keeps the token failure path as it is and makes no APIM call", func() {
		reconciler.getToken = func(context.Context, string, string) (string, error) {
			return "", fmt.Errorf("no workload identity token file")
		}
		result := reconcileOnce()
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		Expect(arm.calls()).To(BeZero())
		policy := getPolicy()
		Expect(policy.Status.Phase).To(Equal(phaseError))
		Expect(policy.Status.ConsecutiveFailures).To(BeZero())
	})
})

var _ = Describe("APIMInboundPolicy update predicate", func() {
	// newPolicy builds one side of an update event for apimInboundPolicyUpdateFilter, the
	// update predicate SetupWithManager installs.
	newPolicy := func(content string, annotations map[string]string) *apimv1.APIMInboundPolicy {
		return &apimv1.APIMInboundPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Annotations: annotations},
			Spec:       apimv1.APIMInboundPolicySpec{APIMService: "svc", APIID: "api", PolicyContent: content},
		}
	}

	It("passes a retry annotation change and ignores other metadata changes", func() {
		filter := apimInboundPolicyUpdateFilter()
		old := newPolicy("<a/>", nil)

		Expect(filter(event.UpdateEvent{ObjectOld: old, ObjectNew: newPolicy("<a/>", map[string]string{retryAnnotation: "1"})})).To(BeTrue())
		Expect(filter(event.UpdateEvent{ObjectOld: old, ObjectNew: newPolicy("<a/>", map[string]string{"other": "x"})})).To(BeFalse())
		Expect(filter(event.UpdateEvent{ObjectOld: old, ObjectNew: newPolicy("<b/>", nil)})).To(BeTrue())
		Expect(filter(event.UpdateEvent{ObjectOld: old, ObjectNew: newPolicy("<a/>", nil)})).To(BeFalse())
	})
})
