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
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs run the same retry matrix against APIMTag and APIMInboundPolicy (at API
// and at operation level): success, Backoff, Stalled at five, reset on a generation
// change, reset on the retry annotation, Invalid on a permanent error, and no ARM call
// (not even a token) while a resource waits or is stopped. Each spec drives the real
// Reconcile against envtest and an httptest ARM with a fake clock, so the whole state
// machine runs in milliseconds. The last block wires the reconcilers into a real manager
// to prove the predicates let the retry annotation through and status updates do not
// loop.

var _ = Describe("APIMTag and APIMInboundPolicy retry matrix", func() {
	for _, k := range tpeKinds() {
		Context(k.label(), func() {
			var h *tpeHarness

			BeforeEach(func() {
				h = newTpeHarness(k, nil)
			})

			// ---------------------------------------------------------------- success

			Context("on success", func() {
				It("PUTs once to the kind's ARM path with the token and If-Match, and records Created", func() {
					Expect(h.reconcile()).To(BeZero())

					reqs := h.arm.all()
					Expect(reqs).To(HaveLen(1))
					Expect(reqs[0].method).To(Equal(http.MethodPut))
					Expect(reqs[0].path).To(Equal(k.armPath(h.service)))
					Expect(reqs[0].query).To(HavePrefix("api-version="))
					Expect(reqs[0].auth).To(Equal("Bearer " + tpeToken))
					Expect(reqs[0].ifMatch).To(Equal("*"))
					Expect(h.tokens()).To(Equal(1))

					v := h.view()
					Expect(v.Phase).To(Equal(phaseCreated))
					Expect(v.Message).To(Equal(k.successMessage))
					Expect(v.ObservedGeneration).To(Equal(v.Generation))
					Expect(v.Retry).To(BeZero())
				})

				It("sends the spec in the PUT body", func() {
					Expect(h.reconcile()).To(BeZero())
					body := h.arm.all()[0].body
					if k.kind == "APIMTag" {
						Expect(body).To(ContainSubstring(`"displayName":"TPE Tag"`))
					} else {
						Expect(body).To(ContainSubstring(`"format":"xml"`))
						Expect(body).NotTo(ContainSubstring("x-tpe"), "the unchanged policy has no header yet")
						var sent struct {
							Properties struct {
								Format string `json:"format"`
								Value  string `json:"value"`
							} `json:"properties"`
						}
						Expect(json.Unmarshal([]byte(body), &sent)).To(Succeed())
						Expect(sent.Properties.Format).To(Equal("xml"))
						Expect(sent.Properties.Value).To(Equal(tpePolicyXML))
					}
				})

				It("logs ▶️ and 💚 with kind, namespace, name and the kind's id", func() {
					Expect(h.reconcile()).To(BeZero())
					var msgs []string
					for _, l := range h.logs() {
						if strings.Contains(l.msg, "APIM write") {
							msgs = append(msgs, l.msg)
							h.expectIdentity(l)
							Expect(l.kv).To(HaveKeyWithValue("attempt", "1/5"))
						}
					}
					Expect(msgs).To(Equal([]string{msgWriteStarting, msgWriteSucceeded}))
				})

				It("writes again on a resync after a success, as before the retry handling", func() {
					Expect(h.reconcile()).To(BeZero())
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(2))
					Expect(h.view().Retry).To(BeZero())
				})

				It("clears earlier failures once a write after an expired backoff succeeds", func() {
					h.setStatus(phaseBackoff, -1, apimv1.RetryStatus{
						ConsecutiveFailures: 3,
						NextAttemptAt:       tpeStart.Add(-time.Minute).Format(time.RFC3339),
					})
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(1))
					v := h.view()
					Expect(v.Phase).To(Equal(phaseCreated))
					Expect(v.Message).To(Equal(k.successMessage))
					Expect(v.Retry).To(BeZero())
					Expect(v.ObservedGeneration).To(Equal(v.Generation))
				})

				It("lets the write through exactly at nextAttemptAt", func() {
					h.setStatus(phaseBackoff, -1, apimv1.RetryStatus{
						ConsecutiveFailures: 2,
						NextAttemptAt:       tpeStart.Format(time.RFC3339),
					})
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(1))
					Expect(h.view().Phase).To(Equal(phaseCreated))
				})

				It("treats an unreadable nextAttemptAt as due rather than stopping for good", func() {
					h.setStatus(phaseBackoff, -1, apimv1.RetryStatus{ConsecutiveFailures: 2, NextAttemptAt: "not-a-time"})
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(1))
					Expect(h.view().Retry).To(BeZero())
				})

				It("counts the next failure from one again after a success in between", func() {
					h.failTransiently(3)
					h.arm.reply(tpeOK)
					Expect(h.reconcile()).To(BeZero())
					Expect(h.view().Retry).To(BeZero())

					h.arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
					Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute))
					v := h.view()
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)))
					Expect(v.Message).To(ContainSubstring("attempt 1/5"))
				})
			})

			// ---------------------------------------------------------------- backoff

			Context("on a transient failure", func() {
				DescribeTable("goes to Backoff for one base delay and keeps the error in the message",
					func(reply tpeReply, wantInMessage string) {
						h.arm.reply(reply)

						result := h.reconcile()
						Expect(result.RequeueAfter).To(Equal(time.Minute))
						Expect(h.puts()).To(Equal(1))

						v := h.view()
						Expect(v.Phase).To(Equal(phaseBackoff))
						Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)))
						Expect(v.Retry.NextAttemptAt).To(Equal("2026-09-25T10:01:00Z"))
						Expect(v.ObservedGeneration).To(Equal(v.Generation), "the failure belongs to this generation")
						Expect(v.Message).To(HavePrefix(k.failurePrefix))
						Expect(v.Message).To(ContainSubstring("transient, attempt 1/5"))
						Expect(v.Message).To(ContainSubstring("next attempt at 2026-09-25T10:01:00Z"))
						Expect(v.Message).To(ContainSubstring(wantInMessage))

						failed := h.linesWith(msgWriteFailed)
						Expect(failed).To(HaveLen(1))
						h.expectIdentity(failed[0])
						Expect(failed[0].err).To(BeTrue())
						Expect(failed[0].kv).To(HaveKeyWithValue("class", errorClassTransient))
						Expect(failed[0].kv).To(HaveKeyWithValue("attempt", "1/5"))
						Expect(failed[0].kv).To(HaveKeyWithValue("nextAttemptAt", "2026-09-25T10:01:00Z"))
						Expect(h.linesWith(msgWriteRejected)).To(BeEmpty())
						Expect(h.linesWith(msgWriteStalled)).To(BeEmpty())
					},
					Entry("409 Conflict", tpeFail(http.StatusConflict, "Conflict"), "Conflict"),
					Entry("412 PreconditionFailed", tpeFail(http.StatusPreconditionFailed, "PreconditionFailed"), "PreconditionFailed"),
					Entry("422 Management API timed out", tpeFail(http.StatusUnprocessableEntity, "ManagementApiRequestFailed"), "ManagementApiRequestFailed"),
					Entry("429 throttled", tpeFail(http.StatusTooManyRequests, "TooManyRequests"), "429"),
					Entry("500 InternalServerError", tpeFail(http.StatusInternalServerError, "InternalServerError"), "InternalServerError"),
					Entry("502 without a body code", tpeFail(http.StatusBadGateway, ""), "502"),
					Entry("503 ServiceUnavailable", tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"), "503"),
					Entry("504 GatewayTimeout", tpeFail(http.StatusGatewayTimeout, "GatewayTimeout"), "504"),
					Entry("400 with the transient code PreconditionFailed", tpeFail(http.StatusBadRequest, "PreconditionFailed"), "PreconditionFailed"),
					Entry("400 with the transient code Timeout", tpeFail(http.StatusBadRequest, "Timeout"), "Timeout"),
					Entry("400 with the transient code ManagementApiRequestFailed", tpeFail(http.StatusBadRequest, "ManagementApiRequestFailed"), "ManagementApiRequestFailed"),
					Entry("403 with the transient code Conflict", tpeFail(http.StatusForbidden, "Conflict"), "Conflict"),
					Entry("404 with the transient code InternalServerError", tpeFail(http.StatusNotFound, "InternalServerError"), "InternalServerError"),
					Entry("400 whose first detail code is transient", tpeFailDetail(http.StatusBadRequest, "ValidationError", "PreconditionFailed"), "PreconditionFailed"),
					Entry("an unknown 418", tpeFail(http.StatusTeapot, "Teapot"), "Teapot"),
					Entry("a dropped connection", tpeHangup, "upsert"),
				)

				DescribeTable("does not call APIM, nor fetch a token, before nextAttemptAt",
					func(after time.Duration) {
						h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))
						Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute))
						before := h.view()

						h.clock.advance(after)
						result := h.reconcile()
						Expect(result.RequeueAfter).To(Equal(time.Minute-after), "the requeue is the time that is left")
						Expect(h.puts()).To(Equal(1), "no APIM call while backing off")
						Expect(h.tokens()).To(Equal(1), "not even a token while backing off")
						Expect(h.view()).To(Equal(before), "a skipped reconcile writes no status")

						skipped := h.linesWith(msgWriteBackingOff)
						Expect(skipped).To(HaveLen(1))
						h.expectIdentity(skipped[0])
						Expect(skipped[0].kv).To(HaveKeyWithValue("nextAttemptAt", "2026-09-25T10:01:00Z"))
						Expect(skipped[0].kv).To(HaveKeyWithValue("attempt", "2/5"))
					},
					Entry("at once", time.Duration(0)),
					Entry("after a second", time.Second),
					Entry("half way", 30*time.Second),
					Entry("a second before", 59*time.Second),
					Entry("a millisecond before", time.Minute-time.Millisecond),
				)

				It("keeps skipping on repeated reconciles before nextAttemptAt", func() {
					h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))
					h.reconcile()
					for i := 1; i <= 10; i++ {
						h.clock.advance(5 * time.Second)
						Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute - time.Duration(i)*5*time.Second))
					}
					Expect(h.puts()).To(Equal(1))
					Expect(h.tokens()).To(Equal(1))
				})

				It("doubles the wait, 1, 2, 4 and 8 minutes, with the attempt in each message", func() {
					h.arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
					want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
					for i, w := range want {
						result := h.reconcile()
						Expect(result.RequeueAfter).To(Equal(w))
						v := h.view()
						Expect(v.Phase).To(Equal(phaseBackoff))
						Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(i + 1)))
						Expect(v.Message).To(ContainSubstring(fmt.Sprintf("attempt %d/5", i+1)))
						Expect(v.Retry.NextAttemptAt).To(Equal(h.clock.now().Add(w).UTC().Format(time.RFC3339)))
						h.clock.advance(w)
					}
					Expect(h.puts()).To(Equal(4))
				})

				It("rounds nextAttemptAt up to a whole second and waits until then", func() {
					h.clock.t = tpeStart.Add(500 * time.Millisecond)
					h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))

					result := h.reconcile()
					Expect(result.RequeueAfter).To(Equal(60*time.Second + 500*time.Millisecond))
					Expect(h.view().Retry.NextAttemptAt).To(Equal("2026-09-25T10:01:01Z"))

					By("reconciling a minute later: half a second still to go")
					h.clock.advance(time.Minute)
					Expect(h.reconcile().RequeueAfter).To(Equal(500 * time.Millisecond))
					Expect(h.puts()).To(Equal(1))

					By("reconciling at nextAttemptAt: the write goes")
					h.clock.advance(500 * time.Millisecond)
					h.arm.reply(tpeOK)
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(2))
				})

				It("caps the wait at MaxDelay", func() {
					h.policy.MaxDelay = 3 * time.Minute
					h.policy.MaxAttempts = 7
					waits := h.failTransiently(7)
					Expect(waits).To(Equal([]time.Duration{
						time.Minute, 2 * time.Minute, 3 * time.Minute, 3 * time.Minute, 3 * time.Minute, 3 * time.Minute, 0,
					}))
					Expect(h.view().Phase).To(Equal(phaseStalled))
				})

				It("caps the production schedule at 30 minutes when attempts allow it", func() {
					h.policy.MaxAttempts = 9
					waits := h.failTransiently(9)
					Expect(waits).To(Equal([]time.Duration{
						time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
						30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 0,
					}))
				})

				DescribeTable("spreads the wait with jitter",
					func(random float64, want time.Duration) {
						h.policy.Jitter = 0.2
						h.policy.Random = func() float64 { return random }
						h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))
						Expect(h.reconcile().RequeueAfter).To(Equal(want))
						Expect(h.view().Retry.NextAttemptAt).To(Equal(tpeStart.Add(want).Format(time.RFC3339)))
					},
					Entry("-20 %", 0.0, 48*time.Second),
					Entry("none", 0.5, time.Minute),
					Entry("+10 %", 0.75, 66*time.Second),
				)

				It("uses the production policy when the reconciler has none", func() {
					h.policy = nil
					h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))
					before := time.Now()
					result := h.reconcile()
					Expect(result.RequeueAfter).To(BeNumerically(">=", 47*time.Second))
					Expect(result.RequeueAfter).To(BeNumerically("<=", 73*time.Second))
					next, err := time.Parse(time.RFC3339, h.view().Retry.NextAttemptAt)
					Expect(err).NotTo(HaveOccurred())
					Expect(next).To(BeTemporally(">=", before.Add(47*time.Second)))
					Expect(next).To(BeTemporally("<=", time.Now().Add(74*time.Second)))
					Expect(h.view().Message).To(ContainSubstring("attempt 1/5"))
				})
			})

			// ---------------------------------------------------------------- stalled

			Context("after five transient failures", func() {
				It("stalls, stops calling APIM, and logs the stable line once", func() {
					waits := h.failTransiently(5)
					Expect(waits).To(Equal([]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 0}))
					Expect(h.puts()).To(Equal(5))
					Expect(h.tokens()).To(Equal(5))

					v := h.view()
					Expect(v.Phase).To(Equal(phaseStalled))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(5)))
					Expect(v.Retry.NextAttemptAt).To(BeEmpty())
					Expect(v.Message).To(HavePrefix(k.failurePrefix))
					Expect(v.Message).To(ContainSubstring("stalled after 5 failures"))
					Expect(v.Message).To(ContainSubstring(retryAnnotation))

					stalled := h.linesWith("🛑 APIM write stalled")
					Expect(stalled).To(HaveLen(1))
					h.expectIdentity(stalled[0])
					Expect(stalled[0].err).To(BeTrue())
					Expect(stalled[0].kv).To(HaveKeyWithValue("attempts", int32(5)))
					Expect(stalled[0].kv).To(HaveKeyWithValue("lastError", ContainSubstring("ServiceUnavailable")))

					failed := h.linesWith(msgWriteFailed)
					Expect(failed).To(HaveLen(4))
					for i, l := range failed {
						Expect(l.kv).To(HaveKeyWithValue("attempt", fmt.Sprintf("%d/5", i+1)))
					}
				})

				DescribeTable("stays stopped however long it waits, without a token or a status write",
					func(wait time.Duration) {
						h.failTransiently(5)
						before := h.view()

						h.clock.advance(wait)
						Expect(h.reconcile()).To(BeZero(), "Stalled is not requeued")
						Expect(h.reconcile()).To(BeZero())
						Expect(h.puts()).To(Equal(5))
						Expect(h.tokens()).To(Equal(5))
						Expect(h.view()).To(Equal(before))
						Expect(h.linesWith("🛑 APIM write stalled")).To(HaveLen(1), "the stall is logged once, not per reconcile")

						held := h.linesWith(msgWriteHeld)
						Expect(held).To(HaveLen(2))
						h.expectIdentity(held[0])
						Expect(held[0].kv).To(HaveKeyWithValue("attempts", int32(5)))
						Expect(held[0].kv).To(HaveKeyWithValue("retryAnnotation", retryAnnotation))
					},
					Entry("at once", time.Duration(0)),
					Entry("an hour", time.Hour),
					Entry("a day", 24*time.Hour),
					Entry("thirty days", 30*24*time.Hour),
				)

				It("stalls on the fifth failure even when every failure is different", func() {
					h.arm.reply(
						tpeFail(http.StatusConflict, "Conflict"),
						tpeFail(http.StatusPreconditionFailed, "PreconditionFailed"),
						tpeFail(http.StatusUnprocessableEntity, "ManagementApiRequestFailed"),
						tpeHangup,
						tpeFail(http.StatusInternalServerError, "InternalServerError"),
					)
					for range 5 {
						h.clock.advance(h.reconcile().RequeueAfter)
					}
					v := h.view()
					Expect(v.Phase).To(Equal(phaseStalled))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(5)))
					stalled := h.linesWith(msgWriteStalled)
					Expect(stalled).To(HaveLen(1))
					Expect(stalled[0].kv).To(HaveKeyWithValue("lastError", ContainSubstring("InternalServerError")))
				})

				It("honours a smaller MaxAttempts", func() {
					h.policy.MaxAttempts = 2
					Expect(h.failTransiently(2)).To(Equal([]time.Duration{time.Minute, 0}))
					v := h.view()
					Expect(v.Phase).To(Equal(phaseStalled))
					Expect(v.Message).To(ContainSubstring("stalled after 2 failures"))
					Expect(h.linesWith(msgWriteStalled)[0].kv).To(HaveKeyWithValue("attempts", int32(2)))
				})

				It("stalls when a resource already at four failures fails once more", func() {
					h.setStatus(phaseBackoff, -1, apimv1.RetryStatus{
						ConsecutiveFailures: 4,
						NextAttemptAt:       tpeStart.Add(-time.Second).Format(time.RFC3339),
					})
					h.arm.reply(tpeFail(http.StatusTooManyRequests, "TooManyRequests"))
					Expect(h.reconcile()).To(BeZero())
					Expect(h.view().Phase).To(Equal(phaseStalled))
					Expect(h.view().Retry.ConsecutiveFailures).To(Equal(int32(5)))
				})
			})

			// ---------------------------------------------------------------- invalid

			Context("on a permanent error", func() {
				permanentEntries := []any{
					Entry("400 ValidationError", tpeFail(http.StatusBadRequest, "ValidationError"), "ValidationError"),
					Entry("400 without a body code", tpeFail(http.StatusBadRequest, ""), "400"),
					Entry("400 whose detail code is not transient", tpeFailDetail(http.StatusBadRequest, "ValidationError", "InvalidXml"), "InvalidXml"),
					Entry("401 InvalidAuthenticationToken", tpeFail(http.StatusUnauthorized, "InvalidAuthenticationToken"), "InvalidAuthenticationToken"),
					Entry("403 LinkedAuthorizationFailed", tpeFail(http.StatusForbidden, "LinkedAuthorizationFailed"), "LinkedAuthorizationFailed"),
				}
				if k.kind == "APIMTag" {
					// The tag's own path: a 404 means the APIM service is not there. A policy's
					// 404 is about the API it hangs off; see "when its API is not in APIM yet".
					permanentEntries = append(permanentEntries,
						Entry("404 ResourceNotFound on the PUT", tpeFail(http.StatusNotFound, "ResourceNotFound"), "ResourceNotFound"),
						Entry("404 without a body code", tpeFail(http.StatusNotFound, ""), "404"),
					)
				}
				invalidAtOnce := func(reply tpeReply, wantInMessage string) {
					h.arm.reply(reply)

					Expect(h.reconcile()).To(BeZero(), "Invalid is not requeued")
					v := h.view()
					Expect(v.Phase).To(Equal(phaseInvalid))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)))
					Expect(v.Retry.NextAttemptAt).To(BeEmpty())
					Expect(v.ObservedGeneration).To(Equal(v.Generation))
					Expect(v.Message).To(HavePrefix(k.failurePrefix))
					Expect(v.Message).To(ContainSubstring(wantInMessage))
					Expect(v.Message).To(ContainSubstring("not retrying"))
					Expect(v.Message).To(ContainSubstring(retryAnnotation))

					rejected := h.linesWith("💔 APIM write rejected; not retrying")
					Expect(rejected).To(HaveLen(1))
					h.expectIdentity(rejected[0])
					Expect(rejected[0].kv).To(HaveKeyWithValue("class", errorClassPermanent))
					Expect(h.linesWith(msgWriteStalled)).To(BeEmpty())
					Expect(h.linesWith(msgWriteFailed)).To(BeEmpty())

					for _, wait := range []time.Duration{0, time.Hour, 7 * 24 * time.Hour} {
						h.clock.advance(wait)
						Expect(h.reconcile()).To(BeZero())
					}
					Expect(h.puts()).To(Equal(1), "an Invalid resource is not written again")
					Expect(h.tokens()).To(Equal(1))
					Expect(h.view()).To(Equal(v))
				}
				DescribeTable("goes Invalid at once and is not written again",
					append([]any{invalidAtOnce}, permanentEntries...)...)

				It("goes Invalid after earlier transient failures, keeping the count", func() {
					h.failTransiently(2)
					h.arm.reply(tpeFail(http.StatusForbidden, "LinkedAuthorizationFailed"))
					Expect(h.reconcile()).To(BeZero())
					v := h.view()
					Expect(v.Phase).To(Equal(phaseInvalid))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(3)))
					Expect(v.Retry.NextAttemptAt).To(BeEmpty())
					Expect(h.puts()).To(Equal(3))
				})

				It("does not count a permanent error towards a stall", func() {
					h.failTransiently(4)
					h.arm.reply(tpeFail(http.StatusBadRequest, "ValidationError"))
					Expect(h.reconcile()).To(BeZero())
					Expect(h.view().Phase).To(Equal(phaseInvalid), "permanent wins over the fifth attempt")
					Expect(h.linesWith(msgWriteStalled)).To(BeEmpty())
				})
			})

			// ---------------------------------------------------------------- missing API

			if k.kind == "APIMInboundPolicy" {
				Context("when its API is not in APIM yet", func() {
					// A policy applied in the same sync as a new API is reconciled at once,
					// while the API's import still waits for a ready pod. Nothing re-triggers
					// the policy when the import lands, so it must retry on its own.
					apiMissing := tpeFail(http.StatusNotFound, "ResourceNotFound")

					It("backs off instead of going Invalid, and is written once the API exists", func() {
						h.arm.reply(apiMissing)
						Expect(h.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
						v := h.view()
						Expect(v.Phase).To(Equal(phaseBackoff))
						Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)))
						Expect(v.Message).To(ContainSubstring("ResourceNotFound"))
						Expect(h.linesWith(msgWriteRejected)).To(BeEmpty())
						failed := h.linesWith(msgWriteFailed)
						Expect(failed).To(HaveLen(1))
						Expect(failed[0].kv).To(HaveKeyWithValue("class", errorClassTransient))

						By("the import landing before the next attempt")
						h.arm.reply(tpeOK)
						h.clock.advance(time.Minute)
						Expect(h.reconcile()).To(BeZero())
						v = h.view()
						Expect(v.Phase).To(Equal(phaseCreated))
						Expect(v.Retry).To(BeZero())
						Expect(h.puts()).To(Equal(2))
					})

					It("never stalls on 404s: past MaxAttempts it waits MaxDelay and keeps one attempt in hand", func() {
						h.arm.reply(apiMissing)
						waits := make([]time.Duration, 0, 8)
						for range 8 {
							result := h.reconcile()
							waits = append(waits, result.RequeueAfter)
							h.clock.advance(result.RequeueAfter)
						}
						Expect(waits).To(Equal([]time.Duration{
							time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
							30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
						}), "past MaxAttempts the wait is MaxDelay")
						v := h.view()
						Expect(v.Phase).To(Equal(phaseBackoff))
						Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(4)), "the count stops one short of the limit")
						Expect(v.Retry.NextAttemptAt).NotTo(BeEmpty())
						Expect(h.linesWith(msgWriteStalled)).To(BeEmpty())
						failed := h.linesWith(msgWriteFailed)
						Expect(failed).To(HaveLen(8))
						for i, l := range failed {
							Expect(l.kv).To(HaveKeyWithValue("attempt", fmt.Sprintf("%d/5", min(i+1, 4))))
						}
						Expect(h.puts()).To(Equal(8))

						By("stalling at once on a failure of another kind after the wait")
						h.arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
						Expect(h.reconcile()).To(BeZero())
						v = h.view()
						Expect(v.Phase).To(Equal(phaseStalled))
						Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(5)))

						By("the import landing, after the retry annotation")
						h.arm.reply(tpeOK)
						h.annotate("api-is-there")
						Expect(h.reconcile()).To(BeZero())
						v = h.view()
						Expect(v.Phase).To(Equal(phaseCreated))
						Expect(v.Retry.ConsecutiveFailures).To(BeZero())
					})
				})
			}

			// ---------------------------------------------------------------- generation reset

			Context("when the spec changes", func() {
				It("writes at once from the middle of a backoff and clears the failures on success", func() {
					h.failTransiently(1)
					h.failNow(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
					h.clock.advance(time.Minute)
					Expect(h.view().Phase).To(Equal(phaseBackoff))
					Expect(h.view().Retry.NextAttemptAt).NotTo(BeEmpty())

					h.arm.reply(tpeOK)
					h.changeSpec(2)
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(3), "no waiting for nextAttemptAt")
					v := h.view()
					Expect(v.Phase).To(Equal(phaseCreated))
					Expect(v.Retry).To(BeZero())
					Expect(v.ObservedGeneration).To(Equal(v.Generation))

					reset := h.linesWith(msgWriteReset)
					Expect(reset).To(HaveLen(1))
					h.expectIdentity(reset[0])
					Expect(reset[0].kv).To(HaveKeyWithValue("reason", "spec changed"))
					Expect(reset[0].kv).To(HaveKeyWithValue("previousFailures", int32(2)))
				})

				It("sends the new spec", func() {
					h.changeSpec(7)
					Expect(h.reconcile()).To(BeZero())
					body := h.arm.all()[0].body
					if k.kind == "APIMTag" {
						Expect(body).To(ContainSubstring("TPE Tag v7"))
					} else {
						Expect(body).To(ContainSubstring("x-tpe"))
					}
				})

				DescribeTable("starts a stopped resource over, counting the next failure from one",
					func(stop func(*tpeHarness), wantPhase string) {
						stop(h)
						Expect(h.view().Phase).To(Equal(wantPhase))
						puts := h.puts()

						h.arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
						h.changeSpec(3)
						Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute))
						Expect(h.puts()).To(Equal(puts + 1))
						v := h.view()
						Expect(v.Phase).To(Equal(phaseBackoff))
						Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)), "the earlier failures were cleared first")
						Expect(v.ObservedGeneration).To(Equal(v.Generation))
						Expect(v.Message).To(ContainSubstring("attempt 1/5"))
					},
					Entry("Stalled", func(h *tpeHarness) { h.failTransiently(5) }, phaseStalled),
					Entry("Invalid", func(h *tpeHarness) {
						h.arm.reply(tpeFail(http.StatusBadRequest, "ValidationError"))
						h.reconcile()
					}, phaseInvalid),
				)

				It("stays Invalid when the new spec is rejected too, with the count back at one", func() {
					h.arm.reply(tpeFail(http.StatusBadRequest, "ValidationError"))
					h.reconcile()
					h.changeSpec(2)
					Expect(h.reconcile()).To(BeZero())
					v := h.view()
					Expect(v.Phase).To(Equal(phaseInvalid))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)))
					Expect(v.ObservedGeneration).To(Equal(v.Generation))
					Expect(h.puts()).To(Equal(2))
				})

				It("stalls again after five more failures on the new spec", func() {
					h.failTransiently(5)
					h.changeSpec(2)
					h.failTransiently(5)
					Expect(h.puts()).To(Equal(10))
					Expect(h.view().Phase).To(Equal(phaseStalled))
					Expect(h.linesWith(msgWriteStalled)).To(HaveLen(2))
				})

				It("only resets once per generation: the next reconcile honours the new backoff", func() {
					h.failTransiently(5)
					h.changeSpec(2)
					h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))
					Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute))
					h.clock.advance(10 * time.Second)
					Expect(h.reconcile().RequeueAfter).To(Equal(50 * time.Second))
					Expect(h.puts()).To(Equal(6))
				})

				It("treats a status observedGeneration behind the generation as a spec change", func() {
					h.setStatus(phaseStalled, 0, apimv1.RetryStatus{ConsecutiveFailures: 5})
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(1))
					v := h.view()
					Expect(v.Phase).To(Equal(phaseCreated))
					Expect(v.ObservedGeneration).To(Equal(v.Generation))
				})

				DescribeTable("a metadata-only change is not a spec change",
					func(stop func(*tpeHarness), wantRequeue bool) {
						stop(h)
						before := h.view()
						puts := h.puts()

						h.label("x")
						result := h.reconcile()
						if wantRequeue {
							Expect(result.RequeueAfter).To(BeNumerically(">", 0), "still backing off")
						} else {
							Expect(result).To(BeZero(), "a stopped resource is not requeued")
						}
						Expect(h.puts()).To(Equal(puts))
						Expect(h.view().Retry).To(Equal(before.Retry))
						Expect(h.view().Phase).To(Equal(before.Phase))
					},
					Entry("while backing off", func(h *tpeHarness) { h.failNow(tpeFail(http.StatusConflict, "Conflict")) }, true),
					Entry("while Stalled", func(h *tpeHarness) { h.failTransiently(5) }, false),
					Entry("while Invalid", func(h *tpeHarness) {
						h.arm.reply(tpeFail(http.StatusForbidden, "LinkedAuthorizationFailed"))
						h.reconcile()
					}, false),
				)
			})

			// ---------------------------------------------------------------- retry annotation

			Context("when the retry annotation is set", func() {
				DescribeTable("retries a stopped resource at once and records the value",
					func(stop func(*tpeHarness)) {
						stop(h)
						puts := h.puts()

						h.arm.reply(tpeOK)
						h.annotate("1")
						Expect(h.reconcile()).To(BeZero())
						Expect(h.puts()).To(Equal(puts + 1))
						v := h.view()
						Expect(v.Phase).To(Equal(phaseCreated))
						Expect(v.Retry.ConsecutiveFailures).To(BeZero())
						Expect(v.Retry.NextAttemptAt).To(BeEmpty())
						Expect(v.Retry.LastRetryAnnotation).To(Equal("1"))

						reset := h.linesWith(msgWriteReset)
						Expect(reset).To(HaveLen(1))
						Expect(reset[0].kv).To(HaveKeyWithValue("reason", retryAnnotation+" annotation set"))
					},
					Entry("Stalled", func(h *tpeHarness) { h.failTransiently(5) }),
					Entry("Invalid", func(h *tpeHarness) {
						h.arm.reply(tpeFail(http.StatusUnauthorized, "InvalidAuthenticationToken"))
						h.reconcile()
					}),
					Entry("backing off, before nextAttemptAt", func(h *tpeHarness) {
						h.failTransiently(2)
						h.failNow(tpeFail(http.StatusConflict, "Conflict"))
						h.clock.advance(time.Second)
					}),
				)

				It("acts once per value: the same value does not retrigger, a new one does", func() {
					h.failTransiently(5)

					By("setting the annotation on a Stalled resource")
					h.arm.reply(tpeFail(http.StatusBadRequest, "ValidationError"))
					h.annotate("a")
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(6))
					v := h.view()
					Expect(v.Phase).To(Equal(phaseInvalid))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)), "the five stalled failures were cleared first")
					Expect(v.Retry.LastRetryAnnotation).To(Equal("a"))

					By("reconciling again and again with the same value")
					for range 3 {
						h.clock.advance(time.Hour)
						Expect(h.reconcile()).To(BeZero())
					}
					Expect(h.puts()).To(Equal(6))

					By("setting a new value")
					h.arm.reply(tpeOK)
					h.annotate("b")
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(7))
					v = h.view()
					Expect(v.Phase).To(Equal(phaseCreated))
					Expect(v.Retry.LastRetryAnnotation).To(Equal("b"))
				})

				It("goes back to an earlier value as a new request", func() {
					h.arm.reply(tpeFail(http.StatusBadRequest, "ValidationError"))
					h.annotate("1")
					h.reconcile()
					h.annotate("2")
					h.reconcile()
					Expect(h.puts()).To(Equal(2))
					h.annotate("1")
					h.reconcile()
					Expect(h.puts()).To(Equal(3), "1 differs from the last handled value 2")
					Expect(h.view().Retry.LastRetryAnnotation).To(Equal("1"))
				})

				It("does not retry when the annotation is removed", func() {
					h.arm.reply(tpeFail(http.StatusForbidden, "LinkedAuthorizationFailed"))
					h.annotate("x")
					h.reconcile()
					Expect(h.view().Phase).To(Equal(phaseInvalid))

					h.annotate("")
					Expect(h.reconcile()).To(BeZero())
					Expect(h.puts()).To(Equal(1))
					v := h.view()
					Expect(v.Phase).To(Equal(phaseInvalid))
					Expect(v.Retry.LastRetryAnnotation).To(Equal("x"), "the last handled value is remembered")
				})

				It("retries a Stalled resource that then stalls again after five more failures", func() {
					h.failTransiently(5)
					h.annotate("again")
					h.failTransiently(5)
					Expect(h.puts()).To(Equal(10))
					v := h.view()
					Expect(v.Phase).To(Equal(phaseStalled))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(5)))
					Expect(v.Retry.LastRetryAnnotation).To(Equal("again"))
					Expect(h.linesWith(msgWriteStalled)).To(HaveLen(2))
				})

				It("records a value set on a healthy resource without logging a reset", func() {
					h.annotate("noop")
					Expect(h.reconcile()).To(BeZero())
					v := h.view()
					Expect(v.Phase).To(Equal(phaseCreated))
					Expect(v.Retry.LastRetryAnnotation).To(Equal("noop"))
					Expect(h.linesWith(msgWriteReset)).To(BeEmpty(), "nothing to clear")

					By("failing later: the old value does not skip the backoff")
					h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))
					Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute))
					Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute))
					Expect(h.puts()).To(Equal(2))
				})

				It("records the value when the write fails, so it does not retrigger", func() {
					h.failTransiently(5)
					h.arm.reply(tpeFail(http.StatusConflict, "Conflict"))
					h.annotate("r1")
					Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute))
					v := h.view()
					Expect(v.Phase).To(Equal(phaseBackoff))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(1)))
					Expect(v.Retry.LastRetryAnnotation).To(Equal("r1"))

					Expect(h.reconcile().RequeueAfter).To(Equal(time.Minute), "still backing off: same value, no reset")
					Expect(h.puts()).To(Equal(6))
				})
			})

			// ---------------------------------------------------------------- outside the state machine

			Context("outside the retry state machine", func() {
				It("keeps the token-failure path as it is: Error, 30 s, no ARM call, failures kept", func() {
					h.setStatus(phaseBackoff, -1, apimv1.RetryStatus{
						ConsecutiveFailures: 2,
						NextAttemptAt:       tpeStart.Add(-time.Second).Format(time.RFC3339),
					})
					h.tokenErr = errors.New("no token for you")
					result := h.reconcile()
					Expect(result.RequeueAfter).To(Equal(30 * time.Second))
					Expect(h.arm.calls()).To(BeZero())
					v := h.view()
					Expect(v.Phase).To(Equal(phaseError))
					Expect(v.Message).To(Equal(errMsgFailedToGetAzureToken))
					Expect(v.Retry.ConsecutiveFailures).To(Equal(int32(2)), "a token failure is not an APIM write failure")
				})

				It("does not fetch a token for a Stalled resource, so a broken identity costs nothing", func() {
					h.failTransiently(5)
					h.tokenErr = errors.New("no token for you")
					Expect(h.reconcile()).To(BeZero())
					Expect(h.tokens()).To(Equal(5))
					Expect(h.view().Phase).To(Equal(phaseStalled))
				})

				It("keeps the missing-identity path as it is", func() {
					Expect(os.Unsetenv("AZURE_CLIENT_ID")).To(Succeed())
					result := h.reconcile()
					Expect(result.RequeueAfter).To(Equal(30 * time.Second))
					Expect(h.arm.calls()).To(BeZero())
					Expect(h.tokens()).To(BeZero())
					Expect(h.view().Phase).To(Equal(phaseError))
				})

				It("keeps the missing-APIMService path as it is", func() {
					Expect(k8sClient.Delete(h.ctx, &apimv1.APIMService{
						ObjectMeta: metav1.ObjectMeta{Name: h.service, Namespace: getOperatorNamespace()},
					})).To(Succeed())
					result := h.reconcile()
					Expect(result.RequeueAfter).To(Equal(requeueMissingAPIMService))
					Expect(h.arm.calls()).To(BeZero())
					Expect(h.tokens()).To(BeZero())
					Expect(h.view().Phase).To(Equal(phaseError))
				})

				// The missing-APIMService and missing-identity paths run before the retry gate
				// and write phase Error. Once the cause is gone, the held reconcile must put
				// Stalled or Invalid back, not leave the stale error and its message in place.
				DescribeTable("a stopped resource that passed through an Error path still reports why it is stopped",
					func(stop func(*tpeHarness), wantPhase string, breakIt func(*tpeHarness), fixIt func(*tpeHarness)) {
						stop(h)
						Expect(h.view().Phase).To(Equal(wantPhase))
						puts := h.puts()

						breakIt(h)
						Expect(h.reconcile().RequeueAfter).To(BeNumerically(">", 0), "the Error paths keep today's requeue")
						Expect(h.view().Phase).To(Equal(phaseError))

						fixIt(h)
						Expect(h.reconcile()).To(BeZero())
						Expect(h.puts()).To(Equal(puts), "still held: no APIM call")
						v := h.view()
						Expect(v.Phase).To(Equal(wantPhase), "phase should say the resource is still stopped, not the stale Error")
						Expect(v.Message).NotTo(Or(
							ContainSubstring("not found"),
							Equal(errMsgMissingAzureIdentity),
						), "message should not keep the resolved cause")
					},
					Entry("Stalled, APIMService deleted and recreated",
						func(h *tpeHarness) { h.failTransiently(5) }, phaseStalled,
						tpeDeleteService, tpeRecreateService),
					Entry("Invalid, APIMService deleted and recreated",
						func(h *tpeHarness) { h.failNow403() }, phaseInvalid,
						tpeDeleteService, tpeRecreateService),
					Entry("Stalled, identity env unset and restored",
						func(h *tpeHarness) { h.failTransiently(5) }, phaseStalled,
						tpeUnsetIdentity, tpeRestoreIdentity),
				)

				It("does nothing for a deleted resource", func() {
					obj := h.object()
					Expect(k8sClient.Delete(h.ctx, obj)).To(Succeed())
					Expect(h.reconcile()).To(BeZero())
					Expect(h.arm.calls()).To(BeZero())
					Expect(h.tokens()).To(BeZero())
				})
			})
		})
	}

	Context("with the retry annotation set before the first reconcile", func() {
		for _, k := range tpeKinds() {
			It(k.label()+" writes and records the value", func() {
				h := newTpeHarness(k, map[string]string{retryAnnotation: "from-create"})
				Expect(h.reconcile()).To(BeZero())
				Expect(h.puts()).To(Equal(1))
				v := h.view()
				Expect(v.Phase).To(Equal(phaseCreated))
				Expect(v.Retry.LastRetryAnnotation).To(Equal("from-create"))
			})
		}
	})
})

