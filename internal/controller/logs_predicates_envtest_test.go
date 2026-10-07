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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs run the real reconcilers of the four kinds that write to APIM against
// envtest and a fake ARM, and check the lines each one logs through the shared retry
// handling, with the keys its controller adds. A second spec starts a manager with the
// controllers wired exactly as SetupWithManager wires them, to prove which updates reach
// Reconcile on a real API server.

// lpSeq keeps resource names unique across specs.
var lpSeq atomic.Int32

// lpFakeARM answers every request with the configured status and body.
type lpFakeARM struct {
	mu     sync.Mutex
	status int
	body   string
	calls  int
}

func (f *lpFakeARM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.calls++
	status, body := f.status, f.body
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *lpFakeARM) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *lpFakeARM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func lpARMError(code, message string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
}

// lpSetEnv sets an environment variable for the rest of the spec.
func lpSetEnv(name, value string) {
	previous, had := os.LookupEnv(name)
	Expect(os.Setenv(name, value)).To(Succeed())
	DeferCleanup(func() {
		if had {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

// lpHarness drives one resource of one kind through its real reconciler.
type lpHarness struct {
	kind string
	key  types.NamespacedName
	// ids are the keys the controller adds to every retry line, with their values.
	ids          map[string]string
	successPhase string
	clock        *lpClock

	reconcile func() ctrl.Result
	// lines returns the lines logged since resetLines.
	lines      func() []lpLine
	resetLines func()
	// writes counts the requests (or seam calls) that reached APIM.
	writes        func() int
	succeed       func()
	failTransient func()
	failPermanent func()
	changeSpec    func()
	setRetry      func(string)
	// state returns the phase and the retry status.
	state func() (string, apimv1.RetryStatus)
}

// retryLines returns the retry handling's lines for this kind and clears the capture.
func (h *lpHarness) retryLines() []lpLine {
	GinkgoHelper()
	lines := lpRetryLines(h.lines(), h.kind)
	h.resetLines()
	for _, l := range lines {
		Expect(l.keys).To(HaveLen(len(l.kv)), "no key may be logged twice: %v", l.keys)
		Expect(l.keys[:3]).To(Equal([]string{"kind", "namespace", "name"}), "%s: identity keys first", l.msg)
		Expect(l.str("namespace")).To(Equal(h.key.Namespace), l.msg)
		Expect(l.str("name")).To(Equal(h.key.Name), l.msg)
		for k, v := range h.ids {
			Expect(l.str(k)).To(Equal(v), "%s: key %s", l.msg, k)
		}
	}
	return lines
}

// lpRunLogScenario is the same story for every kind: success, a transient failure, a
// skipped reconcile while backing off, stalled after five, held, the retry annotation
// with a permanent error (rejected), held again, and a spec change that succeeds.
func lpRunLogScenario(h *lpHarness) {
	GinkgoHelper()

	By(h.kind + ": a first write succeeds with ▶️ and 💚")
	h.succeed()
	h.resetLines()
	Expect(h.reconcile()).To(BeZero())
	lines := h.retryLines()
	starts := lpWithMsg(lines, lpMsgStarting)
	Expect(starts).NotTo(BeEmpty())
	for _, l := range starts {
		Expect(l.str("attempt")).To(Equal("1/5"))
		Expect(l.isError).To(BeFalse())
	}
	Expect(lpMsgs(lines)[len(lines)-1]).To(Equal(lpMsgSucceeded))
	Expect(lpWithMsg(lines, lpMsgSucceeded)).To(HaveLen(1))
	Expect(lpWithMsg(lines, lpMsgSucceeded)[0].str("attempt")).To(Equal("1/5"))
	Expect(lpMsgs(lines)).To(HaveEach(BeElementOf(lpMsgStarting, lpMsgSucceeded)))
	phase, st := h.state()
	Expect(phase).To(Equal(h.successPhase))
	Expect(st.ConsecutiveFailures).To(BeZero())

	By(h.kind + ": a transient failure logs 💔 with attempt 1/5 and the next attempt time")
	h.changeSpec()
	h.failTransient()
	result := h.reconcile()
	lines = h.retryLines()
	Expect(lpMsgs(lines)).To(HaveEach(BeElementOf(lpMsgStarting, lpMsgFailed)))
	failed := lpWithMsg(lines, lpMsgFailed)
	Expect(failed).To(HaveLen(1))
	Expect(lines[len(lines)-1].msg).To(Equal(lpMsgFailed))
	_, st = h.state()
	Expect(st.NextAttemptAt).To(Equal(h.clock.now().Add(time.Minute).UTC().Format(time.RFC3339)))
	Expect(failed[0].isError).To(BeTrue())
	Expect(failed[0].str("class")).To(Equal("transient"))
	Expect(failed[0].str("attempt")).To(Equal("1/5"))
	Expect(failed[0].str("nextAttemptAt")).To(Equal(st.NextAttemptAt))
	Expect(failed[0].errText).To(ContainSubstring("503"))
	Expect(result.RequeueAfter).To(Equal(time.Minute))

	By(h.kind + ": a reconcile before nextAttemptAt logs only ⏸️ and writes nothing")
	writes := h.writes()
	result = h.reconcile()
	lines = h.retryLines()
	Expect(lpMsgs(lines)).To(Equal([]string{lpMsgBackingOff}))
	Expect(lines[0].isError).To(BeFalse())
	Expect(lines[0].str("attempt")).To(Equal("2/5"))
	Expect(lines[0].str("nextAttemptAt")).To(Equal(st.NextAttemptAt))
	Expect(lines[0].str("remaining")).To(Equal("1m0s"))
	Expect(h.writes()).To(Equal(writes))
	Expect(result.RequeueAfter).To(Equal(time.Minute))

	By(h.kind + ": attempts 2 to 4 back off, the fifth logs 🛑 APIM write stalled")
	for n := 2; n <= 5; n++ {
		_, st = h.state()
		next, err := time.Parse(time.RFC3339, st.NextAttemptAt)
		Expect(err).NotTo(HaveOccurred())
		h.clock.set(next)
		result = h.reconcile()
		lines = h.retryLines()
		label := fmt.Sprintf("%d/5", n)
		for _, l := range lpWithMsg(lines, lpMsgStarting) {
			Expect(l.str("attempt")).To(Equal(label))
		}
		last := lines[len(lines)-1]
		if n < 5 {
			Expect(last.msg).To(Equal(lpMsgFailed))
			Expect(last.str("attempt")).To(Equal(label))
			Expect(result.RequeueAfter).To(Equal(time.Minute << (n - 1)))
			continue
		}
		Expect(last.msg).To(Equal("🛑 APIM write stalled"))
		Expect(last.isError).To(BeTrue())
		Expect(last.str("attempts")).To(Equal("5"))
		Expect(last.str("lastError")).To(ContainSubstring("503"))
		Expect(last.str("lastError")).To(Equal(last.errText))
		Expect(last.has("nextAttemptAt")).To(BeFalse())
		Expect(lpWithMsg(lines, lpMsgFailed)).To(BeEmpty())
		Expect(result).To(BeZero())
	}
	phase, st = h.state()
	Expect(phase).To(Equal(phaseStalled))
	Expect(st.NextAttemptAt).To(BeEmpty())

	By(h.kind + ": a stalled resource logs the hold and writes nothing")
	h.clock.advance(24 * time.Hour)
	writes = h.writes()
	Expect(h.reconcile()).To(BeZero())
	lines = h.retryLines()
	Expect(lpMsgs(lines)).To(Equal([]string{lpMsgHeld}))
	Expect(lines[0].str("attempts")).To(Equal("5"))
	Expect(h.writes()).To(Equal(writes))

	By(h.kind + ": the retry annotation logs 🔁, then a permanent error logs the rejection")
	h.failPermanent()
	h.setRetry("lp-1")
	Expect(h.reconcile()).To(BeZero())
	lines = h.retryLines()
	Expect(lines[0].msg).To(Equal(lpMsgReset))
	Expect(lines[0].str("reason")).To(Equal("apim.operator.io/retry annotation set"))
	Expect(lines[0].str("previousFailures")).To(Equal("5"))
	Expect(lines[1].msg).To(Equal(lpMsgStarting))
	Expect(lines[1].str("attempt")).To(Equal("1/5"))
	last := lines[len(lines)-1]
	Expect(last.msg).To(Equal("💔 APIM write rejected; not retrying"))
	Expect(last.isError).To(BeTrue())
	Expect(last.str("class")).To(Equal("permanent"))
	Expect(last.str("attempt")).To(Equal("1/5"))
	Expect(last.errText).To(ContainSubstring("400"))
	Expect(h.writes()).To(BeNumerically(">", writes))
	phase, st = h.state()
	Expect(phase).To(Equal(phaseInvalid))
	Expect(st.LastRetryAnnotation).To(Equal("lp-1"))

	By(h.kind + ": an Invalid resource with the same annotation value is held")
	writes = h.writes()
	Expect(h.reconcile()).To(BeZero())
	lines = h.retryLines()
	Expect(lpMsgs(lines)).To(Equal([]string{lpMsgHeld}))
	Expect(h.writes()).To(Equal(writes))

	By(h.kind + ": a spec change logs 🔁 and the write succeeds")
	h.succeed()
	h.changeSpec()
	Expect(h.reconcile()).To(BeZero())
	lines = h.retryLines()
	Expect(lines[0].msg).To(Equal(lpMsgReset))
	Expect(lines[0].str("reason")).To(Equal("spec changed"))
	Expect(lines[0].str("previousFailures")).To(Equal("1"))
	Expect(lines[len(lines)-1].msg).To(Equal(lpMsgSucceeded))
	Expect(lines[len(lines)-1].str("attempt")).To(Equal("1/5"))
	phase, st = h.state()
	Expect(phase).To(Equal(h.successPhase))
	Expect(st.ConsecutiveFailures).To(BeZero())
	Expect(st.NextAttemptAt).To(BeEmpty())
}

// lpPolicyOn is the production retry shape without jitter on clock.
func lpPolicyOn(clock *lpClock) *retryPolicy {
	return &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now}
}

// lpCreateService creates an APIMService in the operator namespace for one spec.
func lpCreateService(ctx context.Context, name string) {
	GinkgoHelper()
	svc := &apimv1.APIMService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       apimv1.APIMServiceSpec{Name: name, Subscription: "00000000-0000-0000-0000-0000000000aa", ResourceGroup: "rg-lp"},
	}
	Expect(k8sClient.Create(ctx, svc)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), svc) })
}

