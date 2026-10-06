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
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs run APIMAPIDeployment end to end in envtest against a fake ARM that counts
// every request by step and path. They pin down the step 1 ("bound the damage") retry
// design from the 25-28 Sep 2026 import loop: one import per applied hash, Backoff with
// a nextAttemptAt and no ARM call before it, Stalled after exactly five transient
// failures, Invalid at once on a permanent error, and a reset on a new desired hash or a
// new value of the apim.operator.io/retry annotation. Most specs call Reconcile directly
// on a fake clock; the last group runs the controller in a real manager, where requeues,
// predicates and the workqueue decide when it runs.

// Messages the deployment writes into status.message, before the retry part.
const (
	depEnvMsgImport       = "Failed to import API into APIM"
	depEnvMsgWebSocket    = "Failed to create WebSocket API in APIM"
	depEnvMsgServiceURL   = "Failed to patch service URL in APIM"
	depEnvMsgSubscription = "Failed to patch subscription requirement in APIM"
	depEnvMsgProducts     = "Failed to assign API to products"
	depEnvMsgTags         = "Failed to assign API to tags"
	depEnvMsgDetails      = "Failed to fetch APIM service details"
)

var _ = Describe("APIMAPIDeployment end to end against a fake ARM", func() {

	Context("with a healthy APIM", func() {
		It("imports once, runs every step once per product and tag, and records the applied hash", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1", "p2"}, tagIDs: []string{"t1", "t2"}})

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.counts()).To(Equal(map[string]int{
				depEnvGetAPI:               1,
				depEnvImport:               1,
				depEnvServiceURL:           1,
				depEnvSubscriptionRequired: 1,
				depEnvProduct:              2,
				depEnvTag:                  2,
				depEnvServiceDetails:       1,
			}))
			Expect(f.arm.countPath(http.MethodPut, f.rel(""))).To(Equal(1), "exactly one import PUT")
			Expect(f.arm.countPath(http.MethodPatch, f.rel(""))).To(Equal(2), "serviceUrl and subscriptionRequired")
			Expect(f.arm.countPath(http.MethodPut, "/products/p1/apis/"+f.apiID)).To(Equal(1))
			Expect(f.arm.countPath(http.MethodPut, "/products/p2/apis/"+f.apiID)).To(Equal(1))
			Expect(f.arm.countPath(http.MethodPut, f.rel("/tags/t1"))).To(Equal(1))
			Expect(f.arm.countPath(http.MethodPut, f.rel("/tags/t2"))).To(Equal(1))
			Expect(f.tokenCalls.Load()).To(Equal(int32(1)))
			Expect(f.doc.fetches()).To(Equal(1))

			By("sending the document as an OpenAPI import with If-Match * for a new API")
			imp := f.arm.last(depEnvImport)
			Expect(imp.Body).To(Equal(depEnvDocV1))
			Expect(imp.Query.Get("import")).To(Equal("true"))
			Expect(imp.Query.Get("path")).To(Equal(depEnvRoutePrefix))
			Expect(imp.Query.Get("api-version")).To(Equal("2021-08-01"))
			Expect(imp.Query.Has("createRevision")).To(BeFalse())
			Expect(imp.Header.Get("If-Match")).To(Equal("*"))
			Expect(imp.Header.Get("Content-Type")).To(Equal("application/vnd.oai.openapi+json"))
			Expect(imp.Header.Get("Authorization")).To(Equal("Bearer " + depEnvToken))
			Expect(f.arm.last(depEnvServiceURL).Body).To(ContainSubstring(depEnvBackend))
			Expect(f.arm.last(depEnvSubscriptionRequired).Body).To(ContainSubstring("true"))

			By("recording the outcome in the status")
			want := f.desiredHash(depEnvDocV1)
			status := f.get().Status
			Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(status.Status).To(Equal("OK"))
			Expect(status.Message).To(Equal("Successfully reconciled API in APIM"))
			Expect(status.LastError).To(BeEmpty())
			Expect(status.DesiredHash).To(Equal(want))
			Expect(status.AppliedHash).To(Equal(want))
			Expect(status.OpenAPIHash).To(Equal(sha256Hex([]byte(depEnvDocV1))))
			Expect(status.MatchedReplicaSets).To(Equal([]string{f.rsName}))
			Expect(status.ObservedGeneration).To(Equal(f.getAPI().Generation))
			Expect(time.Parse(time.RFC3339, status.ImportedAt)).NotTo(BeZero())
			Expect(time.Parse(time.RFC3339, status.LastAttemptAt)).NotTo(BeZero())
			Expect(status.RetryStatus).To(Equal(apimv1.RetryStatus{}), "no failures, no next attempt, no annotation seen")

			api := f.getAPI()
			Expect(api.Status.Status).To(Equal("OK"))
			Expect(api.Status.ApiHost).To(Equal("https://gw.depenv.net" + depEnvRoutePrefix))
			Expect(api.Status.DeveloperPortalHost).To(Equal("https://portal.depenv.net"))
		})

		It("does nothing in APIM on later reconciles with the same hash", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}, tagIDs: []string{"t1"}})
			Expect(f.reconcile()).To(BeZero())
			applied := f.get().Status.AppliedHash
			calls, tokens := f.arm.total(), f.tokenCalls.Load()

			By("reconciling ten more times, with the clock moving on")
			for i := 0; i < 10; i++ {
				f.clock.advance(time.Hour)
				Expect(f.reconcile()).To(BeZero())
			}

			Expect(f.arm.total()).To(Equal(calls), "an applied hash is never imported again")
			Expect(f.tokenCalls.Load()).To(Equal(tokens), "no token is needed when nothing is written")
			Expect(f.doc.fetches()).To(Equal(11), "the document is still fetched to detect changes")
			status := f.get().Status
			Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(status.Message).To(Equal("No changes detected; APIM is already in sync"))
			Expect(status.AppliedHash).To(Equal(applied))
			Expect(status.RetryStatus).To(Equal(apimv1.RetryStatus{}))
		})

		It("updates an existing API conditionally on its ETag", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvGetAPI, func(w http.ResponseWriter, _ *http.Request) bool {
				w.Header().Set("ETag", `W/"etag-1"`)
				depEnvWriteJSON(w, http.StatusOK, `{"name":"api"}`)
				return true
			})

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.last(depEnvImport).Header.Get("If-Match")).To(Equal(`"etag-1"`))
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})

		It("still imports with If-Match * when reading the existing API fails", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvGetAPI, depEnvFail(http.StatusInternalServerError, "InternalServerError"))

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.count(depEnvImport)).To(Equal(1))
			Expect(f.arm.last(depEnvImport).Header.Get("If-Match")).To(Equal("*"))
			status := f.get().Status
			Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(status.ConsecutiveFailures).To(BeZero(), "the read is advisory and is not a failed write")
		})

		It("creates a revision without reading the API first", func() {
			f := newDepEnvFixture(depEnvOptions{revision: "3"})

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.count(depEnvGetAPI)).To(BeZero())
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
			Expect(f.arm.countPath(http.MethodPut, f.rel(";rev=3"))).To(Equal(1))
			imp := f.arm.last(depEnvImport)
			Expect(imp.Query.Get("createRevision")).To(Equal("true"))
			Expect(imp.Header.Get("If-Match")).To(Equal("*"))
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})

		It("skips the product and tag steps when none are configured", func() {
			f := newDepEnvFixture(depEnvOptions{})

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.stepsSince(0)).To(Equal([]string{
				depEnvGetAPI, depEnvImport, depEnvServiceURL, depEnvSubscriptionRequired, depEnvServiceDetails,
			}))
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})

		DescribeTable("an accepted (202) import that completes counts as one successful import",
			func(retryAfter string, pollStatus int, pollBody string) {
				f := newDepEnvFixture(depEnvOptions{})
				depEnvAsync(200*time.Millisecond, time.Millisecond)
				f.arm.on(depEnvImport, depEnvAccepted(retryAfter))
				f.arm.on(depEnvPoll, depEnvPollAnswer(pollStatus, pollBody))

				Expect(f.reconcile()).To(BeZero())

				Expect(f.arm.count(depEnvImport)).To(Equal(1))
				Expect(f.arm.count(depEnvPoll)).To(Equal(1))
				Expect(f.arm.count(depEnvServiceURL)).To(Equal(1), "the chain continues after the import completes")
				status := f.get().Status
				Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
				Expect(status.AppliedHash).To(Equal(status.DesiredHash))
			},
			Entry("status Succeeded", "", http.StatusOK, `{"status":"Succeeded"}`),
			Entry("provisioningState Succeeded", "", http.StatusOK, `{"properties":{"provisioningState":"Succeeded"}}`),
			Entry("terminal HTTP status without a status field", "", http.StatusOK, `{}`),
			// Retry-After is bounded by the remaining wait (200 ms here), so a large value
			// must not hold the reconcile for an hour; the spec finishing proves it.
			Entry("Retry-After of an hour, bounded by the wait", "3600", http.StatusOK, `{"status":"Succeeded"}`),
		)
	})

	Context("with an import APIM accepts but does not finish", func() {
		It("backs off when the async wait times out", func() {
			f := newDepEnvFixture(depEnvOptions{})
			depEnvAsync(40*time.Millisecond, 2*time.Millisecond)
			f.arm.on(depEnvImport, depEnvAccepted(""))
			f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusAccepted, `{"status":"InProgress"}`))

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			Expect(f.arm.count(depEnvImport)).To(Equal(1))
			Expect(f.arm.count(depEnvPoll)).To(BeNumerically(">=", 1))
			Expect(f.arm.count(depEnvServiceURL)).To(BeZero(), "nothing after the import may run")
			status := f.get().Status
			Expect(status.Phase).To(Equal(phaseBackoff))
			Expect(status.Status).To(Equal(phaseError))
			Expect(status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(status.NextAttemptAt).To(Equal(depEnvAt(time.Minute)))
			Expect(status.Message).To(Equal(depEnvMsgImport + ": APIM write failed (transient, attempt 1/5); next attempt at " +
				depEnvAt(time.Minute)))
			Expect(status.LastError).To(ContainSubstring(apim.ErrImportWaitTimeout.Error()))
			Expect(status.DesiredHash).To(Equal(f.desiredHash(depEnvDocV1)))
			Expect(status.AppliedHash).To(BeEmpty())
		})

		It("stalls after exactly five import PUTs and never calls ARM again", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}})
			depEnvAsync(20*time.Millisecond, 2*time.Millisecond)
			f.arm.on(depEnvImport, depEnvAccepted(""))
			f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusAccepted, `{"status":"InProgress"}`))

			requeues := f.drive(12)

			Expect(requeues).To(Equal([]time.Duration{
				time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0, 0, 0, 0, 0, 0, 0, 0,
			}))
			Expect(f.arm.count(depEnvImport)).To(Equal(5), "exactly five import PUTs")
			Expect(f.arm.countPath(http.MethodPut, f.rel(""))).To(Equal(5))
			Expect(f.arm.count(depEnvGetAPI)).To(Equal(5))
			Expect(f.arm.count(depEnvServiceURL)).To(BeZero())
			Expect(f.tokenCalls.Load()).To(Equal(int32(5)))
			status := f.get().Status
			Expect(status.Phase).To(Equal(phaseStalled))
			Expect(status.Status).To(Equal(phaseError))
			Expect(status.ConsecutiveFailures).To(Equal(int32(5)))
			Expect(status.NextAttemptAt).To(BeEmpty())
			Expect(status.Message).To(Equal(depEnvMsgImport + ": APIM write stalled after 5 failures in a row; " +
				"not retrying until the spec changes or the apim.operator.io/retry annotation is set"))
			Expect(status.LastError).To(ContainSubstring(apim.ErrImportWaitTimeout.Error()))
			Expect(status.AppliedHash).To(BeEmpty())
		})

		It("makes zero ARM calls while Stalled with an unchanged hash, however often and late it reconciles", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}, tagIDs: []string{"t1"}})
			f.stall()
			calls := f.arm.total()

			for i := 0; i < 25; i++ {
				f.clock.advance(time.Duration(i+1) * time.Hour)
				Expect(f.reconcileWithoutAPIM()).To(BeZero(), "Stalled is not requeued")
			}

			Expect(f.arm.total()).To(Equal(calls))
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))
		})

		DescribeTable("an async operation that fails is a transient failure",
			func(pollStatus int, pollBody string, wantInError string) {
				f := newDepEnvFixture(depEnvOptions{})
				f.arm.on(depEnvImport, depEnvAccepted(""))
				f.arm.on(depEnvPoll, depEnvPollAnswer(pollStatus, pollBody))

				Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

				status := f.get().Status
				Expect(status.Phase).To(Equal(phaseBackoff))
				Expect(status.ConsecutiveFailures).To(Equal(int32(1)))
				Expect(status.LastError).To(ContainSubstring(wantInError))
				Expect(f.arm.count(depEnvImport)).To(Equal(1))
				Expect(f.arm.count(depEnvServiceURL)).To(BeZero())
			},
			Entry("Failed with DeadOperationMonitor, as in Sep 2026", http.StatusOK,
				`{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor"}}`, "DeadOperationMonitor"),
			Entry("Canceled", http.StatusOK, `{"status":"Canceled"}`, "Canceled"),
			Entry("Failed with a code that is not on the transient list", http.StatusOK,
				`{"status":"Failed","error":{"code":"ValidationError","message":"bad"}}`, "ValidationError"),
			Entry("the poll itself answers 500", http.StatusInternalServerError,
				`{"error":{"code":"InternalServerError","message":"poll broke"}}`, "poll broke"),
			Entry("the poll itself answers 404 (a read)", http.StatusNotFound,
				`{"error":{"code":"ResourceNotFound","message":"operation gone"}}`, "operation gone"),
		)

		DescribeTable("a backing-off deployment does not call ARM before nextAttemptAt, to the second",
			func(elapsed time.Duration, wantRequeue time.Duration) {
				f := newDepEnvFixture(depEnvOptions{})
				f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))
				Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

				f.clock.advance(elapsed)
				if wantRequeue > 0 {
					Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: wantRequeue}))
					Expect(f.get().Status.Phase).To(Equal(phaseBackoff))
					return
				}
				calls := f.arm.count(depEnvImport)
				Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
				Expect(f.arm.count(depEnvImport)).To(Equal(calls + 1))
				Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(2)))
			},
			Entry("at once", time.Duration(0), time.Minute),
			Entry("after 1 s", time.Second, 59*time.Second),
			Entry("after 30 s", 30*time.Second, 30*time.Second),
			Entry("after 59 s", 59*time.Second, time.Second),
			Entry("after 59.5 s", 59*time.Second+500*time.Millisecond, 500*time.Millisecond),
			Entry("exactly at nextAttemptAt", time.Minute, time.Duration(0)),
			Entry("long after nextAttemptAt", 3*time.Hour, time.Duration(0)),
		)
	})

	Context("with permanent and transient HTTP errors on the import", func() {
		DescribeTable("a permanent error makes it Invalid after exactly one PUT",
			func(status int, code string) {
				f := newDepEnvFixture(depEnvOptions{})
				f.arm.on(depEnvImport, depEnvFail(status, code))

				Expect(f.reconcile()).To(BeZero(), "Invalid is not requeued")

				st := f.get().Status
				Expect(st.Phase).To(Equal(phaseInvalid))
				Expect(st.Status).To(Equal(phaseError))
				Expect(st.ConsecutiveFailures).To(Equal(int32(1)))
				Expect(st.NextAttemptAt).To(BeEmpty())
				Expect(st.Message).To(Equal(depEnvMsgImport + ": APIM rejected the write; not retrying until the spec " +
					"changes or the apim.operator.io/retry annotation is set"))
				Expect(st.LastError).To(ContainSubstring(fmt.Sprint(status)))
				Expect(st.LastError).To(ContainSubstring(code))

				By("reconciling for days without a change")
				for i := 0; i < 10; i++ {
					f.clock.advance(12 * time.Hour)
					Expect(f.reconcileWithoutAPIM()).To(BeZero())
				}
				Expect(f.arm.count(depEnvImport)).To(Equal(1), "zero retries")
			},
			Entry("400 ValidationError", http.StatusBadRequest, "ValidationError"),
			Entry("401 InvalidAuthenticationToken", http.StatusUnauthorized, "InvalidAuthenticationToken"),
			Entry("403 AuthorizationFailed", http.StatusForbidden, "AuthorizationFailed"),
			Entry("404 on the import PUT", http.StatusNotFound, "ResourceNotFound"),
		)

		DescribeTable("a transient error backs off for a minute",
			func(responder depEnvResponder, wantInError string) {
				f := newDepEnvFixture(depEnvOptions{})
				f.arm.on(depEnvImport, responder)

				Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

				st := f.get().Status
				Expect(st.Phase).To(Equal(phaseBackoff))
				Expect(st.ConsecutiveFailures).To(Equal(int32(1)))
				Expect(st.NextAttemptAt).To(Equal(depEnvAt(time.Minute)))
				Expect(st.Message).To(HavePrefix(depEnvMsgImport + ": APIM write failed (transient, attempt 1/5)"))
				Expect(st.LastError).To(ContainSubstring(wantInError))
				Expect(f.arm.count(depEnvImport)).To(Equal(1))
			},
			Entry("409 Conflict", depEnvFail(http.StatusConflict, "Conflict"), "409"),
			Entry("412 PreconditionFailed", depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"), "412"),
			Entry("422 Management API timed out", depEnvFail(http.StatusUnprocessableEntity, "ManagementApiRequestFailed"), "422"),
			Entry("429 TooManyRequests", depEnvFail(http.StatusTooManyRequests, "TooManyRequests"), "429"),
			Entry("500 InternalServerError", depEnvFail(http.StatusInternalServerError, "InternalServerError"), "500"),
			Entry("502 without a JSON body", depEnvFailBody(http.StatusBadGateway, "<html>bad gateway</html>"), "bad gateway"),
			Entry("503 ServiceUnavailable", depEnvFail(http.StatusServiceUnavailable, "ServiceUnavailable"), "503"),
			Entry("504 GatewayTimeout", depEnvFail(http.StatusGatewayTimeout, "GatewayTimeout"), "504"),
			Entry("400 with the transient code PreconditionFailed", depEnvFail(http.StatusBadRequest, "PreconditionFailed"), "PreconditionFailed"),
			Entry("400 with the transient code Timeout", depEnvFail(http.StatusBadRequest, "Timeout"), "Timeout"),
			Entry("400 whose first detail is Conflict", depEnvFailBody(http.StatusBadRequest,
				`{"error":{"code":"ValidationError","message":"x","details":[{"code":"Conflict","message":"busy"}]}}`), "Conflict"),
			Entry("404 with the transient code ManagementApiRequestFailed", depEnvFail(http.StatusNotFound, "ManagementApiRequestFailed"), "404"),
		)

		It("backs off when ARM cannot be reached at all", func() {
			f := newDepEnvFixture(depEnvOptions{})
			closed := httptest.NewServer(http.NotFoundHandler())
			closedURL := closed.URL
			closed.Close()
			DeferCleanup(apim.UseEndpoint(closedURL, &http.Client{Timeout: 2 * time.Second}))

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseBackoff))
			Expect(st.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(st.LastError).To(ContainSubstring("import API"))
			Expect(f.arm.total()).To(BeZero(), "nothing reached the fake")
		})
	})

	Context("with a failure in a later step", func() {
		It("backs off on a 412 from the serviceUrl patch and redoes the chain from the import", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}, tagIDs: []string{"t1"}})
			f.arm.on(depEnvServiceURL, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			Expect(f.arm.stepsSince(0)).To(Equal([]string{depEnvGetAPI, depEnvImport, depEnvServiceURL}))
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseBackoff))
			Expect(st.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(st.NextAttemptAt).To(Equal(depEnvAt(time.Minute)))
			Expect(st.Message).To(HavePrefix(depEnvMsgServiceURL + ": APIM write failed (transient, attempt 1/5)"))
			Expect(st.LastError).To(ContainSubstring("patch serviceUrl"))
			Expect(st.LastError).To(ContainSubstring("PreconditionFailed"))
			Expect(st.AppliedHash).To(BeEmpty())

			By("reconciling before nextAttemptAt")
			f.clock.advance(20 * time.Second)
			Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: 40 * time.Second}))

			By("succeeding once due")
			f.arm.on(depEnvServiceURL, nil)
			f.toNextAttempt()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.counts()).To(Equal(map[string]int{
				depEnvGetAPI: 2, depEnvImport: 2, depEnvServiceURL: 2, depEnvSubscriptionRequired: 1,
				depEnvProduct: 1, depEnvTag: 1, depEnvServiceDetails: 1,
			}))
			st = f.get().Status
			Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(st.RetryStatus).To(Equal(apimv1.RetryStatus{}))
			Expect(st.AppliedHash).To(Equal(st.DesiredHash))
		})

		It("stalls after five serviceUrl failures with five imports and five patches", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvServiceURL, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))

			Expect(f.drive(8)).To(Equal([]time.Duration{
				time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0, 0, 0, 0,
			}))

			Expect(f.arm.count(depEnvImport)).To(Equal(5))
			Expect(f.arm.count(depEnvServiceURL)).To(Equal(5))
			Expect(f.arm.count(depEnvSubscriptionRequired)).To(BeZero())
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled))
			Expect(st.Message).To(HavePrefix(depEnvMsgServiceURL + ": APIM write stalled after 5 failures in a row"))
		})

		DescribeTable("a transient failure of any later step backs off, and the step succeeds on the next attempt",
			func(step string, responder depEnvResponder, wantMessage string, wantSteps []string) {
				f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1", "p2"}, tagIDs: []string{"t1", "t2"}})
				f.arm.on(step, responder)

				Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
				Expect(f.arm.stepsSince(0)).To(Equal(wantSteps))
				st := f.get().Status
				Expect(st.Phase).To(Equal(phaseBackoff))
				Expect(st.Message).To(HavePrefix(wantMessage + ": APIM write failed (transient, attempt 1/5)"))

				f.arm.on(step, nil)
				f.toNextAttempt()
				Expect(f.reconcile()).To(BeZero())
				st = f.get().Status
				Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
				Expect(st.RetryStatus).To(Equal(apimv1.RetryStatus{}))
				Expect(f.arm.count(depEnvImport)).To(Equal(2))
			},
			Entry("subscriptionRequired 409", depEnvSubscriptionRequired, depEnvFail(http.StatusConflict, "Conflict"), depEnvMsgSubscription,
				[]string{depEnvGetAPI, depEnvImport, depEnvServiceURL, depEnvSubscriptionRequired}),
			Entry("second product 412", depEnvProduct, depEnvFailPath("/products/p2/", http.StatusPreconditionFailed, "PreconditionFailed"),
				depEnvMsgProducts,
				[]string{depEnvGetAPI, depEnvImport, depEnvServiceURL, depEnvSubscriptionRequired, depEnvProduct, depEnvProduct}),
			Entry("first tag 429", depEnvTag, depEnvFailPath("/tags/t1", http.StatusTooManyRequests, "TooManyRequests"), depEnvMsgTags,
				[]string{depEnvGetAPI, depEnvImport, depEnvServiceURL, depEnvSubscriptionRequired, depEnvProduct, depEnvProduct, depEnvTag}),
			// The APIMProduct or APIMTag of the same sync has not been written to APIM yet,
			// or is itself backing off. Nothing re-triggers the deployment when it lands,
			// so a 404 here must be retried, not end in Invalid.
			Entry("second product 404 (product not in APIM yet)", depEnvProduct,
				depEnvFailPath("/products/p2/", http.StatusNotFound, "ResourceNotFound"), depEnvMsgProducts,
				[]string{depEnvGetAPI, depEnvImport, depEnvServiceURL, depEnvSubscriptionRequired, depEnvProduct, depEnvProduct}),
			Entry("first tag 404 (tag not in APIM yet)", depEnvTag,
				depEnvFailPath("/tags/t1", http.StatusNotFound, "ResourceNotFound"), depEnvMsgTags,
				[]string{depEnvGetAPI, depEnvImport, depEnvServiceURL, depEnvSubscriptionRequired, depEnvProduct, depEnvProduct, depEnvTag}),
			Entry("serviceUrl 404 on the API just imported", depEnvServiceURL, depEnvFail(http.StatusNotFound, "ResourceNotFound"),
				depEnvMsgServiceURL, []string{depEnvGetAPI, depEnvImport, depEnvServiceURL}),
			Entry("service details 503", depEnvServiceDetails, depEnvFail(http.StatusServiceUnavailable, "ServiceUnavailable"), depEnvMsgDetails,
				[]string{depEnvGetAPI, depEnvImport, depEnvServiceURL, depEnvSubscriptionRequired, depEnvProduct, depEnvProduct,
					depEnvTag, depEnvTag, depEnvServiceDetails}),
		)

		DescribeTable("a permanent failure of a later step makes it Invalid at once",
			func(step string, status int, code, wantMessage string) {
				f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}, tagIDs: []string{"t1"}})
				f.arm.on(step, depEnvFail(status, code))

				Expect(f.reconcile()).To(BeZero())

				st := f.get().Status
				Expect(st.Phase).To(Equal(phaseInvalid))
				Expect(st.Message).To(HavePrefix(wantMessage + ": APIM rejected the write"))
				Expect(st.NextAttemptAt).To(BeEmpty())
				f.clock.advance(24 * time.Hour)
				Expect(f.reconcileWithoutAPIM()).To(BeZero())
				Expect(f.arm.count(step)).To(Equal(1))
			},
			Entry("serviceUrl 400", depEnvServiceURL, http.StatusBadRequest, "ValidationError", depEnvMsgServiceURL),
			Entry("subscriptionRequired 403", depEnvSubscriptionRequired, http.StatusForbidden, "AuthorizationFailed", depEnvMsgSubscription),
			Entry("product 403", depEnvProduct, http.StatusForbidden, "AuthorizationFailed", depEnvMsgProducts),
			Entry("tag 400", depEnvTag, http.StatusBadRequest, "ValidationError", depEnvMsgTags),
			Entry("service details 401", depEnvServiceDetails, http.StatusUnauthorized, "InvalidAuthenticationToken", depEnvMsgDetails),
		)

		It("counts failures of different steps towards the same five", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}, tagIDs: []string{"t1"}})
			plan := []struct {
				step    string
				message string
			}{
				{depEnvImport, depEnvMsgImport},
				{depEnvServiceURL, depEnvMsgServiceURL},
				{depEnvSubscriptionRequired, depEnvMsgSubscription},
				{depEnvProduct, depEnvMsgProducts},
				{depEnvTag, depEnvMsgTags},
			}
			for i, p := range plan {
				for _, other := range plan {
					f.arm.on(other.step, nil)
				}
				f.arm.on(p.step, depEnvFail(http.StatusConflict, "Conflict"))
				f.toNextAttempt()
				f.reconcile()
				st := f.get().Status
				Expect(st.ConsecutiveFailures).To(Equal(int32(i+1)), "after a failure of %s", p.step)
				Expect(st.Message).To(HavePrefix(p.message+": "), "after a failure of %s", p.step)
			}
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled))
			Expect(f.arm.count(depEnvImport)).To(Equal(5))
			f.clock.advance(time.Hour)
			Expect(f.reconcileWithoutAPIM()).To(BeZero())
		})
	})

	Context("with a new desired hash", func() {
		It("resets a Stalled deployment on a new OpenAPI document and imports exactly once", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.stall()
			stalledHash := f.get().Status.DesiredHash
			f.doc.publishNewVersion()
			f.arm.on(depEnvImport, nil)

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.count(depEnvImport)).To(Equal(6))
			Expect(f.arm.last(depEnvImport).Body).To(Equal(depEnvDocV2))
			st := f.get().Status
			Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(st.DesiredHash).To(Equal(f.desiredHash(depEnvDocV2)))
			Expect(st.DesiredHash).NotTo(Equal(stalledHash))
			Expect(st.AppliedHash).To(Equal(st.DesiredHash))
			Expect(st.OpenAPIHash).To(Equal(sha256Hex([]byte(depEnvDocV2))))
			Expect(st.RetryStatus).To(Equal(apimv1.RetryStatus{}))

			By("reconciling again with the new document")
			calls := f.arm.total()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.total()).To(Equal(calls))
		})

		It("starts counting again from one when the new document also fails", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.stall()
			f.doc.publishNewVersion()

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseBackoff))
			Expect(st.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(st.Message).To(ContainSubstring("attempt 1/5"))
			Expect(f.arm.count(depEnvImport)).To(Equal(6))
		})

		It("waits out a backoff on a new OpenAPI document, then imports the new one", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			f.arm.on(depEnvImport, nil)
			f.doc.publishNewVersion()
			Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}),
				"a new document alone does not cut the backoff short")
			Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(1)), "nor clear the count")

			f.toNextAttempt()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.count(depEnvImport)).To(Equal(2))
			Expect(f.arm.last(depEnvImport).Body).To(Equal(depEnvDocV2))
			st := f.get().Status
			Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(st.AppliedHash).To(Equal(f.desiredHash(depEnvDocV2)))
			Expect(st.RetryStatus).To(Equal(apimv1.RetryStatus{}))
		})

		It("still cuts a backoff short on a spec change that comes with a new document", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			f.arm.on(depEnvImport, nil)
			f.doc.publishNewVersion()
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.RoutePrefix = "/depenv-v2" })
			Expect(f.reconcile()).To(BeZero(), "the clock has not moved, yet the new spec is written at once")
			Expect(f.arm.count(depEnvImport)).To(Equal(2))
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})

		It("reaches Stalled after five imports when the document differs on every fetch", func() {
			// Two app versions behind one Service (a canary paused for hours), or a
			// generator that stamps the time into the document: every fetch returns a
			// different document. Each new hash used to count as a spec change and clear
			// the failures, so the deployment re-imported every minute and never stalled.
			f := newDepEnvFixture(depEnvOptions{})
			f.doc.alternate(depEnvDocV1, depEnvDocV2)
			f.arm.on(depEnvImport, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))

			Expect(f.drive(5)).To(Equal([]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}),
				"doubling delays and no requeue once Stalled: the failures were never cleared")
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled))
			Expect(st.ConsecutiveFailures).To(Equal(int32(5)))
			Expect(st.NextAttemptAt).To(BeEmpty())
			Expect(f.arm.count(depEnvImport)).To(Equal(5))
			Expect(f.doc.fetches()).To(Equal(5))
			Expect(f.arm.last(depEnvImport).Body).To(Equal(depEnvDocV1), "the fetches really alternated")
		})

		It("gives a Stalled deployment one more bounded round per event that brings a new document", func() {
			// Stalled is not requeued, so only an event (a rollout's ReplicaSet signal, an
			// operator restart) reconciles it again. A new document then lifts the stall,
			// as a fixed document should, but the round is bounded by five attempts again.
			f := newDepEnvFixture(depEnvOptions{})
			f.doc.alternate(depEnvDocV1, depEnvDocV2)
			f.arm.on(depEnvImport, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))
			f.drive(5)
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))

			Expect(f.drive(5)).To(Equal([]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}))
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))
			Expect(f.arm.count(depEnvImport)).To(Equal(10))
		})

		It("does not import inside a backoff when the document flips between fetches", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.doc.alternate(depEnvDocV1, depEnvDocV2)
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			for range 6 {
				f.clock.advance(5 * time.Second)
				result := f.reconcileWithoutAPIM()
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
				Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
			}
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
			Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(1)))
		})

		It("lifts Invalid on a new OpenAPI document", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusBadRequest, "ValidationError"))
			Expect(f.reconcile()).To(BeZero())
			Expect(f.get().Status.Phase).To(Equal(phaseInvalid))

			f.arm.on(depEnvImport, nil)
			f.doc.publishNewVersion()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.count(depEnvImport)).To(Equal(2))
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})

		DescribeTable("spec edits that change the desired hash reset a Stalled deployment",
			func(mutate func(*apimv1.APIMAPIDeploymentSpec), wantGetAPI bool) {
				f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}, tagIDs: []string{"t1"}})
				f.stall()
				getAPICalls := f.arm.count(depEnvGetAPI)
				f.update(func(d *apimv1.APIMAPIDeployment) { mutate(&d.Spec) })
				f.arm.on(depEnvImport, nil)

				Expect(f.reconcile()).To(BeZero())

				Expect(f.arm.count(depEnvImport)).To(Equal(6), "one import for the new spec")
				if wantGetAPI {
					Expect(f.arm.count(depEnvGetAPI)).To(Equal(getAPICalls + 1))
				}
				st := f.get().Status
				Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
				Expect(st.RetryStatus).To(Equal(apimv1.RetryStatus{}))
				Expect(st.AppliedHash).To(Equal(f.desiredHash(depEnvDocV1)))
			},
			Entry("serviceUrl", func(s *apimv1.APIMAPIDeploymentSpec) { s.ServiceURL = "https://backend-v2.depenv.net" }, true),
			Entry("routePrefix", func(s *apimv1.APIMAPIDeploymentSpec) { s.RoutePrefix = "/depenv-v2" }, true),
			Entry("a product added", func(s *apimv1.APIMAPIDeploymentSpec) { s.ProductIDs = append(s.ProductIDs, "p2") }, true),
			Entry("a tag removed", func(s *apimv1.APIMAPIDeploymentSpec) { s.TagIDs = nil }, true),
			Entry("subscriptionRequired off", func(s *apimv1.APIMAPIDeploymentSpec) { s.SubscriptionRequired = false }, true),
			Entry("a revision", func(s *apimv1.APIMAPIDeploymentSpec) { s.Revision = "2" }, false),
		)

		DescribeTable("edits that leave the desired hash alone keep a Stalled deployment quiet",
			func(mutate func(*apimv1.APIMAPIDeployment)) {
				f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1", "p2"}, tagIDs: []string{"t1", "t2"}})
				f.stall()
				f.update(mutate)
				f.arm.on(depEnvImport, nil)

				Expect(f.reconcileWithoutAPIM()).To(BeZero())
				Expect(f.get().Status.Phase).To(Equal(phaseStalled))
			},
			Entry("a label", func(d *apimv1.APIMAPIDeployment) { d.Labels = map[string]string{"team": "depenv"} }),
			Entry("an unrelated annotation", func(d *apimv1.APIMAPIDeployment) {
				d.Annotations = map[string]string{"example.com/note": "hello"}
			}),
			Entry("products reordered", func(d *apimv1.APIMAPIDeployment) { d.Spec.ProductIDs = []string{"p2", "p1"} }),
			Entry("tags reordered", func(d *apimv1.APIMAPIDeployment) { d.Spec.TagIDs = []string{"t2", "t1"} }),
		)

		It("clears the failures without calling ARM when a Stalled change is reverted to the applied spec", func() {
			f := newDepEnvFixture(depEnvOptions{})
			Expect(f.reconcile()).To(BeZero())
			applied := f.get().Status.AppliedHash

			By("stalling a change of the backend URL")
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.ServiceURL = "https://broken.depenv.net" })
			f.arm.on(depEnvServiceURL, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))
			f.drive(5)
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled))
			Expect(st.AppliedHash).To(Equal(applied), "the last applied state is kept while a change fails")
			Expect(st.DesiredHash).NotTo(Equal(applied))

			By("reverting the change")
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.ServiceURL = depEnvBackend })
			calls := f.arm.total()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.total()).To(Equal(calls))
			st = f.get().Status
			Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(st.DesiredHash).To(Equal(applied))
			Expect(st.RetryStatus).To(Equal(apimv1.RetryStatus{}))
		})
	})

	Context("with the retry annotation", func() {
		It("resets a Stalled deployment once per value", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.stall()

			By("setting the annotation while APIM still fails")
			f.annotate("1")
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
			Expect(f.arm.count(depEnvImport)).To(Equal(6), "exactly one import for the new value")
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseBackoff))
			Expect(st.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(st.LastRetryAnnotation).To(Equal("1"))
			Expect(st.Message).To(ContainSubstring("attempt 1/5"))

			By("reconciling with the same value before nextAttemptAt")
			Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			By("failing four more times on the same value")
			f.drive(4)
			st = f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled))
			Expect(st.ConsecutiveFailures).To(Equal(int32(5)))
			Expect(f.arm.count(depEnvImport)).To(Equal(10))

			By("reconciling with the same value, Stalled again")
			for i := 0; i < 5; i++ {
				f.clock.advance(time.Hour)
				Expect(f.reconcileWithoutAPIM()).To(BeZero())
			}
			Expect(f.arm.count(depEnvImport)).To(Equal(10), "the same value never retriggers")

			By("setting a new value once APIM has recovered")
			f.arm.on(depEnvImport, nil)
			f.annotate("2")
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.count(depEnvImport)).To(Equal(11))
			st = f.get().Status
			Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(st.ConsecutiveFailures).To(BeZero())
			Expect(st.NextAttemptAt).To(BeEmpty())
			Expect(st.LastRetryAnnotation).To(Equal("2"))
		})

		It("retries an Invalid deployment at once and only once per value", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusBadRequest, "ValidationError"))
			Expect(f.reconcile()).To(BeZero())

			f.annotate("a")
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.count(depEnvImport)).To(Equal(2))
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseInvalid))
			Expect(st.ConsecutiveFailures).To(Equal(int32(1)), "the reset makes this attempt the first again")
			Expect(st.LastRetryAnnotation).To(Equal("a"))

			for i := 0; i < 3; i++ {
				Expect(f.reconcileWithoutAPIM()).To(BeZero())
			}

			f.arm.on(depEnvImport, nil)
			f.annotate("b")
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.count(depEnvImport)).To(Equal(3))
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})

		It("cuts a backoff short", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))
			f.drive(3)
			Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(3)))

			f.arm.on(depEnvImport, nil)
			f.annotate("now")
			Expect(f.reconcile()).To(BeZero(), "written before nextAttemptAt")
			Expect(f.arm.count(depEnvImport)).To(Equal(4))
			st := f.get().Status
			Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(st.ConsecutiveFailures).To(BeZero())
			Expect(st.LastRetryAnnotation).To(Equal("now"))
		})

		It("does not retry when the annotation is removed", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.stall()
			f.annotate("1")
			f.drive(5)
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))
			calls := f.arm.count(depEnvImport)

			f.removeRetryAnnotation()
			Expect(f.reconcileWithoutAPIM()).To(BeZero())
			Expect(f.arm.count(depEnvImport)).To(Equal(calls))
		})

		It("retries when the annotation goes back to an earlier value", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusBadRequest, "ValidationError"))
			Expect(f.reconcile()).To(BeZero())

			f.annotate("1")
			f.reconcile()
			f.annotate("2")
			f.reconcile()
			Expect(f.arm.count(depEnvImport)).To(Equal(3))

			f.annotate("1")
			f.reconcile()
			Expect(f.arm.count(depEnvImport)).To(Equal(4), "1 differs from the last handled value 2")
			Expect(f.get().Status.LastRetryAnnotation).To(Equal("1"))
		})

		It("records an annotation present from creation and does not let it reset later failures", func() {
			f := newDepEnvFixture(depEnvOptions{annotations: map[string]string{retryAnnotation: "seed"}})
			f.arm.on(depEnvImport, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
			st := f.get().Status
			Expect(st.LastRetryAnnotation).To(Equal("seed"))
			Expect(st.ConsecutiveFailures).To(Equal(int32(1)))

			By("reconciling again before nextAttemptAt")
			Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			By("failing on to Stalled")
			Expect(f.drive(4)).To(Equal([]time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}))
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))
			Expect(f.arm.count(depEnvImport)).To(Equal(5))
		})

		It("does not call ARM on an in-sync deployment", func() {
			f := newDepEnvFixture(depEnvOptions{})
			Expect(f.reconcile()).To(BeZero())
			calls := f.arm.total()

			f.annotate("x")
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.total()).To(Equal(calls), "APIM already holds the desired hash")
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})
	})

	Context("with a websocket API", func() {
		It("upserts with a plain PUT and never fetches a document", func() {
			f := newDepEnvFixture(depEnvOptions{webSocket: true, productIDs: []string{"p1"}})

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.counts()).To(Equal(map[string]int{
				depEnvGetAPI: 1, depEnvWebSocket: 1, depEnvServiceURL: 1, depEnvSubscriptionRequired: 1,
				depEnvProduct: 1, depEnvServiceDetails: 1,
			}))
			Expect(f.doc.fetches()).To(BeZero())
			ws := f.arm.last(depEnvWebSocket)
			Expect(ws.Query.Has("import")).To(BeFalse())
			Expect(ws.Header.Get("Content-Type")).To(Equal("application/json"))
			Expect(ws.Header.Get("If-Match")).To(Equal("*"))
			var body struct {
				Properties map[string]any `json:"properties"`
			}
			Expect(json.Unmarshal([]byte(ws.Body), &body)).To(Succeed())
			Expect(body.Properties).To(HaveKeyWithValue("type", "websocket"))
			Expect(body.Properties).To(HaveKeyWithValue("path", "depenv"))
			Expect(body.Properties).To(HaveKeyWithValue("serviceUrl", depEnvWSBackend))
			Expect(body.Properties).To(HaveKeyWithValue("displayName", f.apiID))

			st := f.get().Status
			Expect(st.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(st.OpenAPIHash).To(BeEmpty())
			Expect(st.AppliedHash).To(Equal(f.desiredHash("")))
			Expect(f.getAPI().Status.ApiHost).To(Equal("wss://gw.depenv.net" + depEnvRoutePrefix))

			By("reconciling again")
			calls := f.arm.total()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.total()).To(Equal(calls))
		})

		It("backs off and stalls after exactly five websocket PUTs", func() {
			f := newDepEnvFixture(depEnvOptions{webSocket: true})
			f.arm.on(depEnvWebSocket, depEnvAccepted(""))
			f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK,
				`{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor"}}`))

			Expect(f.drive(9)).To(Equal([]time.Duration{
				time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0, 0, 0, 0, 0,
			}))

			Expect(f.arm.count(depEnvWebSocket)).To(Equal(5))
			Expect(f.arm.count(depEnvImport)).To(BeZero())
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled))
			Expect(st.Message).To(HavePrefix(depEnvMsgWebSocket + ": APIM write stalled after 5 failures in a row"))
			Expect(st.LastError).To(ContainSubstring("DeadOperationMonitor"))
		})

		It("goes Invalid on a 400 from the upsert", func() {
			f := newDepEnvFixture(depEnvOptions{webSocket: true})
			f.arm.on(depEnvWebSocket, depEnvFail(http.StatusBadRequest, "ValidationError"))

			Expect(f.reconcile()).To(BeZero())

			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseInvalid))
			Expect(st.Message).To(HavePrefix(depEnvMsgWebSocket + ": APIM rejected the write"))
			f.clock.advance(time.Hour)
			Expect(f.reconcileWithoutAPIM()).To(BeZero())
			Expect(f.arm.count(depEnvWebSocket)).To(Equal(1))
		})

		It("resets a Stalled websocket deployment when its display name changes", func() {
			f := newDepEnvFixture(depEnvOptions{webSocket: true})
			f.arm.on(depEnvWebSocket, depEnvFail(http.StatusConflict, "Conflict"))
			f.drive(5)
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))

			f.update(func(d *apimv1.APIMAPIDeployment) {
				d.Spec.WebSocket = &apimv1.APIMAPIWebSocket{DisplayName: "Depenv hub"}
			})
			f.arm.on(depEnvWebSocket, nil)
			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.count(depEnvWebSocket)).To(Equal(6))
			Expect(f.arm.last(depEnvWebSocket).Body).To(ContainSubstring(`"displayName":"Depenv hub"`))
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		})
	})

	Context("with a non-default retry policy", func() {
		DescribeTable("jitter spreads the first delay by up to 20 % either way",
			func(random float64, want time.Duration) {
				f := newDepEnvFixture(depEnvOptions{})
				f.reconciler.retry = &retryPolicy{
					BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Jitter: 0.2,
					Now: f.clock.now, Random: func() float64 { return random },
				}
				f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))

				Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: want}))
				Expect(f.get().Status.NextAttemptAt).To(Equal(depEnvAt(want)))
			},
			Entry("lowest", 0.0, 48*time.Second),
			Entry("middle", 0.5, time.Minute),
			// 71.99997 s, rounded up to the second the status can hold.
			Entry("highest", 0.999999, 72*time.Second),
		)

		It("caps the delay at MaxDelay", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.reconciler.retry = &retryPolicy{BaseDelay: time.Minute, MaxDelay: 5 * time.Minute, MaxAttempts: 8, Now: f.clock.now}
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))

			Expect(f.drive(9)).To(Equal([]time.Duration{
				time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 0, 0,
			}))
			Expect(f.arm.count(depEnvImport)).To(Equal(8))
			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled))
			Expect(st.Message).To(ContainSubstring("stalled after 8 failures"))
		})

		It("uses the production policy when the reconciler has none", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.reconciler.retry = nil
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))

			before := time.Now()
			result := f.reconcile()

			Expect(result.RequeueAfter).To(BeNumerically(">=", 48*time.Second))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 73*time.Second))
			st := f.get().Status
			Expect(st.Message).To(ContainSubstring("attempt 1/5"))
			next, err := time.Parse(time.RFC3339, st.NextAttemptAt)
			Expect(err).NotTo(HaveOccurred())
			Expect(next).To(BeTemporally(">=", before.Add(47*time.Second)))
			Expect(next).To(BeTemporally("<=", time.Now().Add(73*time.Second)))
		})
	})

	Context("with paths that keep today's behaviour", func() {
		It("requeues after 30 s without touching ARM or the failure count when identity variables are missing", func() {
			f := newDepEnvFixture(depEnvOptions{})
			Expect(os.Unsetenv("AZURE_CLIENT_ID")).To(Succeed())

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: 30 * time.Second}))

			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseError))
			Expect(st.LastError).To(Equal(errMsgMissingAzureIdentity))
			Expect(st.ConsecutiveFailures).To(BeZero())
			Expect(f.arm.total()).To(BeZero())
			Expect(f.tokenCalls.Load()).To(BeZero())
		})

		It("requeues after 30 s on an OpenAPI fetch failure without counting it as an APIM failure", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.doc.fail(true)

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: requeueFetchFailure}))

			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseError))
			Expect(st.Message).To(Equal("Failed to fetch OpenAPI definition"))
			Expect(st.ConsecutiveFailures).To(BeZero())
			Expect(f.arm.total()).To(BeZero())
		})

		It("does not request a token while Stalled or backing off", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusTooManyRequests, "TooManyRequests"))
			f.reconcile()
			Expect(f.tokenCalls.Load()).To(Equal(int32(1)))
			f.clock.advance(10 * time.Second)
			Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: 50 * time.Second}))
			Expect(f.tokenCalls.Load()).To(Equal(int32(1)))
		})

		// A passing OpenAPI fetch failure writes phase Error; once the document is
		// reachable again the held reconcile must put Stalled back, or the deployment would
		// read Error / "Failed to fetch OpenAPI definition" while waiting for the annotation.
		It("still reads Stalled after a passing OpenAPI fetch failure", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.stall()

			By("the document server failing for one reconcile")
			f.doc.fail(true)
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: requeueFetchFailure}))
			Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(5)), "a fetch failure keeps the APIM failure count")

			By("the document server recovering")
			f.doc.fail(false)
			calls := f.arm.total()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.total()).To(Equal(calls), "still held: no ARM call")

			st := f.get().Status
			Expect(st.Phase).To(Equal(phaseStalled), "the status must say why nothing is written to APIM")
		})
	})

	Context("with the controller running in a manager", func() {
		// startManager runs the deployment controller in a manager that only watches the
		// fixture's namespace, with real time and tiny delays: every backoff then lasts
		// at most the one second nextAttemptAt is rounded up to.
		startManager := func(f *depEnvFixture) {
			GinkgoHelper()
			f.reconciler.retry = &retryPolicy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxAttempts: 5, Now: time.Now}
			skip := true
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:                 scheme.Scheme,
				Metrics:                metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
				Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{f.ns: {}}},
				Controller:             ctrlconfig.Controller{SkipNameValidation: &skip},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(f.reconciler.SetupWithManager(mgr)).To(Succeed())

			mgrCtx, stop := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				defer close(done)
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			DeferCleanup(func() {
				stop()
				Eventually(done, 10*time.Second).Should(BeClosed())
			})
		}

		phaseOf := func(f *depEnvFixture) func() string {
			return func() string { return f.get().Status.Phase }
		}

		It("stalls on its own after exactly five import PUTs and then stays quiet", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}})
			f.arm.on(depEnvImport, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))
			startManager(f)

			Eventually(phaseOf(f), 15*time.Second, 20*time.Millisecond).Should(Equal(phaseStalled))
			Expect(f.arm.count(depEnvImport)).To(Equal(5))

			By("staying quiet through events the predicate lets through and events it filters")
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Labels = map[string]string{"touched": "yes"} })
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.ProductIDs = []string{"p1"} }) // no-op spec write
			f.update(func(d *apimv1.APIMAPIDeployment) {
				d.Annotations = map[string]string{apimDeploymentSignalAnnotation: "rs-changed"}
			})
			Consistently(func() int { return f.arm.count(depEnvImport) }, 1500*time.Millisecond, 50*time.Millisecond).Should(Equal(5))
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))
		})

		It("picks up the retry annotation through the predicate and succeeds", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))
			startManager(f)
			Eventually(phaseOf(f), 15*time.Second, 20*time.Millisecond).Should(Equal(phaseStalled))
			Expect(f.arm.count(depEnvImport)).To(Equal(5))

			f.arm.on(depEnvImport, nil)
			f.annotate("after-incident")

			Eventually(phaseOf(f), 10*time.Second, 20*time.Millisecond).Should(Equal(apimDeploymentPhaseSucceeded))
			Expect(f.arm.count(depEnvImport)).To(Equal(6))
			st := f.get().Status
			Expect(st.RetryStatus).To(Equal(apimv1.RetryStatus{LastRetryAnnotation: "after-incident"}))
			Expect(st.AppliedHash).To(Equal(st.DesiredHash))
			Consistently(func() int { return f.arm.count(depEnvImport) }, time.Second, 50*time.Millisecond).Should(Equal(6))
		})

		It("goes Invalid on its own after one PUT and stays quiet", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusBadRequest, "ValidationError"))
			startManager(f)

			Eventually(phaseOf(f), 10*time.Second, 20*time.Millisecond).Should(Equal(phaseInvalid))
			Consistently(func() int { return f.arm.count(depEnvImport) }, time.Second, 50*time.Millisecond).Should(Equal(1))
		})

		It("recovers on its own from two transient failures", func() {
			f := newDepEnvFixture(depEnvOptions{})
			var failures atomic.Int32
			f.arm.on(depEnvImport, func(w http.ResponseWriter, _ *http.Request) bool {
				if failures.Add(1) <= 2 {
					depEnvWriteError(w, http.StatusTooManyRequests, "TooManyRequests", "slow down")
					return true
				}
				return false
			})
			startManager(f)

			Eventually(phaseOf(f), 10*time.Second, 20*time.Millisecond).Should(Equal(apimDeploymentPhaseSucceeded))
			Expect(f.arm.count(depEnvImport)).To(Equal(3))
			Expect(f.get().Status.RetryStatus).To(Equal(apimv1.RetryStatus{}))
		})
	})
})
