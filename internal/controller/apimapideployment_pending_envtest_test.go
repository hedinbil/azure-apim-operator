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
	"net/http"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// Specs for status.pendingImport: every import APIM accepts with 202 is recorded the moment
// the 202 arrives and read on later reconciles, and the API is not written again until APIM
// reports how it ended. They use the fake ARM of deployment_envtest_helpers_test.go.
var _ = Describe("APIMAPIDeployment following an import APIM accepted", func() {
	// pendingFixture is a deployment whose first reconcile leaves an import pending.
	pendingFixture := func() *depEnvFixture {
		GinkgoHelper()
		f := newDepEnvFixture(depEnvOptions{})
		f.arm.on(depEnvImport, depEnvAccepted(""))
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusAccepted, `{"status":"InProgress"}`))
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
		Expect(f.get().Status.PendingImport).NotTo(BeNil())
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		Expect(f.arm.count(depEnvPoll)).To(BeZero(), "the reconcile that sent the import does not read it")
		return f
	}

	It("reads the running import on later reconciles instead of importing again, less often as it ages", func() {
		f := pendingFixture()

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
		Expect(f.arm.count(depEnvPoll)).To(Equal(1), "one reading per reconcile")
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseImporting))
		Expect(status.Message).To(ContainSubstring("still importing"))
		Expect(status.ConsecutiveFailures).To(BeZero())

		By("reading it every half of its age once it is older")
		f.clock.advance(20 * time.Minute)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: (20*time.Minute + minPendingImportPoll) / 2}))

		By("reading it at most every maxPendingImportPoll")
		f.clock.advance(time.Hour)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: maxPendingImportPoll}))

		Expect(f.arm.count(depEnvPoll)).To(Equal(3))
		Expect(f.arm.count(depEnvImport)).To(Equal(1), "never a second import while the first runs")
		Expect(f.arm.count(depEnvGetAPI)).To(Equal(1))
		Expect(f.arm.count(depEnvServiceDetails)).To(BeZero())
		Expect(f.arm.count(depEnvPatchAPI)).To(BeZero())
	})

	It("finishes an import that succeeds without importing again", func() {
		f := pendingFixture()
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(BeZero())

		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		Expect(f.arm.count(depEnvPoll)).To(Equal(1))
		Expect(f.arm.count(depEnvServiceDetails)).To(Equal(1), "the steps after the import run once it is done")
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(status.AppliedHash).To(Equal(f.desiredHash(depEnvDocV1)))
		Expect(status.PendingImport).To(BeNil())
		Expect(status.ConsecutiveFailures).To(BeZero())
		Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusOK))
	})

	It("counts an import that fails as a failed write and imports again after the backoff", func() {
		f := pendingFixture()
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK,
			`{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor"}}`))

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

		status := f.get().Status
		Expect(status.Phase).To(Equal(phaseBackoff))
		Expect(status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(status.LastError).To(ContainSubstring("DeadOperationMonitor"))
		Expect(status.PendingImport).To(BeNil())
		Expect(status.AppliedHash).To(BeEmpty())
		Expect(f.arm.count(depEnvImport)).To(Equal(1), "no import in the reconcile that learns of the failure")

		By("importing again once the backoff is over")
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusAccepted, `{"status":"InProgress"}`))
		f.toNextAttempt()
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
		Expect(f.arm.count(depEnvImport)).To(Equal(2))
		Expect(f.get().Status.PendingImport).NotTo(BeNil())
		Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(1)), "the running import keeps the count")
	})

	It("does not report the API in sync while an import is still running", func() {
		f := pendingFixture()
		deployment := f.get()
		deployment.Status.AppliedHash = f.desiredHash(depEnvDocV1)
		Expect(k8sClient.Status().Update(f.ctx, deployment)).To(Succeed())

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))

		Expect(f.arm.count(depEnvPoll)).To(Equal(1), "the running import is read, not ignored")
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseImporting))
		Expect(status.Message).NotTo(ContainSubstring("No changes detected"))
	})

	It("counts an import that finished for an older definition as a failed write, then imports the new one", func() {
		f := pendingFixture()
		f.doc.publishNewVersion()
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

		Expect(f.arm.count(depEnvImport)).To(Equal(1), "not at once: the retry policy decides when")
		status := f.get().Status
		Expect(status.Phase).To(Equal(phaseBackoff))
		Expect(status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(status.LastError).To(Equal(errPendingImportOutdated.Error()))
		Expect(status.PendingImport).To(BeNil())
		Expect(status.AppliedHash).To(BeEmpty())
		Expect(status.DesiredHash).To(Equal(f.desiredHash(depEnvDocV2)))

		By("importing the new definition once the backoff is over")
		f.arm.on(depEnvImport, nil)
		f.toNextAttempt()
		Expect(f.reconcile()).To(BeZero())
		Expect(f.arm.count(depEnvImport)).To(Equal(2))
		Expect(importedDocument(f.arm.last(depEnvImport).Body)).To(Equal(depEnvDocV2))
		status = f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(status.AppliedHash).To(Equal(f.desiredHash(depEnvDocV2)))
		Expect(status.RetryStatus.ConsecutiveFailures).To(BeZero())
	})

	It("counts an import APIM no longer knows as a failed write, then imports again", func() {
		f := pendingFixture()
		var answered atomic.Int32
		f.arm.on(depEnvPoll, func(w http.ResponseWriter, _ *http.Request) bool {
			answered.Add(1)
			depEnvWriteError(w, http.StatusNotFound, "ResourceNotFound", "operation gone")
			return true
		})

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

		Expect(answered.Load()).To(Equal(int32(1)))
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		status := f.get().Status
		Expect(status.Phase).To(Equal(phaseBackoff))
		Expect(status.ConsecutiveFailures).To(Equal(int32(1)))
		Expect(status.LastError).To(Equal(errPendingImportGone.Error()))
		Expect(status.PendingImport).To(BeNil())

		By("importing again once the backoff is over")
		f.arm.on(depEnvImport, nil)
		f.toNextAttempt()
		Expect(f.reconcile()).To(BeZero())
		Expect(f.arm.count(depEnvImport)).To(Equal(2))
		Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
	})

	It("counts a recorded operation URL that is not on ARM as a failed write and never reads it", func() {
		f := pendingFixture()
		deployment := f.get()
		deployment.Status.PendingImport.OperationURL = "https://attacker.example/op-1"
		Expect(k8sClient.Status().Update(f.ctx, deployment)).To(Succeed())

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

		Expect(f.arm.count(depEnvPoll)).To(BeZero())
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		status := f.get().Status
		Expect(status.Phase).To(Equal(phaseBackoff))
		Expect(status.LastError).To(Equal(errPendingImportUnusable.Error()))
		Expect(status.PendingImport).To(BeNil())
	})

	// Fix C: the operation is read before the age of the import is held against it.
	Context("with an import older than maxPendingImportAge", func() {
		It("finishes it when APIM reports it Succeeded, instead of timing it out", func() {
			f := pendingFixture()
			f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))

			f.clock.advance(maxPendingImportAge + time.Hour)
			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.count(depEnvPoll)).To(Equal(1))
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
			status := f.get().Status
			Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(status.ConsecutiveFailures).To(BeZero(), "a finished import is not a timed-out one")
			Expect(status.AppliedHash).To(Equal(f.desiredHash(depEnvDocV1)))
			Expect(status.PendingImport).To(BeNil())
		})

		It("counts it as a timed-out wait when APIM still reports it Running", func() {
			f := pendingFixture()

			f.clock.advance(maxPendingImportAge + time.Second)
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			Expect(f.arm.count(depEnvPoll)).To(Equal(1), "read once, then given up on")
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
			status := f.get().Status
			Expect(status.Phase).To(Equal(phaseBackoff))
			Expect(status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(status.LastError).To(ContainSubstring(apim.ErrImportWaitTimeout.Error()))
			Expect(status.PendingImport).To(BeNil())
		})

		It("counts it as a timed-out wait when reading it keeps failing", func() {
			f := pendingFixture()
			f.arm.on(depEnvPoll, depEnvFail(http.StatusConflict, "ManagementApiRequestFailed"))

			f.clock.advance(maxPendingImportAge + time.Second)
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			status := f.get().Status
			Expect(status.Phase).To(Equal(phaseBackoff))
			Expect(status.LastError).To(ContainSubstring(apim.ErrImportWaitTimeout.Error()))
			Expect(status.PendingImport).To(BeNil())
		})
	})

	// Fix G: a failed reading is not a failed import, whatever the code.
	DescribeTable("waits without counting a failure while reading the operation fails",
		func(responder depEnvResponder, wantInError string) {
			f := pendingFixture()
			f.arm.on(depEnvPoll, responder)

			for i := 1; i <= 4; i++ {
				f.clock.advance(minPendingImportPoll)
				result := f.reconcile()
				Expect(result.RequeueAfter).To(Equal(pendingImportPollDelay(depEnvAt(0), f.clock.now())))
				status := f.get().Status
				Expect(status.Phase).To(Equal(apimDeploymentPhaseImporting))
				Expect(status.Status).To(Equal(apimDeploymentStatusPending))
				Expect(status.ConsecutiveFailures).To(BeZero(), "reading %d", i)
				Expect(status.NextAttemptAt).To(BeEmpty())
				Expect(status.LastError).To(ContainSubstring(wantInError))
				Expect(status.PendingImport).NotTo(BeNil())
			}
			Expect(f.arm.count(depEnvPoll)).To(Equal(4))
			Expect(f.arm.count(depEnvImport)).To(Equal(1), "no new import while the first may be running")
			Expect(f.arm.count(depEnvGetAPI)).To(Equal(1))

			By("finishing once the reading works again")
			f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))
			f.clock.advance(minPendingImportPoll)
			Expect(f.reconcile()).To(BeZero())
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
		},
		Entry("409 Conflict", depEnvFail(http.StatusConflict, "Conflict"), "409"),
		Entry("409 ManagementApiRequestFailed", depEnvFail(http.StatusConflict, "ManagementApiRequestFailed"), "ManagementApiRequestFailed"),
		Entry("422 Timeout", depEnvFail(http.StatusUnprocessableEntity, "Timeout"), "Timeout"),
		Entry("500 InternalServerError", depEnvFail(http.StatusInternalServerError, "InternalServerError"), "500"),
		Entry("502 without a JSON body", depEnvFailBody(http.StatusBadGateway, "<html>bad gateway</html>"), "502"),
	)

	It("does not import again when a fresh reconciler finds an import pending, only reads it", func() {
		f := pendingFixture()

		By("starting a new reconciler, as an operator restart or a new leader would")
		restarted := &APIMAPIDeploymentReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			fetcher:  testOpenAPIFetcher(),
			getToken: f.reconciler.getToken,
			retry:    &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: f.clock.now},
		}
		f.reconciler = restarted
		calls := f.arm.total()

		f.clock.advance(time.Minute)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: pendingImportPollDelay(depEnvAt(0), f.clock.now())}))
		Expect(f.arm.stepsSince(calls)).To(Equal([]string{depEnvPoll}), "the operation is read; nothing is written")

		By("finishing the import the old process started")
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))
		f.clock.advance(time.Minute)
		calls = f.arm.total()
		Expect(f.reconcile()).To(BeZero())
		Expect(f.arm.stepsSince(calls)).To(Equal([]string{depEnvPoll, depEnvServiceDetails}))
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
	})

	It("keeps the accepted import and retries only the steps after it when one of them fails", func() {
		f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}})
		f.arm.on(depEnvImport, depEnvAccepted(""))
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))
		f.arm.on(depEnvProduct, depEnvFail(http.StatusConflict, "Conflict"))

		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
		status := f.get().Status
		Expect(status.Phase).To(Equal(phaseBackoff))
		Expect(status.Message).To(HavePrefix(depEnvMsgProducts + ": "))
		Expect(status.AppliedHash).To(BeEmpty())

		By("succeeding once the backoff is over")
		f.arm.on(depEnvProduct, nil)
		f.toNextAttempt()
		Expect(f.reconcile()).To(BeZero())
		Expect(f.arm.count(depEnvImport)).To(Equal(1), "the import APIM finished is not sent again")
		Expect(f.arm.count(depEnvProduct)).To(Equal(2))
		status = f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(status.PendingImport).To(BeNil())
		Expect(status.AppliedHash).To(Equal(status.DesiredHash))
	})

	// Fix B: before, an import that finished for an older document restarted at once and
	// cleared nothing, so a document that changes on every fetch imported without end.
	It("stalls after MaxAttempts imports when the document differs on every fetch and APIM answers 202 then Succeeded", func() {
		f := newDepEnvFixture(depEnvOptions{})
		f.doc.alternate(depEnvDocV1, depEnvDocV2)
		f.arm.on(depEnvImport, depEnvAccepted(""))
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))

		var requeues []time.Duration
		for i := 0; i < 40; i++ {
			result := f.reconcile()
			requeues = append(requeues, result.RequeueAfter)
			if result.IsZero() {
				break
			}
			f.clock.advance(result.RequeueAfter)
		}

		Expect(requeues).To(Equal([]time.Duration{
			minPendingImportPoll, time.Minute,
			minPendingImportPoll, 2 * time.Minute,
			minPendingImportPoll, 4 * time.Minute,
			minPendingImportPoll, 8 * time.Minute,
			minPendingImportPoll, 0,
		}), "every import that finished for a stale document counts against the retry policy")
		Expect(f.arm.count(depEnvImport)).To(Equal(5), "bounded: MaxAttempts imports, then Stalled")
		Expect(f.arm.count(depEnvPoll)).To(Equal(5))
		status := f.get().Status
		Expect(status.Phase).To(Equal(phaseStalled))
		Expect(status.ConsecutiveFailures).To(Equal(int32(5)))
		Expect(status.LastError).To(Equal(errPendingImportOutdated.Error()))
		Expect(status.PendingImport).To(BeNil())
	})
})