// lpUpdate re-reads obj, applies mutate and writes it back.
func lpUpdate(ctx context.Context, obj client.Object, mutate func()) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
	mutate()
	Expect(k8sClient.Update(ctx, obj)).To(Succeed())
}

// lpSetAnnotation sets one annotation on obj (after re-reading it).
func lpSetAnnotation(ctx context.Context, obj client.Object, key, value string) {
	GinkgoHelper()
	lpUpdate(ctx, obj, func() {
		a := obj.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[key] = value
		obj.SetAnnotations(a)
	})
}

var _ = Describe("logs-predicates: APIM write log lines through the real reconcilers", func() {
	var (
		ctx     context.Context
		capture *lpCapture
		clock   *lpClock
	)

	BeforeEach(func() {
		capture = &lpCapture{}
		// Product, tag and policy log through log.FromContext.
		ctx = logf.IntoContext(context.Background(), capture.logger())
		clock = &lpClock{t: lpStart}
		lpSetEnv("AZURE_CLIENT_ID", "lp-client")
		lpSetEnv("AZURE_TENANT_ID", "lp-tenant")
	})

	token := func(context.Context, string, string) (string, error) { return "lp-token", nil }

	It("APIMTag", func() {
		n := lpSeq.Add(1)
		svc := fmt.Sprintf("lp-svc-tag-%d", n)
		lpCreateService(ctx, svc)
		arm := &lpFakeARM{status: http.StatusOK, body: `{"name":"t"}`}
		server := httptest.NewServer(arm)
		DeferCleanup(server.Close)
		DeferCleanup(apim.UseEndpoint(server.URL, server.Client()))

		tag := &apimv1.APIMTag{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("lp-tag-%d", n), Namespace: "default"},
			Spec:       apimv1.APIMTagSpec{APIMService: svc, TagID: "lp-tag-id", DisplayName: "LP"},
		}
		Expect(k8sClient.Create(ctx, tag)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), tag) })

		r := &APIMTagReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: lpPolicyOn(clock)}
		key := client.ObjectKeyFromObject(tag)
		lpRunLogScenario(&lpHarness{
			kind: "APIMTag", key: key, ids: map[string]string{"tagID": "lp-tag-id"},
			successPhase: phaseCreated, clock: clock,
			reconcile: func() ctrl.Result {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				return res
			},
			lines: capture.all, resetLines: capture.reset, writes: arm.count,
			succeed:       func() { arm.respond(http.StatusOK, `{"name":"t"}`) },
			failTransient: func() { arm.respond(http.StatusServiceUnavailable, lpARMError("ServiceUnavailable", "busy")) },
			failPermanent: func() { arm.respond(http.StatusBadRequest, lpARMError("ValidationError", "bad")) },
			changeSpec: func() {
				lpUpdate(ctx, tag, func() { tag.Spec.DisplayName += "x" })
			},
			setRetry: func(v string) { lpSetAnnotation(ctx, tag, retryAnnotation, v) },
			state: func() (string, apimv1.RetryStatus) {
				var got apimv1.APIMTag
				Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				return got.Status.Phase, got.Status.RetryStatus
			},
		})
	})

	It("APIMProduct", func() {
		n := lpSeq.Add(1)
		svc := fmt.Sprintf("lp-svc-product-%d", n)
		lpCreateService(ctx, svc)

		var mu sync.Mutex
		var calls int
		var next error
		upsert := func(context.Context, apim.APIMProductConfig) error {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return next
		}
		setNext := func(err error) {
			mu.Lock()
			defer mu.Unlock()
			next = err
		}

		product := &apimv1.APIMProduct{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("lp-product-%d", n), Namespace: "default"},
			Spec:       apimv1.APIMProductSpec{APIMService: svc, ProductID: "lp-product-id", DisplayName: "LP"},
		}
		Expect(k8sClient.Create(ctx, product)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), product) })

		r := &APIMProductReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token,
			upsertProduct: upsert, retry: lpPolicyOn(clock)}
		key := client.ObjectKeyFromObject(product)
		lpRunLogScenario(&lpHarness{
			kind: "APIMProduct", key: key,
			ids:          map[string]string{"productID": "lp-product-id", "operation": "upsert"},
			successPhase: phaseCreated, clock: clock,
			reconcile: func() ctrl.Result {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				return res
			},
			lines: capture.all, resetLines: capture.reset,
			writes: func() int {
				mu.Lock()
				defer mu.Unlock()
				return calls
			},
			succeed: func() { setNext(nil) },
			failTransient: func() {
				setNext(&apim.Error{Operation: "upsert product lp-product-id", Method: http.MethodPut,
					StatusCode: http.StatusServiceUnavailable, Code: "ServiceUnavailable", Message: "busy"})
			},
			failPermanent: func() {
				setNext(&apim.Error{Operation: "upsert product lp-product-id", Method: http.MethodPut,
					StatusCode: http.StatusBadRequest, Code: "ValidationError", Message: "bad"})
			},
			changeSpec: func() {
				lpUpdate(ctx, product, func() { product.Spec.Description += "x" })
			},
			setRetry: func(v string) { lpSetAnnotation(ctx, product, retryAnnotation, v) },
			state: func() (string, apimv1.RetryStatus) {
				var got apimv1.APIMProduct
				Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				return got.Status.Phase, got.Status.RetryStatus
			},
		})
	})

	It("APIMInboundPolicy", func() {
		n := lpSeq.Add(1)
		svc := fmt.Sprintf("lp-svc-policy-%d", n)
		lpCreateService(ctx, svc)
		arm := &lpFakeARM{status: http.StatusOK, body: `{}`}
		server := httptest.NewServer(arm)
		DeferCleanup(server.Close)
		DeferCleanup(apim.UseEndpoint(server.URL, server.Client()))

		policy := &apimv1.APIMInboundPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("lp-policy-%d", n), Namespace: "default"},
			Spec: apimv1.APIMInboundPolicySpec{APIMService: svc, APIID: "lp-api", OperationID: "lp-op",
				PolicyContent: "<policies><inbound><base /></inbound></policies>"},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), policy) })

		r := &APIMInboundPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: lpPolicyOn(clock)}
		key := client.ObjectKeyFromObject(policy)
		lpRunLogScenario(&lpHarness{
			kind: "APIMInboundPolicy", key: key,
			ids:          map[string]string{"apiID": "lp-api", "operationID": "lp-op"},
			successPhase: phaseCreated, clock: clock,
			reconcile: func() ctrl.Result {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				return res
			},
			lines: capture.all, resetLines: capture.reset, writes: arm.count,
			succeed:       func() { arm.respond(http.StatusOK, `{}`) },
			failTransient: func() { arm.respond(http.StatusServiceUnavailable, lpARMError("ServiceUnavailable", "busy")) },
			failPermanent: func() { arm.respond(http.StatusBadRequest, lpARMError("ValidationError", "bad")) },
			changeSpec: func() {
				lpUpdate(ctx, policy, func() {
					policy.Spec.PolicyContent = strings.Replace(policy.Spec.PolicyContent, "<base />", "<base /><base />", 1)
				})
			},
			setRetry: func(v string) { lpSetAnnotation(ctx, policy, retryAnnotation, v) },
			state: func() (string, apimv1.RetryStatus) {
				var got apimv1.APIMInboundPolicy
				Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				return got.Status.Phase, got.Status.RetryStatus
			},
		})
	})

	It("APIMAPIDeployment", func() {
		// The deployment logs through ctrl.Log, which the suite sends to GinkgoWriter;
		// tee that and parse the zap lines.
		tee := &lpWriter{}
		GinkgoWriter.TeeTo(tee)
		DeferCleanup(GinkgoWriter.ClearTeeWriters)

		n := lpSeq.Add(1)
		name := fmt.Sprintf("lp-deploy-%d", n)
		apiID := fmt.Sprintf("lp-deploy-api-%d", n)
		svc := fmt.Sprintf("lp-svc-deploy-%d", n)
		const (
			subscription  = "00000000-0000-0000-0000-0000000000aa"
			resourceGroup = "rg-lp"
		)
		Expect(k8sClient.Create(ctx, &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: "default"},
			Spec:       apimv1.APIMServiceSpec{Name: svc, Subscription: subscription, ResourceGroup: resourceGroup},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &apimv1.APIMService{ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: "default"}})
		})

		docServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"openapi":"3.0.0","info":{"title":"lp","version":"1.0.0"},"paths":{}}`)
		}))
		DeferCleanup(docServer.Close)
		arm := newDeploymentFakeARM(subscription, resourceGroup, svc, apiID)
		DeferCleanup(arm.server.Close)
		DeferCleanup(apim.UseEndpoint(arm.server.URL, arm.server.Client()))

		api := &apimv1.APIMAPI{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: apimv1.APIMAPISpec{APIID: apiID, APIMService: svc, RoutePrefix: "/lp",
				ServiceURL: "https://backend.example.net", OpenAPIDefinitionURL: docServer.URL},
		}
		Expect(k8sClient.Create(ctx, api)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), api) })
		deployment := &apimv1.APIMAPIDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: apimv1.APIMAPIDeploymentSpec{APIID: apiID, APIMService: svc, APIMAPIName: name,
				Subscription: subscription, ResourceGroup: resourceGroup, RoutePrefix: "/lp",
				ServiceURL: "https://backend.example.net", OpenAPIDefinitionURL: docServer.URL,
				SubscriptionRequired: true, ProductIDs: []string{"lp-product"}, TagIDs: []string{"lp-tag"}},
		}
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), deployment) })
		rs := createReplicaSet(ctx, name+"-rs", map[string]string{"app.kubernetes.io/name": name}, map[string]string{"app": name})
		createReadyPodForReplicaSet(ctx, rs, name+"-pod")
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), rs)
			_ = k8sClient.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: "default"}})
		})

		r := &APIMAPIDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), fetcher: testOpenAPIFetcher(),
			getToken: token, retry: lpPolicyOn(clock)}
		key := client.ObjectKeyFromObject(deployment)
		urlSeq := 0
		lpRunLogScenario(&lpHarness{
			kind: "APIMAPIDeployment", key: key, ids: map[string]string{"apiID": apiID},
			successPhase: apimDeploymentPhaseSucceeded, clock: clock,
			reconcile: func() ctrl.Result {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				return res
			},
			lines:         func() []lpLine { return lpParseZapConsole(tee.String()) },
			resetLines:    tee.reset,
			writes:        arm.count,
			succeed:       func() { arm.set(armStepImport, nil) },
			failTransient: func() { arm.set(armStepImport, armFails(http.StatusServiceUnavailable, "ServiceUnavailable")) },
			failPermanent: func() { arm.set(armStepImport, armFails(http.StatusBadRequest, "ValidationError")) },
			changeSpec: func() {
				urlSeq++
				lpUpdate(ctx, deployment, func() {
					deployment.Spec.ServiceURL = fmt.Sprintf("https://backend-%d.example.net", urlSeq)
				})
			},
			setRetry: func(v string) { lpSetAnnotation(ctx, deployment, retryAnnotation, v) },
			state: func() (string, apimv1.RetryStatus) {
				var got apimv1.APIMAPIDeployment
				Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
				return got.Status.Phase, got.Status.RetryStatus
			},
		})
	})
})

var _ = Describe("logs-predicates: which updates reach Reconcile with the controllers wired by SetupWithManager", func() {
	const namespace = "lp-wired"

	It("lets the retry annotation and spec changes through for all four kinds, and nothing else", func() {
		ctx := context.Background()

		// Every reconcile of these objects ends early with a log line naming the object:
		// the APIMService (looked up in OPERATOR_NAMESPACE) does not exist for product,
		// tag and policy, and the APIMAPI does not exist for the deployment. Counting
		// those lines counts reconciles.
		lpSetEnv("OPERATOR_NAMESPACE", namespace)
		tee := &lpWriter{}
		GinkgoWriter.TeeTo(tee)
		DeferCleanup(GinkgoWriter.ClearTeeWriters)

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}))).To(Succeed())

		skip := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
			Controller:             config.Controller{SkipNameValidation: &skip},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&APIMAPIDeploymentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		Expect((&APIMProductReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		Expect((&APIMTagReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		Expect((&APIMInboundPolicyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			done <- mgr.Start(mgrCtx)
		}()
		// Stop the manager before deleting what the spec created, so it does not
		// reconcile the deletes.
		created := make([]client.Object, 0, 8) // two objects of each of the four kinds
		DeferCleanup(func() {
			stop()
			Eventually(done, 10*time.Second).Should(Receive(Not(HaveOccurred())))
			for _, obj := range created {
				_ = k8sClient.Delete(context.Background(), obj)
			}
		})

		n := lpSeq.Add(1)
		type wiredKind struct {
			kind   string
			marker string
			newObj func(name string) client.Object
			spec   func(client.Object)
			status func(client.Object)
		}
		kinds := []wiredKind{
			{
				kind: "APIMAPIDeployment", marker: "🧩 Loaded APIMAPIDeployment",
				newObj: func(name string) client.Object {
					return &apimv1.APIMAPIDeployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
						Spec: apimv1.APIMAPIDeploymentSpec{APIID: name, APIMService: "lp-missing", Subscription: "00000000-0000-0000-0000-0000000000aa",
							ResourceGroup: "rg", RoutePrefix: "/lp", ServiceURL: "https://a.example.net"}}
				},
				spec: func(o client.Object) { o.(*apimv1.APIMAPIDeployment).Spec.ServiceURL = "https://b.example.net" },
				status: func(o client.Object) {
					d := o.(*apimv1.APIMAPIDeployment)
					d.Status.Message = "status only"
					d.Status.LastRetryAnnotation = "zzz"
				},
			},
			{
				kind: "APIMProduct", marker: "⏳ APIMService",
				newObj: func(name string) client.Object {
					return &apimv1.APIMProduct{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
						Spec: apimv1.APIMProductSpec{APIMService: "lp-missing", ProductID: name, DisplayName: "A"}}
				},
				spec: func(o client.Object) { o.(*apimv1.APIMProduct).Spec.DisplayName = "B" },
				status: func(o client.Object) {
					p := o.(*apimv1.APIMProduct)
					p.Status.Message = "status only"
					p.Status.LastRetryAnnotation = "zzz"
				},
			},
			{
				kind: "APIMTag", marker: "⏳ APIMService",
				newObj: func(name string) client.Object {
					return &apimv1.APIMTag{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
						Spec: apimv1.APIMTagSpec{APIMService: "lp-missing", TagID: name, DisplayName: "A"}}
				},
				spec: func(o client.Object) { o.(*apimv1.APIMTag).Spec.DisplayName = "B" },
				status: func(o client.Object) {
					tg := o.(*apimv1.APIMTag)
					tg.Status.Message = "status only"
					tg.Status.LastRetryAnnotation = "zzz"
				},
			},
			{
				kind: "APIMInboundPolicy", marker: "⏳ APIMService",
				newObj: func(name string) client.Object {
					return &apimv1.APIMInboundPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
						Spec: apimv1.APIMInboundPolicySpec{APIMService: "lp-missing", APIID: "lp-api", PolicyContent: "<a/>"}}
				},
				spec: func(o client.Object) { o.(*apimv1.APIMInboundPolicy).Spec.PolicyContent = "<b/>" },
				status: func(o client.Object) {
					p := o.(*apimv1.APIMInboundPolicy)
					p.Status.Message = "status only"
					p.Status.LastRetryAnnotation = "zzz"
				},
			},
		}

		for _, k := range kinds {
			By(k.kind)
			lower := strings.ToLower(strings.TrimPrefix(k.kind, "APIM"))
			a := k.newObj(fmt.Sprintf("lp-a-%s-%d", lower, n))
			b := k.newObj(fmt.Sprintf("lp-b-%s-%d", lower, n))
			reconciles := func(obj client.Object) func() int {
				quoted := fmt.Sprintf("%q", obj.GetName())
				return func() int {
					count := 0
					for _, line := range strings.Split(tee.String(), "\n") {
						if strings.Contains(line, k.marker) && strings.Contains(line, quoted) {
							count++
						}
					}
					return count
				}
			}
			countA, countB := reconciles(a), reconciles(b)

			Expect(k8sClient.Create(ctx, a)).To(Succeed())
			Expect(k8sClient.Create(ctx, b)).To(Succeed())
			created = append(created, a, b)

			By(k.kind + ": create reconciles once, and the controller's own status write does not retrigger it")
			Eventually(countA, 10*time.Second, 10*time.Millisecond).Should(Equal(1))
			Eventually(countB, 10*time.Second, 10*time.Millisecond).Should(Equal(1))

			By(k.kind + ": unrelated metadata, an empty retry value and status-only updates are filtered")
			lpSetAnnotation(ctx, a, "example.com/note", "x")
			lpSetAnnotation(ctx, a, "kubectl.kubernetes.io/last-applied-configuration", "{}")
			lpUpdate(ctx, a, func() { a.SetLabels(map[string]string{"team": "lp"}) })
			lpSetAnnotation(ctx, a, retryAnnotation, "")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
			k.status(a)
			Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
			// A later, passing event on B: the informer delivers events in order, so any
			// reconcile of A that the updates above caused is queued ahead of this one.
			lpSetAnnotation(ctx, b, retryAnnotation, "b1")
			Eventually(countB, 10*time.Second, 10*time.Millisecond).Should(Equal(2))
			Consistently(countA, 300*time.Millisecond, 10*time.Millisecond).Should(Equal(1))

			By(k.kind + ": adding, changing and removing the retry annotation each reconcile")
			lpSetAnnotation(ctx, a, retryAnnotation, "1")
			Eventually(countA, 10*time.Second, 10*time.Millisecond).Should(Equal(2))
			lpSetAnnotation(ctx, a, retryAnnotation, "2")
			Eventually(countA, 10*time.Second, 10*time.Millisecond).Should(Equal(3))
			lpUpdate(ctx, a, func() {
				annotations := a.GetAnnotations()
				delete(annotations, retryAnnotation)
				a.SetAnnotations(annotations)
			})
			Eventually(countA, 10*time.Second, 10*time.Millisecond).Should(Equal(4))

			By(k.kind + ": a spec change reconciles")
			lpUpdate(ctx, a, func() { k.spec(a) })
			Eventually(countA, 10*time.Second, 10*time.Millisecond).Should(Equal(5))

			By(k.kind + ": and nothing more happens afterwards")
			lpSetAnnotation(ctx, b, retryAnnotation, "b2")
			Eventually(countB, 10*time.Second, 10*time.Millisecond).Should(Equal(3))
			Consistently(countA, 300*time.Millisecond, 10*time.Millisecond).Should(Equal(5))
		}
	})
})
