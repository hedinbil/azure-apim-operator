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
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs run the APIMTag write path end to end against an httptest server standing
// in for ARM, with an injected token and a fixed clock, so the whole retry state machine
// (Backoff, Stalled, Invalid, resets) runs in milliseconds.
var _ = Describe("APIMTag Controller retry handling", func() {
	const (
		tagServiceName = "tag-retry-apim-service"
		tagSub         = "00000000-0000-0000-0000-0000000000aa"
		tagRG          = "tag-retry-rg"
	)

	ctx := context.Background()

	// tagARMRequest is one request the fake ARM saw.
	type tagARMRequest struct {
		Method        string
		Path          string
		Query         string
		Authorization string
		IfMatch       string
		Body          string
	}

	// tagARMReply is what the fake ARM answers to one request.
	type tagARMReply struct {
		Status int
		Body   string
	}

	var (
		server   *httptest.Server
		restore  func()
		mu       sync.Mutex
		requests []tagARMRequest
		// replies are served in order; once used up, the last one repeats.
		replies []tagARMReply
		now     time.Time
		tagName types.NamespacedName
	)

	setReplies := func(r ...tagARMReply) {
		mu.Lock()
		defer mu.Unlock()
		replies = r
	}
	callCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(requests)
	}
	lastRequest := func() tagARMRequest {
		mu.Lock()
		defer mu.Unlock()
		Expect(requests).NotTo(BeEmpty())
		return requests[len(requests)-1]
	}

	ok := tagARMReply{Status: http.StatusOK, Body: `{"name":"t"}`}
	azureErr := func(status int, code, message string) tagARMReply {
		return tagARMReply{Status: status, Body: fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)}
	}

	newReconciler := func() *APIMTagReconciler {
		return &APIMTagReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			getToken: func(context.Context, string, string) (string, error) {
				return "fake-token", nil
			},
			retry: &retryPolicy{
				BaseDelay:   time.Minute,
				MaxDelay:    30 * time.Minute,
				MaxAttempts: 5,
				Jitter:      0,
				Now:         func() time.Time { return now },
			},
		}
	}
	reconcileTag := func() ctrl.Result {
		result, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: tagName})
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	getTag := func() *apimv1.APIMTag {
		var tag apimv1.APIMTag
		Expect(k8sClient.Get(ctx, tagName, &tag)).To(Succeed())
		return &tag
	}
	// advancePast moves the fake clock beyond the tag's nextAttemptAt.
	advancePast := func() {
		tag := getTag()
		Expect(tag.Status.NextAttemptAt).NotTo(BeEmpty())
		next, err := time.Parse(time.RFC3339, tag.Status.NextAttemptAt)
		Expect(err).NotTo(HaveOccurred())
		now = next.Add(time.Second)
	}
	setAnnotation := func(value string) {
		tag := getTag()
		patch := client.MergeFrom(tag.DeepCopy())
		if tag.Annotations == nil {
			tag.Annotations = map[string]string{}
		}
		tag.Annotations[retryAnnotation] = value
		Expect(k8sClient.Patch(ctx, tag, patch)).To(Succeed())
	}
	setDisplayName := func(value string) {
		tag := getTag()
		patch := client.MergeFrom(tag.DeepCopy())
		tag.Spec.DisplayName = value
		Expect(k8sClient.Patch(ctx, tag, patch)).To(Succeed())
	}
	// failUntilStalled reconciles through five transient failures, moving the clock past
	// each backoff, and leaves the tag Stalled.
	failUntilStalled := func() {
		setReplies(azureErr(http.StatusServiceUnavailable, "ServiceUnavailable", "busy"))
		for i := 1; i <= 5; i++ {
			if i > 1 {
				advancePast()
			}
			reconcileTag()
			Expect(callCount()).To(Equal(i))
		}
		Expect(getTag().Status.Phase).To(Equal(phaseStalled))
	}

	BeforeEach(func() {
		mu.Lock()
		requests = nil
		replies = []tagARMReply{ok}
		mu.Unlock()
		now = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			requests = append(requests, tagARMRequest{
				Method:        r.Method,
				Path:          r.URL.Path,
				Query:         r.URL.RawQuery,
				Authorization: r.Header.Get("Authorization"),
				IfMatch:       r.Header.Get("If-Match"),
				Body:          string(body),
			})
			idx := len(requests) - 1
			if idx >= len(replies) {
				idx = len(replies) - 1
			}
			reply := replies[idx]
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(reply.Status)
			_, _ = w.Write([]byte(reply.Body))
		}))
		restore = apim.UseEndpoint(server.URL, server.Client())

		// The controller still requires the identity env vars before it writes.
		for key, value := range map[string]string{"AZURE_CLIENT_ID": "fake-client", "AZURE_TENANT_ID": "fake-tenant"} {
			previous, had := os.LookupEnv(key)
			Expect(os.Setenv(key, value)).To(Succeed())
			DeferCleanup(func() {
				if had {
					Expect(os.Setenv(key, previous)).To(Succeed())
				} else {
					Expect(os.Unsetenv(key)).To(Succeed())
				}
			})
		}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: tagServiceName, Namespace: "default"},
			Spec: apimv1.APIMServiceSpec{
				Name:          "tag-retry-apim",
				ResourceGroup: tagRG,
				Subscription:  tagSub,
			},
		}))).To(Succeed())

		tagName = types.NamespacedName{Name: fmt.Sprintf("tag-retry-%d", time.Now().UnixNano()), Namespace: "default"}
		Expect(k8sClient.Create(ctx, &apimv1.APIMTag{
			ObjectMeta: metav1.ObjectMeta{Name: tagName.Name, Namespace: tagName.Namespace},
			Spec: apimv1.APIMTagSpec{
				APIMService: tagServiceName,
				TagID:       "retry-tag",
				DisplayName: "Retry Tag",
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		restore()
		server.Close()
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &apimv1.APIMTag{
			ObjectMeta: metav1.ObjectMeta{Name: tagName.Name, Namespace: tagName.Namespace},
		}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: tagServiceName, Namespace: "default"},
		}))).To(Succeed())
	})

	It("upserts the tag through ARM and records success", func() {
		result := reconcileTag()
		Expect(result).To(Equal(ctrl.Result{}))

		By("sending one PUT to the tag's ARM path with the injected token")
		Expect(callCount()).To(Equal(1))
		req := lastRequest()
		Expect(req.Method).To(Equal(http.MethodPut))
		Expect(req.Path).To(Equal("/subscriptions/" + tagSub + "/resourceGroups/" + tagRG +
			"/providers/Microsoft.ApiManagement/service/" + tagServiceName + "/tags/retry-tag"))
		Expect(req.Query).To(Equal("api-version=2021-08-01"))
		Expect(req.Authorization).To(Equal("Bearer fake-token"))
		Expect(req.IfMatch).To(Equal("*"))
		var body map[string]map[string]string
		Expect(json.Unmarshal([]byte(req.Body), &body)).To(Succeed())
		Expect(body["properties"]["displayName"]).To(Equal("Retry Tag"))

		By("persisting Created with observedGeneration and no retry state")
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseCreated))
		Expect(tag.Status.Message).To(Equal("Tag created or updated"))
		Expect(tag.Status.ObservedGeneration).To(Equal(tag.Generation))
		Expect(tag.Status.ConsecutiveFailures).To(BeZero())
		Expect(tag.Status.NextAttemptAt).To(BeEmpty())
	})

	It("upserts again on a resync after a success, as today", func() {
		reconcileTag()
		Expect(callCount()).To(Equal(1))
		// A resync of an unchanged, Created tag still upserts (idempotent PUT), as today.
		reconcileTag()
		Expect(callCount()).To(Equal(2))
		Expect(getTag().Status.Phase).To(Equal(phaseCreated))
	})

	It("clears earlier failures on success", func() {
		setReplies(azureErr(http.StatusTooManyRequests, "TooManyRequests", "slow down"), ok)
		reconcileTag()
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseBackoff))
		Expect(tag.Status.ConsecutiveFailures).To(Equal(int32(1)))

		advancePast()
		Expect(reconcileTag()).To(Equal(ctrl.Result{}))
		tag = getTag()
		Expect(callCount()).To(Equal(2))
		Expect(tag.Status.Phase).To(Equal(phaseCreated))
		Expect(tag.Status.ConsecutiveFailures).To(BeZero())
		Expect(tag.Status.NextAttemptAt).To(BeEmpty())
		Expect(tag.Status.ObservedGeneration).To(Equal(tag.Generation))
	})

	It("backs off after a transient failure and records nextAttemptAt", func() {
		setReplies(azureErr(http.StatusPreconditionFailed, "PreconditionFailed", "etag mismatch"))
		result := reconcileTag()

		Expect(result.RequeueAfter).To(Equal(time.Minute))
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseBackoff))
		Expect(tag.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(tag.Status.NextAttemptAt).To(Equal(now.Add(time.Minute).Format(time.RFC3339)))
		Expect(tag.Status.ObservedGeneration).To(Equal(tag.Generation))
		Expect(tag.Status.Message).To(ContainSubstring("transient, attempt 1/5"))
		Expect(tag.Status.Message).To(ContainSubstring("PreconditionFailed"))
	})

	It("doubles the backoff with every further failure", func() {
		setReplies(azureErr(http.StatusInternalServerError, "InternalServerError", "boom"))
		Expect(reconcileTag().RequeueAfter).To(Equal(time.Minute))
		advancePast()
		Expect(reconcileTag().RequeueAfter).To(Equal(2 * time.Minute))
		advancePast()
		Expect(reconcileTag().RequeueAfter).To(Equal(4 * time.Minute))
		Expect(getTag().Status.ConsecutiveFailures).To(Equal(int32(3)))
	})

	It("does not call APIM when reconciled before nextAttemptAt", func() {
		setReplies(azureErr(http.StatusConflict, "Conflict", "busy"))
		reconcileTag()
		Expect(callCount()).To(Equal(1))
		before := getTag().Status

		By("reconciling 20 s later, still inside the 1 min backoff")
		now = now.Add(20 * time.Second)
		result := reconcileTag()
		Expect(callCount()).To(Equal(1))
		Expect(result.RequeueAfter).To(Equal(40 * time.Second))

		By("leaving the status untouched")
		Expect(getTag().Status).To(Equal(before))
	})

	It("does not even fetch a token while backing off", func() {
		setReplies(azureErr(http.StatusServiceUnavailable, "", "busy"))
		reconcileTag()

		tokenCalls := 0
		r := newReconciler()
		r.getToken = func(context.Context, string, string) (string, error) {
			tokenCalls++
			return "fake-token", nil
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: tagName})
		Expect(err).NotTo(HaveOccurred())
		Expect(tokenCalls).To(BeZero())
		Expect(callCount()).To(Equal(1))
	})

	It("stalls after five transient failures and stops calling APIM", func() {
		failUntilStalled()
		tag := getTag()
		Expect(tag.Status.ConsecutiveFailures).To(Equal(int32(5)))
		Expect(tag.Status.NextAttemptAt).To(BeEmpty())
		Expect(tag.Status.Message).To(ContainSubstring("stalled after 5 failures"))

		By("reconciling again, an hour later, without calling APIM or requeueing")
		now = now.Add(time.Hour)
		for range 3 {
			Expect(reconcileTag()).To(Equal(ctrl.Result{}))
		}
		Expect(callCount()).To(Equal(5))
		Expect(getTag().Status.Phase).To(Equal(phaseStalled))
	})

	It("stalls on the fifth failure even when the failures differ", func() {
		setReplies(
			azureErr(http.StatusConflict, "Conflict", "a"),
			azureErr(http.StatusPreconditionFailed, "PreconditionFailed", "b"),
			azureErr(http.StatusUnprocessableEntity, "ManagementApiRequestFailed", "Management API timed out"),
			azureErr(http.StatusTooManyRequests, "", "c"),
			azureErr(http.StatusBadGateway, "", "d"),
		)
		for i := 1; i <= 5; i++ {
			if i > 1 {
				advancePast()
			}
			reconcileTag()
		}
		Expect(callCount()).To(Equal(5))
		Expect(getTag().Status.Phase).To(Equal(phaseStalled))
	})

	It("treats a transport failure as transient", func() {
		// Point ARM at a server that is already closed, so the connection is refused.
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL
		dead.Close()
		restoreDead := apim.UseEndpoint(deadURL, http.DefaultClient)
		defer restoreDead()

		result := reconcileTag()
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseBackoff))
		Expect(tag.Status.ConsecutiveFailures).To(Equal(int32(1)))
	})

	DescribeTable("marks the tag Invalid on a permanent error without retrying",
		func(status int, code string) {
			setReplies(azureErr(status, code, "rejected"))
			result := reconcileTag()
			Expect(result).To(Equal(ctrl.Result{}))
			tag := getTag()
			Expect(tag.Status.Phase).To(Equal(phaseInvalid))
			Expect(tag.Status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(tag.Status.NextAttemptAt).To(BeEmpty())
			Expect(tag.Status.Message).To(ContainSubstring("not retrying"))

			By("reconciling later without calling APIM")
			now = now.Add(24 * time.Hour)
			Expect(reconcileTag()).To(Equal(ctrl.Result{}))
			Expect(callCount()).To(Equal(1))
		},
		Entry("400 ValidationError", http.StatusBadRequest, "ValidationError"),
		Entry("401 Unauthorized", http.StatusUnauthorized, "InvalidAuthenticationToken"),
		Entry("403 AuthorizationFailed", http.StatusForbidden, "AuthorizationFailed"),
		Entry("404 on PUT", http.StatusNotFound, "ResourceNotFound"),
	)

	DescribeTable("backs off on a transient error",
		func(status int, code string) {
			setReplies(azureErr(status, code, "try later"))
			result := reconcileTag()
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(getTag().Status.Phase).To(Equal(phaseBackoff))
		},
		Entry("409 Conflict", http.StatusConflict, "Conflict"),
		Entry("412 PreconditionFailed", http.StatusPreconditionFailed, "PreconditionFailed"),
		Entry("422 Management API timed out", http.StatusUnprocessableEntity, "ManagementApiRequestFailed"),
		Entry("429 throttled", http.StatusTooManyRequests, "TooManyRequests"),
		Entry("500", http.StatusInternalServerError, "InternalServerError"),
		Entry("503", http.StatusServiceUnavailable, ""),
		Entry("400 with a transient Azure code", http.StatusBadRequest, "PreconditionFailed"),
	)

	It("resets on a spec change while Stalled", func() {
		failUntilStalled()

		setReplies(ok)
		setDisplayName("Renamed Tag")
		Expect(reconcileTag()).To(Equal(ctrl.Result{}))

		Expect(callCount()).To(Equal(6))
		var body map[string]map[string]string
		Expect(json.Unmarshal([]byte(lastRequest().Body), &body)).To(Succeed())
		Expect(body["properties"]["displayName"]).To(Equal("Renamed Tag"))
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseCreated))
		Expect(tag.Status.ConsecutiveFailures).To(BeZero())
		Expect(tag.Status.ObservedGeneration).To(Equal(tag.Generation))
	})

	It("resets on a spec change while backing off, without waiting for nextAttemptAt", func() {
		setReplies(azureErr(http.StatusServiceUnavailable, "", "busy"))
		reconcileTag()
		reconcileTag()
		Expect(callCount()).To(Equal(1))

		setDisplayName("Renamed While Backing Off")
		result := reconcileTag()
		Expect(callCount()).To(Equal(2))
		By("counting the new spec's failure as its first")
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(getTag().Status.ConsecutiveFailures).To(Equal(int32(1)))
	})

	It("resets on a spec change while Invalid", func() {
		setReplies(azureErr(http.StatusBadRequest, "ValidationError", "bad display name"), ok)
		reconcileTag()
		Expect(getTag().Status.Phase).To(Equal(phaseInvalid))

		setDisplayName("Fixed Name")
		reconcileTag()
		Expect(callCount()).To(Equal(2))
		Expect(getTag().Status.Phase).To(Equal(phaseCreated))
	})

	It("resumes a Stalled tag when the retry annotation is set, once per value", func() {
		failUntilStalled()

		By("setting the annotation: one fresh attempt, counted as the first")
		setAnnotation("1")
		result := reconcileTag()
		Expect(callCount()).To(Equal(6))
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseBackoff))
		Expect(tag.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(tag.Status.LastRetryAnnotation).To(Equal("1"))

		By("succeeding on the next attempt")
		setReplies(ok)
		advancePast()
		reconcileTag()
		tag = getTag()
		Expect(callCount()).To(Equal(7))
		Expect(tag.Status.Phase).To(Equal(phaseCreated))
		Expect(tag.Status.ConsecutiveFailures).To(BeZero())
		Expect(tag.Status.LastRetryAnnotation).To(Equal("1"))
	})

	It("does not retrigger an Invalid tag on the same annotation value", func() {
		setReplies(azureErr(http.StatusForbidden, "AuthorizationFailed", "no access"))
		reconcileTag()
		Expect(getTag().Status.Phase).To(Equal(phaseInvalid))

		By("setting the annotation once: one more attempt, still rejected")
		setAnnotation("first")
		reconcileTag()
		Expect(callCount()).To(Equal(2))
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseInvalid))
		Expect(tag.Status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(tag.Status.LastRetryAnnotation).To(Equal("first"))

		By("reconciling again with the same value: no call")
		reconcileTag()
		reconcileTag()
		Expect(callCount()).To(Equal(2))

		By("changing the value: one more attempt")
		setReplies(ok)
		setAnnotation("second")
		reconcileTag()
		Expect(callCount()).To(Equal(3))
		tag = getTag()
		Expect(tag.Status.Phase).To(Equal(phaseCreated))
		Expect(tag.Status.LastRetryAnnotation).To(Equal("second"))
	})

	It("records a retry annotation set on a healthy tag without retriggering later", func() {
		setAnnotation("noop")
		reconcileTag()
		Expect(getTag().Status.LastRetryAnnotation).To(Equal("noop"))

		By("failing afterwards backs off as usual; the old annotation does not bypass it")
		setReplies(azureErr(http.StatusServiceUnavailable, "", "busy"))
		reconcileTag()
		Expect(callCount()).To(Equal(2))
		reconcileTag()
		Expect(callCount()).To(Equal(2))
		Expect(getTag().Status.Phase).To(Equal(phaseBackoff))
	})

	It("logs the stable stalled line the Datadog monitor matches", func() {
		var linesMu sync.Mutex
		var lines []string
		sink := funcr.New(func(prefix, args string) {
			linesMu.Lock()
			defer linesMu.Unlock()
			lines = append(lines, args)
		}, funcr.Options{})
		logCtx := logf.IntoContext(ctx, sink)

		setReplies(azureErr(http.StatusServiceUnavailable, "ServiceUnavailable", "busy"))
		for i := 1; i <= 5; i++ {
			if i > 1 {
				advancePast()
			}
			_, err := newReconciler().Reconcile(logCtx, reconcile.Request{NamespacedName: tagName})
			Expect(err).NotTo(HaveOccurred())
		}

		linesMu.Lock()
		defer linesMu.Unlock()
		find := func(msg string) []string {
			var out []string
			for _, l := range lines {
				if strings.Contains(l, `"msg"=`+fmt.Sprintf("%q", msg)) {
					out = append(out, l)
				}
			}
			return out
		}
		Expect(find(msgWriteStarting)).To(HaveLen(5))
		Expect(find(msgWriteFailed)).To(HaveLen(4))
		stalled := find(msgWriteStalled)
		Expect(stalled).To(HaveLen(1))
		for _, kv := range []string{
			`"kind"="APIMTag"`, `"namespace"="default"`, `"name"="` + tagName.Name + `"`,
			`"tagID"="retry-tag"`, `"attempts"=5`, `"lastError"=`,
		} {
			Expect(stalled[0]).To(ContainSubstring(kv))
		}

		By("logging the hold, not another write, on a later reconcile")
		lines = nil
		linesMu.Unlock()
		_, err := newReconciler().Reconcile(logCtx, reconcile.Request{NamespacedName: tagName})
		linesMu.Lock()
		Expect(err).NotTo(HaveOccurred())
		Expect(find(msgWriteHeld)).To(HaveLen(1))
		Expect(find(msgWriteStarting)).To(BeEmpty())
	})

	It("keeps the token-failure path outside the retry state machine", func() {
		r := newReconciler()
		r.getToken = func(context.Context, string, string) (string, error) {
			return "", fmt.Errorf("no token")
		}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: tagName})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		tag := getTag()
		Expect(tag.Status.Phase).To(Equal(phaseError))
		Expect(tag.Status.ConsecutiveFailures).To(BeZero())
		Expect(callCount()).To(BeZero())
	})
})
