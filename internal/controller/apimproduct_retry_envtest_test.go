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
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs run the APIMProduct reconciler's full write path (token, ARM request, error
// shaping, classification, retry state, status patch, finalizer) against envtest and a
// fake ARM, with a fixed clock. Nothing sleeps: time only moves when a spec advances it.
var _ = Describe("APIMProduct retry state machine (envtest, fake ARM)", func() {
	const (
		peName         = "pe-product"
		peProductID    = "pe-product-id"
		peService      = "pe-apim-service"
		peSubscription = "00000000-0000-0000-0000-000000000077"
		peRG           = "rg-pe"
	)
	peProductPath := "/subscriptions/" + peSubscription + "/resourceGroups/" + peRG +
		"/providers/Microsoft.ApiManagement/service/" + peService + "/products/" + peProductID

	var (
		ctx        context.Context
		key        types.NamespacedName
		serviceKey types.NamespacedName
		arm        *peFakeARM
		clock      *fakeClock
		tokenCalls int
		tokenArgs  []string
		reconciler *APIMProductReconciler
		restoreARM func()
		restoreEnv func()
		logLines   func() []logLine
		start      = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	)

	reconcileIt := func() ctrl.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "a failed APIM write must never be handed to the rate limiter")
		return result
	}
	get := func() *apimv1.APIMProduct {
		GinkgoHelper()
		product := &apimv1.APIMProduct{}
		Expect(k8sClient.Get(ctx, key, product)).To(Succeed())
		return product
	}
	gone := func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, key, &apimv1.APIMProduct{}))
	}
	puts := func() int { return arm.count(http.MethodPut) }
	deletes := func() int { return arm.count(http.MethodDelete) }
	annotate := func(value string) {
		GinkgoHelper()
		product := get()
		if product.Annotations == nil {
			product.Annotations = map[string]string{}
		}
		product.Annotations[retryAnnotation] = value
		Expect(k8sClient.Update(ctx, product)).To(Succeed())
	}
	changeSpec := func(displayName string) {
		GinkgoHelper()
		product := get()
		product.Spec.DisplayName = displayName
		Expect(k8sClient.Update(ctx, product)).To(Succeed())
	}
	// fail reconciles times in a row with method answering reply, each once the previous
	// backoff is over, and returns the requeues.
	fail := func(method string, reply peReply, times int) []time.Duration {
		GinkgoHelper()
		arm.set(method, reply)
		waits := make([]time.Duration, 0, times)
		for range times {
			result := reconcileIt()
			waits = append(waits, result.RequeueAfter)
			clock.advance(result.RequeueAfter)
		}
		return waits
	}
	busy := peARMError(http.StatusServiceUnavailable, "ServiceUnavailable", "APIM is busy")
	forbidden := peARMError(http.StatusForbidden, "LinkedAuthorizationFailed", "no write permission")

	// driveTo puts the product's current write (upsert, or delete once deleting) into
	// state and returns the consecutive failures that state has. Backoff is left in the
	// middle of its second wait, so nextAttemptAt is still ahead.
	driveTo := func(method, state string) int32 {
		GinkgoHelper()
		switch state {
		case phaseBackoff:
			arm.set(method, busy)
			clock.advance(reconcileIt().RequeueAfter)
			Expect(reconcileIt().RequeueAfter).To(Equal(2 * time.Minute))
			clock.advance(30 * time.Second)
			Expect(get().Status.Phase).To(Equal(phaseBackoff))
			return 2
		case phaseStalled:
			fail(method, busy, 5)
			Expect(get().Status.Phase).To(Equal(phaseStalled))
			return 5
		case phaseInvalid:
			arm.set(method, forbidden)
			Expect(reconcileIt()).To(BeZero())
			Expect(get().Status.Phase).To(Equal(phaseInvalid))
			return 1
		}
		Fail("unknown state " + state)
		return 0
	}
	// deleteResource writes the product to APIM (taking the finalizer), then deletes the
	// resource so the next reconcile is the finalizer's delete.
	deleteResource := func() {
		GinkgoHelper()
		arm.set(http.MethodPut, peOK)
		Expect(reconcileIt()).To(BeZero())
		Expect(controllerutil.ContainsFinalizer(get(), productFinalizer)).To(BeTrue())
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		Expect(get().DeletionTimestamp.IsZero()).To(BeFalse())
	}
	removeService := func() {
		GinkgoHelper()
		service := &apimv1.APIMService{}
		Expect(k8sClient.Get(ctx, serviceKey, service)).To(Succeed())
		Expect(k8sClient.Delete(ctx, service)).To(Succeed())
	}
	createService := func() {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: peService, Namespace: "default"},
			Spec: apimv1.APIMServiceSpec{
				Name:          peService,
				ResourceGroup: peRG,
				Subscription:  peSubscription,
			},
		})).To(Succeed())
	}
	linesWith := func(msg string) []logLine { return peLinesWith(logLines(), msg) }

	BeforeEach(func() {
		var logger logr.Logger
		logger, logLines = captureLogs()
		ctx = log.IntoContext(context.Background(), logger)
		key = types.NamespacedName{Name: peName, Namespace: "default"}
		serviceKey = types.NamespacedName{Name: peService, Namespace: "default"}

		restoreEnv = stubAzureIdentityEnv()
		arm = newPEFakeARM()
		restoreARM = apim.UseEndpoint(arm.srv.URL, arm.srv.Client())
		clock = &fakeClock{t: start}
		tokenCalls = 0
		tokenArgs = nil
		reconciler = &APIMProductReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			getToken: func(_ context.Context, clientID, tenantID string) (string, error) {
				tokenCalls++
				tokenArgs = []string{clientID, tenantID}
				return "pe-token", nil
			},
			retry: &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now},
		}

		createService()
		Expect(k8sClient.Create(ctx, &apimv1.APIMProduct{
			ObjectMeta: metav1.ObjectMeta{Name: peName, Namespace: "default"},
			Spec: apimv1.APIMProductSpec{
				APIMService: peService,
				ProductID:   peProductID,
				DisplayName: "PE Product",
				Description: "envtest product",
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

	Describe("a successful upsert", func() {
		DescribeTable("writes the product through ARM and records observedGeneration",
			func(policy apimv1.DeletionPolicy, published bool, wantState string) {
				product := get()
				product.Spec.DeletionPolicy = policy
				product.Spec.Published = published
				Expect(k8sClient.Update(ctx, product)).To(Succeed())

				Expect(reconcileIt()).To(BeZero())

				Expect(puts()).To(Equal(1))
				req, ok := arm.last(http.MethodPut)
				Expect(ok).To(BeTrue())
				Expect(req.path).To(Equal(peProductPath))
				Expect(req.query).To(HavePrefix("api-version="))
				Expect(req.auth).To(Equal("Bearer pe-token"))
				Expect(req.ifMatch).To(Equal("*"))
				Expect(req.contentType).To(Equal("application/json"))
				var body struct {
					Properties map[string]any `json:"properties"`
				}
				Expect(json.Unmarshal([]byte(req.body), &body)).To(Succeed())
				Expect(body.Properties).To(HaveKeyWithValue("displayName", "PE Product"))
				Expect(body.Properties).To(HaveKeyWithValue("description", "envtest product"))
				Expect(body.Properties).To(HaveKeyWithValue("state", wantState))
				Expect(tokenCalls).To(Equal(1))
				Expect(tokenArgs).To(Equal([]string{"test-client-id", "test-tenant-id"}))

				product = get()
				Expect(product.Status.Phase).To(Equal(phaseCreated))
				Expect(product.Status.Message).To(Equal("Product created successfully"))
				Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
				Expect(product.Status.ObservedGeneration).To(BeNumerically(">", 0))
				Expect(product.Status.RetryStatus).To(BeZero())
				Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).
					To(Equal(policy == apimv1.DeletionPolicyDelete))
				Expect(deletes()).To(BeZero())
			},
			Entry("Retain, not published", apimv1.DeletionPolicyRetain, false, "notPublished"),
			Entry("Retain, published", apimv1.DeletionPolicyRetain, true, "published"),
			Entry("Delete, not published", apimv1.DeletionPolicyDelete, false, "notPublished"),
			Entry("Delete, published", apimv1.DeletionPolicyDelete, true, "published"),
		)

		It("moves observedGeneration along with every spec change it writes", func() {
			Expect(reconcileIt()).To(BeZero())
			first := get()
			Expect(first.Status.ObservedGeneration).To(Equal(first.Generation))

			for i, name := range []string{"PE Product v2", "PE Product v3"} {
				changeSpec(name)
				Expect(reconcileIt()).To(BeZero())
				product := get()
				Expect(product.Generation).To(Equal(first.Generation + int64(i+1)))
				Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
				req, _ := arm.last(http.MethodPut)
				Expect(req.body).To(ContainSubstring(name))
			}
		})

		It("logs ▶️ and 💚 with the attempt number, and nothing else from the retry path", func() {
			Expect(reconcileIt()).To(BeZero())
			starting := linesWith(msgWriteStarting)
			Expect(starting).To(HaveLen(1))
			Expect(starting[0].kv).To(HaveKeyWithValue("attempt", "1/5"))
			Expect(starting[0].kv).To(HaveKeyWithValue("kind", "APIMProduct"))
			Expect(starting[0].kv).To(HaveKeyWithValue("productID", peProductID))
			succeeded := linesWith(msgWriteSucceeded)
			Expect(succeeded).To(HaveLen(1))
			Expect(succeeded[0].kv).To(HaveKeyWithValue("operation", "upsert"))
			for _, msg := range []string{msgWriteFailed, msgWriteRejected, msgWriteStalled, msgWriteBackingOff, msgWriteHeld, msgWriteReset} {
				Expect(linesWith(msg)).To(BeEmpty(), msg)
			}
		})
	})

	Describe("transient upsert failures", func() {
		DescribeTable("back off 1, 2, 4, 8 minutes and stall at the fifth",
			func(reply peReply, wantInMessage string) {
				arm.set(http.MethodPut, reply)
				wantWaits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
				for i, want := range wantWaits {
					now := clock.t
					result := reconcileIt()
					Expect(result.RequeueAfter).To(Equal(want), "requeue after failure %d", i+1)
					product := get()
					Expect(product.Status.Phase).To(Equal(phaseBackoff))
					Expect(product.Status.ConsecutiveFailures).To(Equal(int32(i + 1)))
					Expect(product.Status.NextAttemptAt).To(Equal(now.Add(want).Format(time.RFC3339)))
					Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
					Expect(product.Status.Message).To(HavePrefix("Failed to create product in APIM: "))
					Expect(product.Status.Message).To(ContainSubstring(wantInMessage))
					Expect(product.Status.Message).To(ContainSubstring("transient"))
					Expect(product.Status.Message).To(ContainSubstring("attempt %d/5", i+1))
					Expect(puts()).To(Equal(i + 1))

					By("a reconcile half way through the wait does not call APIM")
					clock.advance(want / 2)
					Expect(reconcileIt().RequeueAfter).To(Equal(want - want/2))
					Expect(puts()).To(Equal(i + 1))
					clock.advance(want - want/2)
				}

				Expect(reconcileIt()).To(BeZero(), "Stalled is not requeued")
				product := get()
				Expect(product.Status.Phase).To(Equal(phaseStalled))
				Expect(product.Status.ConsecutiveFailures).To(Equal(int32(5)))
				Expect(product.Status.NextAttemptAt).To(BeEmpty())
				Expect(product.Status.Message).To(ContainSubstring(wantInMessage))
				Expect(product.Status.Message).To(ContainSubstring("stalled after 5 failures"))
				Expect(product.Status.Message).To(ContainSubstring(retryAnnotation))
				Expect(product.Status.Message).NotTo(ContainSubstring("deletionPolicy"),
					"the Retain hint belongs to a held delete only")
				Expect(puts()).To(Equal(5))
				Expect(tokenCalls).To(Equal(5))

				By("staying quiet for days")
				for range 3 {
					clock.advance(24 * time.Hour)
					Expect(reconcileIt()).To(BeZero())
				}
				Expect(puts()).To(Equal(5))
				Expect(tokenCalls).To(Equal(5), "a Stalled product does not even fetch a token")
			},
			Entry("409 Conflict", peARMError(http.StatusConflict, "Conflict", "operation in progress"), "Conflict"),
			Entry("412 PreconditionFailed", peARMError(http.StatusPreconditionFailed, "PreconditionFailed", "etag mismatch"),
				"PreconditionFailed"),
			Entry("422 Management API timed out",
				peARMError(http.StatusUnprocessableEntity, "ManagementApiRequestFailed", "Management API timed out"),
				"Management API timed out"),
			Entry("429 throttled", peARMError(http.StatusTooManyRequests, "TooManyRequests", "slow down"), "429"),
			Entry("500", peARMError(http.StatusInternalServerError, "InternalServerError", "DeadOperationMonitor"),
				"DeadOperationMonitor"),
			Entry("502 with a non-JSON body", peReply{status: http.StatusBadGateway, body: "<html>Bad Gateway</html>"},
				"<html>Bad Gateway</html>"),
			Entry("503", busy, "APIM is busy"),
			Entry("504", peARMError(http.StatusGatewayTimeout, "GatewayTimeout", "upstream timed out"), "GatewayTimeout"),
			Entry("400 with Azure code InternalServerError",
				peARMError(http.StatusBadRequest, "InternalServerError", "odd"), "InternalServerError"),
			Entry("400 with Azure code Timeout", peARMError(http.StatusBadRequest, "Timeout", "took too long"), "Timeout"),
			Entry("400 ValidationError whose first detail is PreconditionFailed",
				peARMErrorWithDetail(http.StatusBadRequest, "ValidationError", "PreconditionFailed", "racing"),
				"ValidationError/PreconditionFailed"),
			Entry("403 with Azure code ManagementApiRequestFailed",
				peARMError(http.StatusForbidden, "ManagementApiRequestFailed", "busy"), "ManagementApiRequestFailed"),
			Entry("an unexpected 4xx defaults to transient", peARMError(http.StatusTeapot, "Teapot", "short and stout"),
				"Teapot"),
		)

		It("writes the exact status message for a backoff", func() {
			arm.set(http.MethodPut, busy)
			Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute))
			Expect(get().Status.Message).To(Equal(
				"Failed to create product in APIM: upsert product " + peProductID +
					" failed: 503 Service Unavailable: ServiceUnavailable: APIM is busy; " +
					"APIM write failed (transient, attempt 1/5); next attempt at 2026-09-25T10:01:00Z"))
		})

		It("treats a transport failure as transient", func() {
			dead := httptest.NewServer(http.NotFoundHandler())
			deadURL := dead.URL
			dead.Close()
			restoreDead := apim.UseEndpoint(deadURL, &http.Client{Timeout: 2 * time.Second})
			defer restoreDead()

			Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute))
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseBackoff))
			Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(product.Status.Message).To(ContainSubstring("upsert product " + peProductID))
			Expect(product.Status.Message).To(ContainSubstring("transient"))
		})

		It("lets the write through exactly at nextAttemptAt and not a second before", func() {
			arm.set(http.MethodPut, busy)
			Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute))

			clock.advance(59 * time.Second)
			Expect(reconcileIt().RequeueAfter).To(Equal(time.Second))
			Expect(puts()).To(Equal(1))

			clock.advance(time.Second)
			Expect(reconcileIt().RequeueAfter).To(Equal(2 * time.Minute))
			Expect(puts()).To(Equal(2))
		})

		It("skips without fetching a token or touching the status while backing off", func() {
			arm.set(http.MethodPut, busy)
			reconcileIt()
			before := get()

			for _, step := range []time.Duration{0, 10 * time.Second, 40 * time.Second} {
				clock.advance(step)
				reconcileIt()
			}
			after := get()
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "a skipped reconcile writes nothing")
			Expect(tokenCalls).To(Equal(1))
			Expect(puts()).To(Equal(1))

			skipped := linesWith(msgWriteBackingOff)
			Expect(skipped).To(HaveLen(3))
			Expect(skipped[0].kv).To(HaveKeyWithValue("nextAttemptAt", "2026-09-25T10:01:00Z"))
			Expect(skipped[0].kv).To(HaveKeyWithValue("remaining", "1m0s"))
			Expect(skipped[2].kv).To(HaveKeyWithValue("remaining", "10s"))
			Expect(skipped[2].kv).To(HaveKeyWithValue("attempt", "2/5"))
		})

		It("logs 💔 per failure and exactly one stable 🛑 line when it stalls", func() {
			fail(http.MethodPut, busy, 5)
			Expect(reconcileIt()).To(BeZero())

			failed := linesWith(msgWriteFailed)
			Expect(failed).To(HaveLen(4))
			for i, line := range failed {
				Expect(line.err).To(BeTrue())
				Expect(line.kv).To(HaveKeyWithValue("class", errorClassTransient))
				Expect(line.kv).To(HaveKeyWithValue("attempt", []string{"1/5", "2/5", "3/5", "4/5"}[i]))
				Expect(line.kv).To(HaveKey("nextAttemptAt"))
				Expect(line.kv).To(HaveKeyWithValue("operation", "upsert"))
			}

			stalled := linesWith("🛑 APIM write stalled")
			Expect(stalled).To(HaveLen(1))
			Expect(stalled[0].err).To(BeTrue())
			Expect(stalled[0].kv).To(HaveKeyWithValue("kind", "APIMProduct"))
			Expect(stalled[0].kv).To(HaveKeyWithValue("namespace", "default"))
			Expect(stalled[0].kv).To(HaveKeyWithValue("name", peName))
			Expect(stalled[0].kv).To(HaveKeyWithValue("productID", peProductID))
			Expect(stalled[0].kv).To(HaveKeyWithValue("attempts", int32(5)))
			Expect(stalled[0].kv).To(HaveKeyWithValue("lastError", ContainSubstring("503")))

			held := linesWith(msgWriteHeld)
			Expect(held).To(HaveLen(1), "the reconcile after the stall is held")
			Expect(held[0].kv).To(HaveKeyWithValue("attempts", int32(5)))
			Expect(linesWith(msgWriteRejected)).To(BeEmpty())
		})

		It("clears the count on success and starts at 1 again on the next failure", func() {
			fail(http.MethodPut, busy, 2)
			arm.set(http.MethodPut, peOK)
			Expect(reconcileIt()).To(BeZero())
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseCreated))
			Expect(product.Status.RetryStatus).To(BeZero())
			succeeded := linesWith(msgWriteSucceeded)
			Expect(succeeded).To(HaveLen(1))
			Expect(succeeded[0].kv).To(HaveKeyWithValue("attempt", "3/5"))

			arm.set(http.MethodPut, busy)
			Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute), "the backoff starts over")
			Expect(get().Status.ConsecutiveFailures).To(Equal(int32(1)))
		})

		It("treats an unreadable nextAttemptAt as due", func() {
			product := get()
			product.Status.Phase = phaseBackoff
			product.Status.ObservedGeneration = product.Generation
			product.Status.ConsecutiveFailures = 2
			product.Status.NextAttemptAt = "not-a-time"
			Expect(k8sClient.Status().Update(ctx, product)).To(Succeed())

			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(1))
			product = get()
			Expect(product.Status.Phase).To(Equal(phaseCreated))
			Expect(product.Status.RetryStatus).To(BeZero())
		})

		It("does not reset a Stalled product on a metadata-only change", func() {
			fail(http.MethodPut, busy, 5)
			product := get()
			generation := product.Generation
			product.Labels = map[string]string{"team": "pe"}
			Expect(k8sClient.Update(ctx, product)).To(Succeed())
			Expect(get().Generation).To(Equal(generation))

			arm.set(http.MethodPut, peOK)
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(5))
			Expect(get().Status.Phase).To(Equal(phaseStalled))
		})
	})

	Describe("the retry policy", func() {
		DescribeTable("spreads the first wait by the jitter, rounded up to whole seconds",
			func(random float64, want time.Duration) {
				reconciler.retry = &retryPolicy{
					BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5,
					Jitter: 0.2, Now: clock.now, Random: func() float64 { return random },
				}
				arm.set(http.MethodPut, busy)
				Expect(reconcileIt().RequeueAfter).To(Equal(want))
				Expect(get().Status.NextAttemptAt).To(Equal(start.Add(want).Format(time.RFC3339)))
			},
			Entry("lowest: -20 %", 0.0, 48*time.Second),
			Entry("-10 %", 0.25, 54*time.Second),
			Entry("middle: none", 0.5, time.Minute),
			Entry("highest: just under +20 %, rounded up", 0.999, 72*time.Second),
		)

		DescribeTable("caps the wait at MaxDelay and stalls at MaxAttempts",
			func(policy retryPolicy, want []time.Duration) {
				policy.Now = clock.now
				reconciler.retry = &policy
				Expect(fail(http.MethodPut, busy, len(want))).To(Equal(want))
				Expect(puts()).To(Equal(len(want)))
				product := get()
				Expect(product.Status.Phase).To(Equal(phaseStalled))
				Expect(product.Status.ConsecutiveFailures).To(Equal(int32(len(want))))
				Expect(product.Status.Message).To(ContainSubstring("stalled after %d failures", len(want)))
			},
			Entry("capped at 5 minutes over 7 attempts",
				retryPolicy{BaseDelay: time.Minute, MaxDelay: 5 * time.Minute, MaxAttempts: 7},
				[]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 0}),
			Entry("short base, three attempts",
				retryPolicy{BaseDelay: 10 * time.Second, MaxDelay: 30 * time.Minute, MaxAttempts: 3},
				[]time.Duration{10 * time.Second, 20 * time.Second, 0}),
			Entry("one attempt stalls on the first failure",
				retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 1},
				[]time.Duration{0}),
			Entry("production defaults fill a zero policy (no jitter)",
				retryPolicy{},
				[]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}),
		)
	})

	Describe("permanent upsert failures", func() {
		DescribeTable("go to Invalid at once and are not retried",
			func(reply peReply, wantInMessage string) {
				arm.set(http.MethodPut, reply)
				Expect(reconcileIt()).To(BeZero(), "Invalid is not requeued")

				product := get()
				Expect(product.Status.Phase).To(Equal(phaseInvalid))
				Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)))
				Expect(product.Status.NextAttemptAt).To(BeEmpty())
				Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
				Expect(product.Status.Message).To(HavePrefix("Failed to create product in APIM: upsert product " + peProductID))
				Expect(product.Status.Message).To(ContainSubstring(wantInMessage))
				Expect(product.Status.Message).To(ContainSubstring("not retrying"))
				Expect(product.Status.Message).To(ContainSubstring(retryAnnotation))

				rejected := linesWith(msgWriteRejected)
				Expect(rejected).To(HaveLen(1))
				Expect(rejected[0].err).To(BeTrue())
				Expect(rejected[0].kv).To(HaveKeyWithValue("class", errorClassPermanent))
				Expect(rejected[0].kv).To(HaveKeyWithValue("attempt", "1/5"))
				Expect(rejected[0].kv).To(HaveKeyWithValue("operation", "upsert"))
				Expect(linesWith(msgWriteStalled)).To(BeEmpty())
				Expect(linesWith(msgWriteFailed)).To(BeEmpty())

				By("reconciling over the following day")
				arm.set(http.MethodPut, peOK)
				for range 4 {
					clock.advance(6 * time.Hour)
					Expect(reconcileIt()).To(BeZero())
				}
				Expect(puts()).To(Equal(1))
				Expect(tokenCalls).To(Equal(1), "an Invalid product does not even fetch a token")
				Expect(get().Status.Phase).To(Equal(phaseInvalid))
			},
			Entry("400 ValidationError", peARMError(http.StatusBadRequest, "ValidationError", "displayName too long"),
				"displayName too long"),
			Entry("400 with a plain-text body", peReply{status: http.StatusBadRequest, body: "bad request"}, "bad request"),
			Entry("400 whose detail code is not transient",
				peARMErrorWithDetail(http.StatusBadRequest, "ValidationError", "InvalidParameter", "bad field"),
				"ValidationError/InvalidParameter"),
			Entry("401", peARMError(http.StatusUnauthorized, "InvalidAuthenticationToken", "token expired"),
				"InvalidAuthenticationToken"),
			Entry("403", forbidden, "LinkedAuthorizationFailed"),
			Entry("404 on a PUT", peARMError(http.StatusNotFound, "ResourceNotFound", "no such service"), "ResourceNotFound"),
		)

		It("stops a product that was backing off as soon as APIM answers 403", func() {
			fail(http.MethodPut, busy, 3)
			arm.set(http.MethodPut, forbidden)
			Expect(reconcileIt()).To(BeZero())
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseInvalid))
			Expect(product.Status.ConsecutiveFailures).To(Equal(int32(4)))
			Expect(product.Status.NextAttemptAt).To(BeEmpty())
			Expect(puts()).To(Equal(4))
		})
	})

	Describe("a spec change (generation bump)", func() {
		DescribeTable("clears the failures and writes at once",
			func(state string, succeed bool) {
				previous := driveTo(http.MethodPut, state)
				writes := puts()

				if succeed {
					arm.set(http.MethodPut, peOK)
				} else {
					arm.set(http.MethodPut, busy)
				}
				changeSpec("PE Product fixed")
				result := reconcileIt()
				Expect(puts()).To(Equal(writes+1), "no waiting for nextAttemptAt")

				product := get()
				Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
				if succeed {
					Expect(result).To(BeZero())
					Expect(product.Status.Phase).To(Equal(phaseCreated))
					Expect(product.Status.RetryStatus).To(BeZero())
				} else {
					Expect(result.RequeueAfter).To(Equal(time.Minute), "the backoff starts over")
					Expect(product.Status.Phase).To(Equal(phaseBackoff))
					Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)))
				}

				reset := linesWith(msgWriteReset)
				Expect(reset).To(HaveLen(1))
				Expect(reset[0].kv).To(HaveKeyWithValue("reason", "spec changed"))
				Expect(reset[0].kv).To(HaveKeyWithValue("previousFailures", previous))
			},
			Entry("from Backoff, then succeeding", phaseBackoff, true),
			Entry("from Backoff, then failing again", phaseBackoff, false),
			Entry("from Stalled, then succeeding", phaseStalled, true),
			Entry("from Stalled, then failing again", phaseStalled, false),
			Entry("from Invalid, then succeeding", phaseInvalid, true),
			Entry("from Invalid, then failing again", phaseInvalid, false),
		)

		It("resets only once: the next reconcile waits out the new backoff", func() {
			driveTo(http.MethodPut, phaseStalled)
			changeSpec("PE Product fixed")
			Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute))
			writes := puts()

			clock.advance(10 * time.Second)
			Expect(reconcileIt().RequeueAfter).To(Equal(50 * time.Second))
			Expect(puts()).To(Equal(writes))
		})
	})

	Describe("the retry annotation", func() {
		DescribeTable("clears the failures and writes at once",
			func(state string) {
				previous := driveTo(http.MethodPut, state)
				writes := puts()
				generation := get().Generation

				arm.set(http.MethodPut, peOK)
				annotate("try-1")
				Expect(get().Generation).To(Equal(generation), "an annotation is not a spec change")
				Expect(reconcileIt()).To(BeZero())
				Expect(puts()).To(Equal(writes + 1))

				product := get()
				Expect(product.Status.Phase).To(Equal(phaseCreated))
				Expect(product.Status.ConsecutiveFailures).To(BeZero())
				Expect(product.Status.NextAttemptAt).To(BeEmpty())
				Expect(product.Status.LastRetryAnnotation).To(Equal("try-1"))

				reset := linesWith(msgWriteReset)
				Expect(reset).To(HaveLen(1))
				Expect(reset[0].kv).To(HaveKeyWithValue("reason", retryAnnotation+" annotation set"))
				Expect(reset[0].kv).To(HaveKeyWithValue("previousFailures", previous))
			},
			Entry("from Backoff, before nextAttemptAt", phaseBackoff),
			Entry("from Stalled", phaseStalled),
			Entry("from Invalid", phaseInvalid),
		)

		It("fires once per value: the same value never retriggers, a new one does", func() {
			driveTo(http.MethodPut, phaseStalled)
			annotate("1")
			Expect(fail(http.MethodPut, busy, 5)).To(Equal(
				[]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}),
				"the annotation buys a full new set of attempts")
			Expect(puts()).To(Equal(10))
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseStalled))
			Expect(product.Status.LastRetryAnnotation).To(Equal("1"))

			By("reconciling again with the value already handled")
			clock.advance(time.Hour)
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(10))

			By("setting a new value")
			arm.set(http.MethodPut, peOK)
			annotate("2")
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(11))
			product = get()
			Expect(product.Status.Phase).To(Equal(phaseCreated))
			Expect(product.Status.LastRetryAnnotation).To(Equal("2"))
		})

		It("does not retry when the annotation is removed", func() {
			driveTo(http.MethodPut, phaseInvalid)
			annotate("1")
			arm.set(http.MethodPut, forbidden)
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(2))

			product := get()
			delete(product.Annotations, retryAnnotation)
			Expect(k8sClient.Update(ctx, product)).To(Succeed())
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(2))
			product = get()
			Expect(product.Status.Phase).To(Equal(phaseInvalid))
			Expect(product.Status.LastRetryAnnotation).To(Equal("1"), "the last handled value is kept")
		})

		It("records the value on a healthy product's success", func() {
			annotate("first")
			Expect(reconcileIt()).To(BeZero())
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseCreated))
			Expect(product.Status.LastRetryAnnotation).To(Equal("first"))
			Expect(linesWith(msgWriteReset)).To(BeEmpty(), "nothing to clear on a healthy product")
		})

		It("counts a spec change and a new annotation together as one reset and one write", func() {
			driveTo(http.MethodPut, phaseStalled)
			product := get()
			product.Spec.DisplayName = "PE Product both"
			product.Annotations = map[string]string{retryAnnotation: "both"}
			Expect(k8sClient.Update(ctx, product)).To(Succeed())

			arm.set(http.MethodPut, peOK)
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(6))
			Expect(linesWith(msgWriteReset)).To(HaveLen(1))
			product = get()
			Expect(product.Status.LastRetryAnnotation).To(Equal("both"))
			Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
		})
	})

	Context("with deletionPolicy Delete", func() {
		BeforeEach(func() {
			setDeletionPolicy(ctx, key, apimv1.DeletionPolicyDelete)
		})

		DescribeTable("removes the product from APIM and releases the finalizer on success",
			func(reply peReply) {
				deleteResource()
				arm.set(http.MethodDelete, reply)
				Expect(reconcileIt()).To(BeZero())

				Expect(deletes()).To(Equal(1))
				req, _ := arm.last(http.MethodDelete)
				Expect(req.path).To(Equal(peProductPath))
				Expect(req.query).To(ContainSubstring("deleteSubscriptions=true"))
				Expect(req.ifMatch).To(Equal("*"))
				Expect(req.auth).To(Equal("Bearer pe-token"))
				Expect(gone()).To(BeTrue())

				succeeded := linesWith(msgWriteSucceeded)
				Expect(succeeded).To(HaveLen(2), "the upsert, then the delete")
				Expect(succeeded[1].kv).To(HaveKeyWithValue("operation", "delete"))
			},
			Entry("200", peOK),
			Entry("204", peReply{status: http.StatusNoContent}),
			Entry("404: already gone", peARMError(http.StatusNotFound, "ResourceNotFound", "no such product")),
		)

		DescribeTable("backs off transient delete failures, keeps the finalizer, stalls at five",
			func(reply peReply, wantInMessage string) {
				deleteResource()
				arm.set(http.MethodDelete, reply)
				wantWaits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
				for i, want := range wantWaits {
					now := clock.t
					Expect(reconcileIt().RequeueAfter).To(Equal(want))
					product := get()
					Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
					Expect(product.Status.Phase).To(Equal(phaseBackoff))
					Expect(product.Status.ConsecutiveFailures).To(Equal(int32(i + 1)))
					Expect(product.Status.NextAttemptAt).To(Equal(now.Add(want).Format(time.RFC3339)))
					Expect(product.Status.Message).To(HavePrefix("Failed to delete product in APIM: delete product " + peProductID))
					Expect(product.Status.Message).To(ContainSubstring(wantInMessage))
					Expect(product.Status.Message).NotTo(ContainSubstring("deletionPolicy"),
						"a delete that is still being retried needs no way out yet")
					Expect(deletes()).To(Equal(i + 1))

					clock.advance(want - time.Second)
					Expect(reconcileIt().RequeueAfter).To(Equal(time.Second))
					Expect(deletes()).To(Equal(i+1), "no DELETE before nextAttemptAt")
					clock.advance(time.Second)
				}

				Expect(reconcileIt()).To(BeZero())
				product := get()
				Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
				Expect(product.Status.Phase).To(Equal(phaseStalled))
				Expect(product.Status.ConsecutiveFailures).To(Equal(int32(5)))
				Expect(product.Status.NextAttemptAt).To(BeEmpty())
				Expect(product.Status.Message).To(ContainSubstring("stalled after 5 failures"))
				Expect(product.Status.Message).To(ContainSubstring(retryAnnotation))
				Expect(product.Status.Message).To(ContainSubstring("set spec.deletionPolicy to Retain"))
				Expect(deletes()).To(Equal(5))
				Expect(puts()).To(Equal(1))
				stalled := linesWith(msgWriteStalled)
				Expect(stalled).To(HaveLen(1))
				Expect(stalled[0].kv).To(HaveKeyWithValue("operation", "delete"))

				clock.advance(24 * time.Hour)
				Expect(reconcileIt()).To(BeZero())
				Expect(deletes()).To(Equal(5))

				By("the retry annotation lets the delete through once APIM recovers")
				arm.set(http.MethodDelete, peOK)
				annotate("after-incident")
				Expect(reconcileIt()).To(BeZero())
				Expect(deletes()).To(Equal(6))
				Expect(gone()).To(BeTrue())
			},
			Entry("409 Conflict", peARMError(http.StatusConflict, "Conflict", "operation in progress"), "Conflict"),
			Entry("412", peARMError(http.StatusPreconditionFailed, "PreconditionFailed", "etag mismatch"), "PreconditionFailed"),
			Entry("422", peARMError(http.StatusUnprocessableEntity, "ManagementApiRequestFailed", "Management API timed out"),
				"Management API timed out"),
			Entry("429", peARMError(http.StatusTooManyRequests, "TooManyRequests", "slow down"), "TooManyRequests"),
			Entry("500", peARMError(http.StatusInternalServerError, "InternalServerError", "boom"), "boom"),
			Entry("503", busy, "APIM is busy"),
			Entry("400 with Azure code Conflict", peARMError(http.StatusBadRequest, "Conflict", "racing"), "racing"),
		)

		It("releases the finalizer once a backed-off delete succeeds", func() {
			deleteResource()
			fail(http.MethodDelete, busy, 3)
			arm.set(http.MethodDelete, peOK)
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(4))
			Expect(gone()).To(BeTrue())
		})

		DescribeTable("holds a permanently rejected delete as Invalid with the finalizer and a clear status",
			func(reply peReply, wantInMessage string) {
				deleteResource()
				arm.set(http.MethodDelete, reply)
				Expect(reconcileIt()).To(BeZero(), "Invalid waits for a person, not a timer")

				product := get()
				Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue())
				Expect(product.DeletionTimestamp.IsZero()).To(BeFalse())
				Expect(product.Status.Phase).To(Equal(phaseInvalid))
				Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)))
				Expect(product.Status.NextAttemptAt).To(BeEmpty())
				Expect(product.Status.ObservedGeneration).To(Equal(product.Generation))
				Expect(product.Status.Message).To(HavePrefix("Failed to delete product in APIM: delete product " + peProductID))
				Expect(product.Status.Message).To(ContainSubstring(wantInMessage))
				Expect(product.Status.Message).To(ContainSubstring("APIM rejected the write; not retrying"))
				Expect(product.Status.Message).To(ContainSubstring(retryAnnotation))
				Expect(product.Status.Message).To(HaveSuffix(
					"; or set spec.deletionPolicy to Retain to delete the resource and keep the product"))

				rejected := linesWith(msgWriteRejected)
				Expect(rejected).To(HaveLen(1))
				Expect(rejected[0].kv).To(HaveKeyWithValue("operation", "delete"))
				Expect(rejected[0].kv).To(HaveKeyWithValue("class", errorClassPermanent))

				arm.set(http.MethodDelete, peOK)
				for range 3 {
					clock.advance(time.Hour)
					Expect(reconcileIt()).To(BeZero())
				}
				Expect(deletes()).To(Equal(1), "an Invalid delete is not tried again")
				Expect(get().Status.Phase).To(Equal(phaseInvalid))
			},
			Entry("400 ValidationError", peARMError(http.StatusBadRequest, "ValidationError", "has subscriptions"),
				"has subscriptions"),
			Entry("401", peARMError(http.StatusUnauthorized, "InvalidAuthenticationToken", "token expired"),
				"InvalidAuthenticationToken"),
			Entry("403", forbidden, "LinkedAuthorizationFailed"),
		)

		It("retries an Invalid delete after a spec change", func() {
			deleteResource()
			driveTo(http.MethodDelete, phaseInvalid)
			arm.set(http.MethodDelete, peOK)
			changeSpec("PE Product, subscriptions cleaned")
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(2))
			Expect(gone()).To(BeTrue())
		})

		It("gives a product that was backing off on its upsert a fresh, immediate delete", func() {
			driveTo(http.MethodPut, phaseBackoff)
			generation := get().Generation
			Expect(k8sClient.Delete(ctx, get())).To(Succeed())
			Expect(get().Generation).To(BeNumerically(">", generation), "the API server bumps the generation on delete")

			arm.set(http.MethodDelete, busy)
			Expect(reconcileIt().RequeueAfter).To(Equal(time.Minute), "attempt 1 of the delete, not 3")
			product := get()
			Expect(product.Status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(product.Status.Message).To(ContainSubstring("attempt 1/5"))
			Expect(deletes()).To(Equal(1))
		})

		It("gives an Invalid product's delete a fresh attempt", func() {
			driveTo(http.MethodPut, phaseInvalid)
			Expect(k8sClient.Delete(ctx, get())).To(Succeed())
			arm.set(http.MethodDelete, peOK)
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(1))
			Expect(gone()).To(BeTrue())
		})

		It("lets a Stalled delete go without APIM once deletionPolicy is switched to Retain", func() {
			deleteResource()
			driveTo(http.MethodDelete, phaseStalled)
			setDeletionPolicy(ctx, key, apimv1.DeletionPolicyRetain)
			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(5))
			Expect(gone()).To(BeTrue())
		})

		DescribeTable("releases the finalizer without APIM when the APIMService is gone, whatever the retry state",
			func(method, state string) {
				if method == http.MethodDelete {
					deleteResource()
				} else {
					arm.set(http.MethodPut, peOK)
					Expect(reconcileIt()).To(BeZero())
				}
				if state != phaseCreated {
					driveTo(method, state)
				}
				writes, removes, tokens := puts(), deletes(), tokenCalls

				removeService()
				if method == http.MethodPut {
					Expect(k8sClient.Delete(ctx, get())).To(Succeed())
				}
				Expect(reconcileIt()).To(BeZero())

				Expect(gone()).To(BeTrue())
				Expect(puts()).To(Equal(writes))
				Expect(deletes()).To(Equal(removes), "no APIMService, no DELETE")
				Expect(tokenCalls).To(Equal(tokens))
			},
			Entry("a healthy product", http.MethodPut, phaseCreated),
			Entry("an upsert in Backoff", http.MethodPut, phaseBackoff),
			Entry("an upsert Stalled", http.MethodPut, phaseStalled),
			Entry("an upsert Invalid", http.MethodPut, phaseInvalid),
			Entry("a delete in Backoff", http.MethodDelete, phaseBackoff),
			Entry("a delete Stalled", http.MethodDelete, phaseStalled),
			Entry("a delete Invalid", http.MethodDelete, phaseInvalid),
		)

		It("releases the finalizer of a backing-off delete when the Azure identity is gone", func() {
			deleteResource()
			driveTo(http.MethodDelete, phaseBackoff)
			restoreIdentity := unsetAzureIdentityEnvVars()
			defer restoreIdentity()

			Expect(reconcileIt()).To(BeZero())
			Expect(deletes()).To(Equal(2))
			Expect(gone()).To(BeTrue())
		})
	})

	Describe("dependency failures keep today's behaviour", func() {
		It("reports a missing APIMService as Error and requeues without touching the retry state", func() {
			fail(http.MethodPut, busy, 2)
			before := get().Status.RetryStatus

			removeService()
			Expect(reconcileIt().RequeueAfter).To(Equal(requeueMissingAPIMService))
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseError))
			Expect(product.Status.RetryStatus).To(Equal(before))
			Expect(puts()).To(Equal(2))
		})

		It("reports a failed token as Error with a 30 s requeue and counts no APIM failure", func() {
			reconciler.getToken = func(context.Context, string, string) (string, error) {
				return "", context.DeadlineExceeded
			}
			Expect(reconcileIt().RequeueAfter).To(Equal(30 * time.Second))
			product := get()
			Expect(product.Status.Phase).To(Equal(phaseError))
			Expect(product.Status.RetryStatus).To(BeZero())
			Expect(puts()).To(BeZero())
		})

		It("holds a Stalled product after its APIMService comes back, and says so in the phase", func() {
			driveTo(http.MethodPut, phaseStalled)
			removeService()
			Expect(reconcileIt().RequeueAfter).To(Equal(requeueMissingAPIMService))
			Expect(get().Status.Phase).To(Equal(phaseError))

			createService()
			Expect(reconcileIt()).To(BeZero())
			Expect(puts()).To(Equal(5), "the stall survives the dependency blip")
			product := get()
			Expect(product.Status.ConsecutiveFailures).To(Equal(int32(5)))
			// The held reconcile puts Stalled back over the Error the missing-service path
			// wrote, so the phase does not keep claiming the APIMService is missing.
			Expect(product.Status.Phase).To(Equal(phaseStalled),
				"the phase must not claim the APIMService is missing once it is back")
		})
	})
})