// ---------------------------------------------------------------------- predicates

var _ = Describe("APIMTag and APIMInboundPolicy update predicates for the retry matrix", func() {
	for _, k := range tpeKinds()[:2] {
		Context(k.label(), func() {
			// build returns an object of the kind at generation 1 with these annotations.
			build := func(annotations map[string]string) client.Object {
				var obj client.Object
				if k.kind == "APIMTag" {
					obj = &apimv1.APIMTag{Spec: apimv1.APIMTagSpec{APIMService: "s", TagID: "t", DisplayName: "d"}}
				} else {
					obj = &apimv1.APIMInboundPolicy{Spec: apimv1.APIMInboundPolicySpec{APIMService: "s", APIID: "a", PolicyContent: tpePolicyXML}}
				}
				obj.SetName("n")
				obj.SetNamespace("default")
				obj.SetGeneration(1)
				obj.SetAnnotations(annotations)
				return obj
			}
			specChanged := func(obj client.Object) {
				obj.SetGeneration(2)
				switch o := obj.(type) {
				case *apimv1.APIMTag:
					o.Spec.DisplayName = "changed"
				case *apimv1.APIMInboundPolicy:
					o.Spec.PolicyContent = "<policies />"
				}
			}
			labelled := func(obj client.Object) { obj.SetLabels(map[string]string{"a": "b"}) }
			statusChanged := func(obj client.Object) {
				switch o := obj.(type) {
				case *apimv1.APIMTag:
					o.Status.Phase = phaseStalled
					o.Status.ConsecutiveFailures = 5
				case *apimv1.APIMInboundPolicy:
					o.Status.Phase = phaseStalled
					o.Status.ConsecutiveFailures = 5
				}
			}

			DescribeTable("decides which updates reach Reconcile",
				func(oldAnnotations, newAnnotations map[string]string, mutate func(client.Object), want bool) {
					oldObj, newObj := build(oldAnnotations), build(newAnnotations)
					if mutate != nil {
						mutate(newObj)
					}
					Expect(k.updateFilter(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})).To(Equal(want))
				},
				Entry("retry annotation added", nil, map[string]string{retryAnnotation: "1"}, nil, true),
				Entry("retry annotation changed", map[string]string{retryAnnotation: "1"}, map[string]string{retryAnnotation: "2"}, nil, true),
				Entry("retry annotation unchanged, another annotation changed",
					map[string]string{retryAnnotation: "1", "o": "1"}, map[string]string{retryAnnotation: "1", "o": "2"}, nil, false),
				Entry("another annotation added", nil, map[string]string{"o": "1"}, nil, false),
				Entry("a label added", map[string]string{retryAnnotation: "1"}, map[string]string{retryAnnotation: "1"}, labelled, false),
				Entry("a status-only update", map[string]string{retryAnnotation: "1"}, map[string]string{retryAnnotation: "1"}, statusChanged, false),
				Entry("nothing changed", nil, nil, nil, false),
				Entry("the spec changed", nil, nil, specChanged, true),
				Entry("the spec and the retry annotation changed", nil, map[string]string{retryAnnotation: "x"}, specChanged, true),
			)
		})
	}
})

