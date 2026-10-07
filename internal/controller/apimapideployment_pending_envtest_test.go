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
)

// Specs for status.pendingImport: an import APIM accepted and was still running when the
// wait for it ended is waited for across reconciles, and the API is not written again
// until APIM reports how it ended. They use the fake ARM of deployment_envtest_helpers_test.go.
var _ = Describe("APIMAPIDeployment waiting for an import APIM is still running", func() {
	// pendingFixture is a deployment whose first reconcile leaves an import pending.
	pendingFixture := func() *depEnvFixture {
		GinkgoHelper()
		f := newDepEnvFixture(depEnvOptions{})
		depEnvAsync(20*time.Millisecond, 2*time.Millisecond)
		f.arm.on(depEnvImport, depEnvAccepted(""))
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusAccepted, `{"status":"InProgress"}`))
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
		Expect(f.get().Status.PendingImport).NotTo(BeNil())
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		return f
	}

	It("reads the running import on later reconciles instead of importing again, less often as it ages", func() {
		f := pendingFixture()

		f.clock.advance(minPendingImportPoll)
		polls := f.arm.count(depEnvPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
		Expect(f.arm.count(depEnvPoll)).To(Equal(polls+1), "one reading per reconcile")
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

		Expect(f.arm.count(depEnvImport)).To(Equal(1), "never a second import while the first runs")
		Expect(f.arm.count(depEnvServiceURL)).To(BeZero())
	})

	It("finishes an import that succeeds without importing again", func() {
		f := pendingFixture()
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(BeZero())

		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		Expect(f.arm.count(depEnvServiceURL)).To(Equal(1), "the steps after the import run once it is done")
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(status.AppliedHash).To(Equal(f.desiredHash(depEnvDocV1)))
		Expect(status.PendingImport).To(BeNil())
		Expect(status.ConsecutiveFailures).To(BeZero())
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
	})

	It("does not report the API in sync while an import is still running", func() {
		f := pendingFixture()
		deployment := f.get()
		deployment.Status.AppliedHash = f.desiredHash(depEnvDocV1)
		Expect(k8sClient.Status().Update(f.ctx, deployment)).To(Succeed())

		f.clock.advance(minPendingImportPoll)
		polls := f.arm.count(depEnvPoll)
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))

		Expect(f.arm.count(depEnvPoll)).To(Equal(polls+1), "the running import is read, not ignored")
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseImporting))
		Expect(status.Message).NotTo(ContainSubstring("No changes detected"))
	})

	It("imports the new definition once an import of an older one finishes", func() {
		f := pendingFixture()
		f.doc.publishNewVersion()
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(BeZero())

		Expect(f.arm.count(depEnvImport)).To(Equal(2), "the finished import was for the old definition")
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(status.AppliedHash).To(Equal(status.DesiredHash))
		Expect(status.AppliedHash).NotTo(Equal(f.desiredHash(depEnvDocV1)))
		Expect(status.PendingImport).To(BeNil())
	})

	It("starts over when APIM no longer knows the running import", func() {
		f := pendingFixture()
		var answered atomic.Int32
		f.arm.on(depEnvPoll, func(w http.ResponseWriter, _ *http.Request) bool {
			if answered.Add(1) == 1 {
				depEnvWriteError(w, http.StatusNotFound, "ResourceNotFound", "operation gone")
			} else {
				depEnvWriteJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
			}
			return true
		})

		f.clock.advance(minPendingImportPoll)
		Expect(f.reconcile()).To(BeZero())

		Expect(f.arm.count(depEnvImport)).To(Equal(2))
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
		Expect(status.PendingImport).To(BeNil())
		Expect(status.ConsecutiveFailures).To(BeZero())
	})
})
