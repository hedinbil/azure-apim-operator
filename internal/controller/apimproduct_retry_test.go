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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// errProductHasSubscriptions is what APIM answers a product delete it refuses: a 400,
// which no retry will change.
var errProductHasSubscriptions = &apim.Error{
	Operation:  "delete product test-product-id",
	Method:     http.MethodDelete,
	StatusCode: http.StatusBadRequest,
	Code:       "ValidationError",
	Message:    "product has active subscriptions",
}

// productFakeARM stands in for Azure Resource Manager in the APIMProduct retry tests. It
// answers every request with the reply currently set and counts product PUTs and DELETEs.
type productFakeARM struct {
	srv *httptest.Server

	mu       sync.Mutex
	status   int
	body     string
	puts     int
	deletes  int
	requests []string // "METHOD path"
	auth     string   // Authorization header of the last request
}

func newProductFakeARM() *productFakeARM {
	f := &productFakeARM{status: http.StatusOK, body: `{}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			f.puts++
		case http.MethodDelete:
			f.deletes++
		}
		f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		f.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	return f
}

// reply sets what every following request gets: a status and, for a failure, an ARM error body.
func (f *productFakeARM) reply(status int, code, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
	f.body = `{}`
	if code != "" {
		f.body = fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
	}
}

// calls returns how many product PUTs and DELETEs ARM has seen.
func (f *productFakeARM) calls() (puts, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.deletes
}

var _ = Describe("APIMProduct Controller retry handling", func() {
	const (
		productName     = "retry-product"
		productID       = "retry-product-id"
		apimServiceName = "product-retry-apim-service"
	)

	var (
		ctx         context.Context
		key         types.NamespacedName
		serviceKey  types.NamespacedName
		arm         *productFakeARM
		clock       *fakeClock
		tokenCalls  int
		reconciler  *APIMProductReconciler
		restoreARM  func()
		restoreEnv  func()
		logLines    func() []logLine
		start       = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
		reconcileIt = func() ctrl.Result {
			GinkgoHelper()
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(), "a failed APIM write must never be handed to the rate limiter")
			return result
		}
		get = func() *apimv1.APIMProduct {
			GinkgoHelper()
			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, key, product)).To(Succeed())
			return product
		}
		puts = func() int {
			p, _ := arm.calls()
			return p
		}
		deletes = func() int {
			_, d := arm.calls()
			return d
		}
		annotate = func(value string) {
			GinkgoHelper()
			product := get()
			if product.Annotations == nil {
				product.Annotations = map[string]string{}
			}
			product.Annotations[retryAnnotation] = value
			Expect(k8sClient.Update(ctx, product)).To(Succeed())
		}
		changeSpec = func(displayName string) {
			GinkgoHelper()
			product := get()
			product.Spec.DisplayName = displayName
			Expect(k8sClient.Update(ctx, product)).To(Succeed())
		}
		// failTransiently fails the product's write times in a row with a 503, each
		// reconcile once the previous backoff is over, and returns the requeues. Five
		// times makes it Stalled.
		failTransiently = func(times int) []time.Duration {
			GinkgoHelper()
			arm.reply(http.StatusServiceUnavailable, "ServiceUnavailable", "APIM is busy")
			waits := make([]time.Duration, 0, times)
			for range times {
				result := reconcileIt()
				waits = append(waits, result.RequeueAfter)
				clock.advance(result.RequeueAfter)
			}
			return waits
		}
	)

	BeforeEach(func() {
		var logger logr.Logger
		logger, logLines = captureLogs()
		ctx = log.IntoContext(context.Background(), logger)
		key = types.NamespacedName{Name: productName, Namespace: "default"}
		serviceKey = types.NamespacedName{Name: apimServiceName, Namespace: "default"}

		restoreEnv = stubAzureIdentityEnv()
		arm = newProductFakeARM()
		restoreARM = apim.UseEndpoint(arm.srv.URL, arm.srv.Client())
		clock = &fakeClock{t: start}
		tokenCalls = 0
		reconciler = &APIMProductReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			getToken: func(context.Context, string, string) (string, error) {
				tokenCalls++
				return "fake-token", nil
			},
			retry: &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now},
		}

		Expect(k8sClient.Create(ctx, &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: apimServiceName, Namespace: "default"},
			Spec: apimv1.APIMServiceSpec{
				Name:          apimServiceName,
				ResourceGroup: "rg-retry",
				Subscription:  "00000000-0000-0000-0000-000000000002",
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &apimv1.APIMProduct{
			ObjectMeta: metav1.ObjectMeta{Name: productName, Namespace: "default"},
			Spec: apimv1.APIMProductSpec{
				APIMService: apimServiceName,
				ProductID:   productID,
				DisplayName: "Retry Product",
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		restoreARM()
		arm.srv.Close()
		restoreEnv()
		forceDeleteProduct(ctx, key)
		service := &apimv1.APIMService{}
		if err := k8sClient.Get(ctx, serviceKey, service); err == nil {
			Expect(k8sClient.Delete(ctx, service)).To(Succeed())
		}
	})

	It("writes the product through ARM and records observedGeneration", func() {
		Expect(reconcileIt()).To(BeZero())

		Expect(puts()).To(Equal(1))
		Expect(arm.requests).To(ConsistOf(HavePrefix(
			"PUT /subscriptions/00000000-0000-0000-0000-000000000002/resourceGroups/rg-retry/providers/Microsoft.ApiManagement/service/" +
				apimServiceName + "/products/" + productID + "?api-version=")))
		Expect(arm.auth).To(Equal("Bearer fake-token"))

		product := get()
		Expect(product.Status.Phase).To(Equal(phaseCreated))
		Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
		Expect(product.Status.RetryStatus).To(BeZero())
	})

	It("clears earlier failures on success", func() {
		product := get()
		product.Status.Phase = phaseBackoff
		product.Status.ObservedGeneration = product.Generation
		product.Status.ConsecutiveFailures = 3
		product.Status.NextAttemptAt = start.Add(-time.Minute).Format(time.RFC3339)
		Expect(k8sClient.Status().Update(ctx, product)).To(Succeed())

		Expect(reconcileIt()).To(BeZero())

		Expect(puts()).To(Equal(1), "a backoff that has run out lets the write through")
		product = get()
		Expect(product.Status.Phase).To(Equal(phaseCreated))
		Expect(product.Status.Message).To(Equal("Product created successfully"))
		Expect(product.Status.ConsecutiveFailures).To(BeZero())
		Expect(product.Status.NextAttemptAt).To(BeEmpty())
		Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
	})

	It("backs off after a transient failure and does not call APIM before nextAttemptAt", func() {
		arm.reply(http.StatusPreconditionFailed, "PreconditionFailed", "the resource was modified")

		result := reconcileIt()
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		product := get()
		Expect(product.Status.Phase).To(Equal(phaseBackoff))
		Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(product.Status.NextAttemptAt).To(Equal("2026-09-25T10:01:00Z"))
		Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
		Expect(product.Status.Message).To(ContainSubstring("PreconditionFailed"))
		Expect(product.Status.Message).To(ContainSubstring("attempt 1/5"))
		Expect(puts()).To(Equal(1))
		Expect(tokenCalls).To(Equal(1))

		By("reconciling half way through the backoff")
		clock.advance(30 * time.Second)
		result = reconcileIt()
		Expect(result.RequeueAfter).To(Equal(30*time.Second), "the requeue is the time that is left")
		Expect(puts()).To(Equal(1), "no APIM call while backing off")
		Expect(tokenCalls).To(Equal(1), "not even a token while backing off")
		Expect(get().Status).To(Equal(product.Status), "a skipped reconcile writes no status")

		By("reconciling once the backoff is over")
		clock.advance(30 * time.Second)
		result = reconcileIt()
		Expect(puts()).To(Equal(2))
		Expect(result.RequeueAfter).To(Equal(2*time.Minute), "the wait doubles")
		Expect(get().Status.ConsecutiveFailures).To(Equal(int32(2)))
	})

	It("stalls after five transient failures and stops calling APIM", func() {
		waits := failTransiently(5)
		Expect(waits).To(Equal([]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}))
		Expect(puts()).To(Equal(5))

		product := get()
		Expect(product.Status.Phase).To(Equal(phaseStalled))
		Expect(product.Status.ConsecutiveFailures).To(Equal(int32(5)))
		Expect(product.Status.NextAttemptAt).To(BeEmpty())
		Expect(product.Status.Message).To(ContainSubstring("stalled after 5 failures"))
		Expect(product.Status.Message).To(ContainSubstring(retryAnnotation))

		By("reconciling a day later: still nothing")
		clock.advance(24 * time.Hour)
		Expect(reconcileIt()).To(BeZero())
		Expect(reconcileIt()).To(BeZero())
		Expect(puts()).To(Equal(5))

		By("logging the stable line the Datadog monitor matches")
		var stalled []logLine
		for _, line := range logLines() {
			if line.msg == "🛑 APIM write stalled" {
				stalled = append(stalled, line)
			}
		}
		Expect(stalled).To(HaveLen(1))
		Expect(stalled[0].kv).To(HaveKeyWithValue("kind", "APIMProduct"))
		Expect(stalled[0].kv).To(HaveKeyWithValue("namespace", "default"))
		Expect(stalled[0].kv).To(HaveKeyWithValue("name", productName))
		Expect(stalled[0].kv).To(HaveKeyWithValue("productID", productID))
		Expect(stalled[0].kv).To(HaveKeyWithValue("attempts", int32(5)))
		Expect(stalled[0].kv).To(HaveKey("lastError"))
	})

	DescribeTable("rejects a write APIM will never accept as Invalid, without retrying",
		func(status int, code string) {
			arm.reply(status, code, "no")

			Expect(reconcileIt()).To(BeZero())
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseInvalid))
			Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(product.Status.NextAttemptAt).To(BeEmpty())
			Expect(product.Status.Message).To(ContainSubstring(code))

			clock.advance(time.Hour)
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(1), "an Invalid product is not written again")
		},
		Entry("400", http.StatusBadRequest, "ValidationError"),
		Entry("401", http.StatusUnauthorized, "InvalidAuthenticationToken"),
		Entry("403", http.StatusForbidden, "AuthorizationFailed"),
		Entry("404 on a write", http.StatusNotFound, "ResourceNotFound"),
	)

	It("treats a transient Azure code as transient whatever the status", func() {
		arm.reply(http.StatusBadRequest, "ManagementApiRequestFailed", "Management API timed out")
		Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute))
		Expect(get().Status.Phase).To(Equal(phaseBackoff))
	})

	It("starts over on a spec change, from Invalid and in the middle of a backoff", func() {
		arm.reply(http.StatusBadRequest, "ValidationError", "bad display name")
		Expect(reconcileIt()).To(BeZero())
		Expect(get().Status.Phase).To(Equal(phaseInvalid))

		By("fixing the spec: the write happens again at once")
		arm.reply(http.StatusServiceUnavailable, "ServiceUnavailable", "busy")
		changeSpec("Retry Product v2")
		Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute))
		Expect(puts()).To(Equal(2))
		product := get()
		Expect(product.Status.Phase).To(Equal(phaseBackoff))
		Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)), "the Invalid failure was cleared first")

		By("changing the spec again while backing off: no waiting for nextAttemptAt")
		arm.reply(http.StatusOK, "", "")
		changeSpec("Retry Product v3")
		Expect(reconcileIt()).To(BeZero())
		Expect(puts()).To(Equal(3))
		product = get()
		Expect(product.Status.Phase).To(Equal(phaseCreated))
		Expect(product.Status.RetryStatus).To(BeZero())
		Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
	})

	It("retries once per new value of the retry annotation", func() {
		failTransiently(5)
		Expect(get().Status.Phase).To(Equal(phaseStalled))

		By("setting the annotation on a Stalled product")
		arm.reply(http.StatusBadRequest, "ValidationError", "still wrong")
		annotate("1")
		Expect(reconcileIt()).To(BeZero())
		Expect(puts()).To(Equal(6))
		product := get()
		Expect(product.Status.Phase).To(Equal(phaseInvalid))
		Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)), "the five stalled failures were cleared first")
		Expect(product.Status.LastRetryAnnotation).To(Equal("1"))

		By("reconciling again with the same value: it does not retrigger")
		Expect(reconcileIt()).To(BeZero())
		Expect(puts()).To(Equal(6))

		By("setting a new value")
		arm.reply(http.StatusOK, "", "")
		annotate("2")
		Expect(reconcileIt()).To(BeZero())
		Expect(puts()).To(Equal(7))
		product = get()
		Expect(product.Status.Phase).To(Equal(phaseCreated))
		Expect(product.Status.ConsecutiveFailures).To(BeZero())
		Expect(product.Status.LastRetryAnnotation).To(Equal("2"))
	})

	Context("with deletionPolicy Delete", func() {
		BeforeEach(func() {
			setDeletionPolicy(ctx, key, apimv1.DeletionPolicyDelete)
		})

		// deleteResource creates the product in APIM, then deletes the resource.
		deleteResource := func() {
			GinkgoHelper()
			Expect(reconcileIt()).To(BeZero())
			Expect(controllerutil.ContainsFinalizer(get(), productFinalizer)).To(BeTrue())
			Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		}

		It("backs off a failed delete and releases the finalizer once it succeeds", func() {
			deleteResource()

			arm.reply(http.StatusConflict, "Conflict", "operation in progress")
			Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute))
			Expect(deletes()).To(Equal(1))
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseBackoff))
			Expect(product.Status.Message).To(HavePrefix("Failed to delete product in APIM"))
			Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())

			By("reconciling before nextAttemptAt: no APIM call")
			clock.advance(20 * time.Second)
			Expect(reconcileIt().RequeueAfter).To(Equal(40 * time.Second))
			Expect(deletes()).To(Equal(1))

			By("reconciling after it: the delete goes through")
			clock.advance(40 * time.Second)
			arm.reply(http.StatusOK, "", "")
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(2))
			Expect(arm.requests[len(arm.requests)-1]).To(ContainSubstring("deleteSubscriptions=true"))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("counts a product that is already gone as deleted", func() {
			deleteResource()
			arm.reply(http.StatusNotFound, "ResourceNotFound", "no such product")
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(1))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("holds a rejected delete as Invalid until deletionPolicy is switched to Retain", func() {
			deleteResource()

			arm.reply(http.StatusForbidden, "AuthorizationFailed", "no delete permission")
			Expect(reconcileIt()).To(BeZero())
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseInvalid))
			Expect(product.Status.Message).To(ContainSubstring("AuthorizationFailed"))
			Expect(product.Status.Message).To(ContainSubstring("spec.deletionPolicy to Retain"))
			Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
			rejected := 0
			for _, line := range logLines() {
				if line.msg == "💔 APIM write rejected; not retrying" {
					rejected++
					Expect(line.kv).To(HaveKeyWithValue("operation", "delete"))
				}
			}
			Expect(rejected).To(Equal(1))

			clock.advance(time.Hour)
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(1), "an Invalid delete is not tried again")

			setDeletionPolicy(ctx, key, apimv1.DeletionPolicyRetain)
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(1), "Retain lets the resource go without APIM")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("retries a rejected delete when the retry annotation is set", func() {
			deleteResource()
			arm.reply(http.StatusBadRequest, "ValidationError", "product has active subscriptions")
			Expect(reconcileIt()).To(BeZero())
			Expect(get().Status.Phase).To(Equal(phaseInvalid))

			arm.reply(http.StatusOK, "", "")
			annotate("after-cleanup")
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(2))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &apimv1.APIMProduct{}))).To(BeTrue())
		})

		It("still tries the delete of a product that stalled while being created", func() {
			failTransiently(5)
			Expect(get().Status.Phase).To(Equal(phaseStalled))
			Expect(k8sClient.Delete(ctx, get())).To(Succeed())

			arm.reply(http.StatusOK, "", "")
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(1), "the generation bump of the delete starts the attempts over")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, key, &apimv1.APIMProduct{}))).To(BeTrue())
		})
	})

	It("logs the write's start and success", func() {
		Expect(reconcileIt()).To(BeZero())
		var msgs []string
		for _, line := range logLines() {
			if strings.Contains(line.msg, "APIM write") {
				msgs = append(msgs, line.msg)
				Expect(line.kv).To(HaveKeyWithValue("productID", productID))
				Expect(line.kv).To(HaveKeyWithValue("operation", "upsert"))
			}
		}
		Expect(msgs).To(Equal([]string{msgWriteStarting, msgWriteSucceeded}))
	})
})