// ---------------------------------------------------------------------- through a manager

var _ = Describe("APIMTag and APIMInboundPolicy retry handling through a running manager", func() {
	for _, k := range tpeKinds()[:2] {
		Context(k.label(), func() {
			var (
				ns    string
				arm   *tpeFakeARM
				key   types.NamespacedName
				start func(policy *retryPolicy)
				view  func() tpeView
				puts  func() int
			)

			BeforeEach(func() {
				n := tpeSeq.Add(1)
				ns = fmt.Sprintf("tpe-mgr-%d", n)
				Expect(k8sClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

				// The reconcilers look the APIMService up in the operator namespace; point it at
				// this spec's namespace, which is also the only one the manager's cache sees, so
				// resources other specs left behind are never reconciled here.
				previousNS, hadNS := os.LookupEnv("OPERATOR_NAMESPACE")
				Expect(os.Setenv("OPERATOR_NAMESPACE", ns)).To(Succeed())
				DeferCleanup(func() {
					if hadNS {
						_ = os.Setenv("OPERATOR_NAMESPACE", previousNS)
					} else {
						_ = os.Unsetenv("OPERATOR_NAMESPACE")
					}
				})
				DeferCleanup(stubAzureIdentityEnv())
				arm = newTpeFakeARM()
				DeferCleanup(arm.srv.Close)
				DeferCleanup(apim.UseEndpoint(arm.srv.URL, arm.srv.Client()))

				service := fmt.Sprintf("tpe-svc-%d", n)
				Expect(k8sClient.Create(context.Background(), &apimv1.APIMService{
					ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: ns},
					Spec:       apimv1.APIMServiceSpec{Name: service, ResourceGroup: tpeResourceGroup, Subscription: tpeSubscription},
				})).To(Succeed())
				key = types.NamespacedName{Name: fmt.Sprintf("tpe-mgr-%s-%d", strings.ToLower(k.kind), n), Namespace: ns}
				// Registered before the manager starts, so it runs after the manager has stopped.
				DeferCleanup(func() {
					obj := k.newObject()
					obj.SetName(key.Name)
					obj.SetNamespace(key.Namespace)
					Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj))).To(Succeed())
					Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), &apimv1.APIMService{
						ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: ns},
					}))).To(Succeed())
				})

				view = func() tpeView {
					GinkgoHelper()
					obj := k.newObject()
					Expect(k8sClient.Get(context.Background(), key, obj)).To(Succeed())
					return k.view(obj)
				}
				puts = func() int {
					n := 0
					for _, r := range arm.all() {
						if r.method == http.MethodPut && r.path == k.armPath(service) {
							n++
						}
					}
					return n
				}
				start = func(policy *retryPolicy) {
					GinkgoHelper()
					skip := true
					mgr, err := ctrl.NewManager(cfg, ctrl.Options{
						Scheme:                 scheme.Scheme,
						Logger:                 tpeQuietLogger(),
						Metrics:                metricsserver.Options{BindAddress: "0"},
						HealthProbeBindAddress: "0",
						Controller:             config.Controller{SkipNameValidation: &skip},
						Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
					})
					Expect(err).NotTo(HaveOccurred())
					token := func(context.Context, string, string) (string, error) { return tpeToken, nil }
					Expect(k.setup(mgr, token, policy)).To(Succeed())

					mgrCtx, cancel := context.WithCancel(context.Background())
					done := make(chan struct{})
					go func() {
						defer GinkgoRecover()
						defer close(done)
						Expect(mgr.Start(mgrCtx)).To(Succeed())
					}()
					DeferCleanup(func() {
						cancel()
						Eventually(done, 10*time.Second).Should(BeClosed())
					})
					k.create(context.Background(), key, service, nil)
				}
			})

			It("goes Invalid, ignores its own status updates and labels, and resumes on the retry annotation", func() {
				arm.reply(tpeFail(http.StatusBadRequest, "ValidationError"))
				clock := &fakeClock{t: tpeStart}
				start(&retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now})

				Eventually(func() string { return view().Phase }, 10*time.Second, 20*time.Millisecond).Should(Equal(phaseInvalid))
				Consistently(puts, 500*time.Millisecond, 20*time.Millisecond).Should(Equal(1),
					"the status patch must not trigger another write")

				By("adding a label: filtered out, and gated even if it got through")
				obj := k.newObject()
				Expect(k8sClient.Get(context.Background(), key, obj)).To(Succeed())
				obj.SetLabels(map[string]string{"tpe": "label"})
				Expect(k8sClient.Update(context.Background(), obj)).To(Succeed())
				Consistently(puts, 300*time.Millisecond, 20*time.Millisecond).Should(Equal(1))

				By("setting the retry annotation: the predicate lets it through and the write goes")
				arm.reply(tpeOK)
				Expect(k8sClient.Get(context.Background(), key, obj)).To(Succeed())
				obj.SetAnnotations(map[string]string{retryAnnotation: "go"})
				Expect(k8sClient.Update(context.Background(), obj)).To(Succeed())
				Eventually(func() string { return view().Phase }, 10*time.Second, 20*time.Millisecond).Should(Equal(phaseCreated))
				Expect(puts()).To(Equal(2))
				v := view()
				Expect(v.Retry.LastRetryAnnotation).To(Equal("go"))
				Expect(v.Retry.ConsecutiveFailures).To(BeZero())
				Consistently(puts, 300*time.Millisecond, 20*time.Millisecond).Should(Equal(2))

				By("changing the spec: written again")
				Expect(k8sClient.Get(context.Background(), key, obj)).To(Succeed())
				k.bumpSpec(obj, 9)
				Expect(k8sClient.Update(context.Background(), obj)).To(Succeed())
				Eventually(puts, 10*time.Second, 20*time.Millisecond).Should(Equal(3))
				Eventually(func() int64 { v := view(); return v.Generation - v.ObservedGeneration }, 10*time.Second, 20*time.Millisecond).Should(BeZero())
			})

			It("backs off by its own requeues to Stalled after five writes, then stops", func() {
				arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
				// A real clock and tiny delays: each requeue is at most the rounding of
				// nextAttemptAt up to the next whole second.
				start(&retryPolicy{BaseDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxAttempts: 5})

				Eventually(func() string { return view().Phase }, 20*time.Second, 50*time.Millisecond).Should(Equal(phaseStalled))
				Expect(view().Retry.ConsecutiveFailures).To(Equal(int32(5)))
				Expect(puts()).To(Equal(5))
				Consistently(puts, 1500*time.Millisecond, 50*time.Millisecond).Should(Equal(5), "Stalled is not requeued")

				By("setting the retry annotation with APIM healthy again")
				arm.reply(tpeOK)
				obj := k.newObject()
				Expect(k8sClient.Get(context.Background(), key, obj)).To(Succeed())
				obj.SetAnnotations(map[string]string{retryAnnotation: "1"})
				Expect(k8sClient.Update(context.Background(), obj)).To(Succeed())
				Eventually(func() string { return view().Phase }, 10*time.Second, 20*time.Millisecond).Should(Equal(phaseCreated))
				Expect(puts()).To(Equal(6))
			})
		})
	}
})
