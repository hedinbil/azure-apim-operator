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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs pin down how the outcome of an APIM write is recorded: in a context the
// reconcile's own cancellation cannot take away, never handed back as an error when only
// the recording failed, and on the APIMAPI that the team's ArgoCD app reads.

// errInjectedStatusPatch is the error a status patch fails with in these specs.
var errInjectedStatusPatch = errors.New("status patch refused by the test")

// statusPatchHook returns a client that passes everything through to the API server, but
// runs hook before every status patch: an error from it fails the patch, and a change it
// makes to the object is what is sent, which is how a field the server prunes looks to the
// caller.
func statusPatchHook(hook func(obj client.Object) error) client.Client {
	GinkgoHelper()
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if sub == "status" {
				if err := hook(obj); err != nil {
					return err
				}
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}

// statusPatchInterceptor is statusPatchHook for the patches selects picks: with fail set
// they fail with errInjectedStatusPatch, otherwise rewrite changes them before they are sent.
func statusPatchInterceptor(selects func(obj client.Object) bool, fail bool, rewrite func(obj client.Object)) client.Client {
	GinkgoHelper()
	return statusPatchHook(func(obj client.Object) error {
		if !selects(obj) {
			return nil
		}
		if fail {
			return errInjectedStatusPatch
		}
		rewrite(obj)
		return nil
	})
}

// deploymentWith selects APIMAPIDeployment status patches whose new status matches.
func deploymentWith(match func(apimv1.APIMAPIDeploymentStatus) bool) func(client.Object) bool {
	return func(obj client.Object) bool {
		d, ok := obj.(*apimv1.APIMAPIDeployment)
		return ok && match(d.Status)
	}
}

// cancelOnAccepted is an http.RoundTripper that lets onAccepted end the request's context
// right after APIM answered a write (PUT) with 202: the response is read in full first, so what the caller
// gets is the 202, but by the time it acts on it its context is done.
type cancelOnAccepted struct {
	base       http.RoundTripper
	onAccepted func(req *http.Request)
}

func (t cancelOnAccepted) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusAccepted || req.Method != http.MethodPut {
		return resp, err
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	t.onAccepted(req)
	return resp, nil
}

var _ = Describe("Recording the outcome of an APIMAPIDeployment write", func() {
	Context("when the reconcile's context ends right as APIM answers 202", func() {
		DescribeTable("still records the accepted import, so the next reconcile follows it instead of importing again",
			func(endContext func(cancel context.CancelFunc, req *http.Request), deadline time.Duration) {
				f := newDepEnvFixture(depEnvOptions{})
				f.arm.on(depEnvImport, depEnvAccepted(""))
				f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusAccepted, `{"status":"InProgress"}`))

				var (
					ctx    context.Context
					cancel context.CancelFunc
				)
				if deadline > 0 {
					ctx, cancel = context.WithTimeout(context.Background(), deadline)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				DeferCleanup(cancel)
				transport := cancelOnAccepted{base: f.arm.server.Client().Transport,
					onAccepted: func(req *http.Request) { endContext(cancel, req) }}
				DeferCleanup(apim.UseEndpoint(f.arm.server.URL, &http.Client{Transport: transport}))

				result, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})

				Expect(ctx.Err()).To(HaveOccurred(), "the reconcile's context ended during the reconcile")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
				status := f.get().Status
				Expect(status.PendingImport).NotTo(BeNil(), "the 202 must be recorded whatever happened to the reconcile")
				Expect(status.PendingImport.OperationURL).To(Equal(f.arm.server.URL + depEnvAsyncPath + "?api-version=2021-08-01"))
				Expect(status.PendingImport.DesiredHash).To(Equal(f.desiredHash(depEnvDocV1)))
				Expect(status.Phase).To(Equal(apimDeploymentPhaseImporting))
				Expect(status.ConsecutiveFailures).To(BeZero())

				By("following the import on the next reconcile, after a restart")
				f.clock.advance(minPendingImportPoll)
				Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
				Expect(f.arm.count(depEnvImport)).To(Equal(1), "never a second import")
				Expect(f.arm.count(depEnvPoll)).To(Equal(1))
			},
			Entry("cancelled (the operator is shutting down)",
				func(cancel context.CancelFunc, _ *http.Request) { cancel() }, time.Duration(0)),
			Entry("deadline exceeded (apimReconcileTimeout)",
				func(_ context.CancelFunc, req *http.Request) { <-req.Context().Done() }, 2*time.Second),
		)
	})

	Context("when status.pendingImport cannot be recorded", func() {
		recordsPending := deploymentWith(func(st apimv1.APIMAPIDeploymentStatus) bool { return st.PendingImport != nil })

		It("counts the import as failed and waits unknownWriteRetryFloor when the CRD prunes the field", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvAccepted(""))
			f.reconciler.Client = statusPatchInterceptor(recordsPending, false,
				func(obj client.Object) { obj.(*apimv1.APIMAPIDeployment).Status.PendingImport = nil })

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: unknownWriteRetryFloor}),
				"APIM may be running the import: keep well clear of it")

			status := f.get().Status
			Expect(status.Phase).To(Equal(phaseBackoff))
			Expect(status.ConsecutiveFailures).To(Equal(int32(1)))
			Expect(status.NextAttemptAt).To(Equal(depEnvAt(unknownWriteRetryFloor)))
			Expect(status.LastError).To(Equal(errPendingImportNotRecorded.Error()))
			Expect(status.PendingImport).To(BeNil())
			Expect(f.arm.count(depEnvImport)).To(Equal(1))

			By("not importing again before nextAttemptAt")
			f.clock.advance(unknownWriteRetryFloor - time.Minute)
			Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			By("stalling after MaxAttempts imports, never more")
			f.drive(8)
			status = f.get().Status
			Expect(status.Phase).To(Equal(phaseStalled))
			Expect(status.ConsecutiveFailures).To(Equal(int32(5)))
			Expect(f.arm.count(depEnvImport)).To(Equal(5))
			Expect(f.arm.count(depEnvPoll)).To(BeZero(), "an operation that was not recorded is never read")
		})

		It("retries the patch that records it, and follows the import once one goes through", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvAccepted(""))
			refused := 0
			f.reconciler.Client = statusPatchHook(func(obj client.Object) error {
				if recordsPending(obj) && refused < recordPendingImportAttempts-1 {
					refused++
					return errInjectedStatusPatch
				}
				return nil
			})

			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))

			Expect(refused).To(Equal(recordPendingImportAttempts - 1))
			status := f.get().Status
			Expect(status.PendingImport).NotTo(BeNil())
			Expect(status.Phase).To(Equal(apimDeploymentPhaseImporting))
			Expect(status.ConsecutiveFailures).To(BeZero())
		})

		It("returns the floor and no error when every patch that would keep it fails", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvAccepted(""))
			tries := 0
			f.reconciler.Client = statusPatchHook(func(obj client.Object) error {
				if recordsPending(obj) {
					tries++
					return errInjectedStatusPatch
				}
				return nil
			})

			result, err := f.reconciler.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: unknownWriteRetryFloor}))
			Expect(tries).To(Equal(recordPendingImportAttempts+1),
				"every attempt to record it, then the failure patch that carries it once more")
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
		})
	})

	Context("when the API is imported but a step after it fails", func() {
		// A product that is not in APIM yet: the import is not repeated, the product step
		// is retried alone, and the wait never stalls.
		DescribeTable("retries only the product assignment, never the import",
			func(async bool) {
				f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}})
				if async {
					f.arm.on(depEnvImport, depEnvAccepted(""))
					f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusOK, `{"status":"Succeeded"}`))
					Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))
					f.clock.advance(minPendingImportPoll)
				}
				f.arm.on(depEnvProduct, depEnvFail(http.StatusNotFound, "ResourceNotFound"))

				waits := make([]time.Duration, 0, 7)
				for range 7 {
					f.toNextAttempt()
					waits = append(waits, f.reconcile().RequeueAfter)
				}

				Expect(waits).To(Equal([]time.Duration{
					time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
					30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
				}))
				status := f.get().Status
				Expect(status.Phase).To(Equal(phaseBackoff), "a missing dependency never stalls")
				Expect(status.ConsecutiveFailures).To(Equal(int32(4)))
				Expect(status.ImportedHash).To(Equal(f.desiredHash(depEnvDocV1)))
				Expect(status.PendingImport).To(BeNil())
				Expect(status.AppliedHash).To(BeEmpty())
				Expect(f.arm.count(depEnvImport)).To(Equal(1))
				Expect(f.arm.count(depEnvGetAPI)).To(Equal(1))
				Expect(f.arm.count(depEnvProduct)).To(Equal(7))
				if async {
					Expect(f.arm.count(depEnvPoll)).To(Equal(1), "the finished import is not read again")
				}

				By("succeeding once the product exists")
				f.arm.on(depEnvProduct, nil)
				f.toNextAttempt()
				Expect(f.reconcile()).To(BeZero())
				status = f.get().Status
				Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
				Expect(status.AppliedHash).To(Equal(status.DesiredHash))
				Expect(f.arm.count(depEnvImport)).To(Equal(1))
			},
			Entry("after an import APIM finished at once", false),
			Entry("after an import APIM accepted with 202", true),
		)

		It("imports again once the desired state changes", func() {
			f := newDepEnvFixture(depEnvOptions{productIDs: []string{"p1"}})
			f.arm.on(depEnvProduct, depEnvFail(http.StatusConflict, "Conflict"))
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
			Expect(f.get().Status.ImportedHash).NotTo(BeEmpty())

			f.arm.on(depEnvProduct, nil)
			f.doc.publishNewVersion()
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.RoutePrefix = "/depenv-v2" })
			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.count(depEnvImport)).To(Equal(2))
			Expect(importedDocument(f.arm.last(depEnvImport).Body)).To(Equal(depEnvDocV2))
			status := f.get().Status
			Expect(status.ImportedHash).To(Equal(status.DesiredHash))
			Expect(status.AppliedHash).To(Equal(status.DesiredHash))
		})
	})

	It("waits on, with no error, when the status patch of a waiting reconcile fails", func() {
		f := newDepEnvFixture(depEnvOptions{})
		f.arm.on(depEnvImport, depEnvAccepted(""))
		f.arm.on(depEnvPoll, depEnvPollAnswer(http.StatusAccepted, `{"status":"InProgress"}`))
		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: minPendingImportPoll}))

		f.reconciler.Client = statusPatchInterceptor(
			deploymentWith(func(st apimv1.APIMAPIDeploymentStatus) bool { return st.Phase == apimDeploymentPhaseImporting }),
			true, nil)
		f.clock.advance(10 * time.Minute)
		result, err := f.reconciler.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key})

		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: pendingImportPollDelay(depEnvAt(0), f.clock.now())}))
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		Expect(f.arm.count(depEnvPoll)).To(Equal(1))
		Expect(f.get().Status.PendingImport).NotTo(BeNil(), "the pending import is still recorded")
	})

	Context("when recording the outcome fails", func() {
		It("returns no error and checks again after requeueUnrecordedSuccess when the success is not recorded", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.reconciler.Client = statusPatchInterceptor(
				deploymentWith(func(s apimv1.APIMAPIDeploymentStatus) bool { return s.Phase == apimDeploymentPhaseSucceeded }),
				true, nil)

			result, err := f.reconciler.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key})

			Expect(err).NotTo(HaveOccurred(), "an error would hand the writes above to the rate limiter")
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: requeueUnrecordedSuccess}))
			Expect(requeueUnrecordedSuccess).To(Equal(5 * time.Minute))
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
			status := f.get().Status
			Expect(status.Phase).To(Equal(apimDeploymentPhaseImporting), "the success was not recorded")
			Expect(status.AppliedHash).To(BeEmpty())
			Expect(f.getAPI().Status.Status).To(BeEmpty(), "the APIMAPI follows the deployment, not ahead of it")
		})

		It("still succeeds when only the APIMAPI status cannot be patched", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.reconciler.Client = statusPatchInterceptor(func(obj client.Object) bool {
				_, ok := obj.(*apimv1.APIMAPI)
				return ok
			}, true, nil)

			Expect(f.reconcile()).To(BeZero())

			status := f.get().Status
			Expect(status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(status.AppliedHash).To(Equal(status.DesiredHash))
			Expect(f.getAPI().Status.ApiHost).To(BeEmpty(), "the patch was refused")

			By("filling the hosts in on the next reconcile, without importing again")
			f.reconciler.Client = k8sClient
			calls := f.arm.total()
			imports := f.arm.count(depEnvImport)
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.stepsSince(calls)).To(Equal([]string{depEnvServiceDetails}))
			Expect(f.arm.count(depEnvImport)).To(Equal(imports))
			Expect(f.getAPI().Status.ApiHost).To(Equal("https://gw.depenv.net" + depEnvRoutePrefix))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusOK))

			By("then repairing nothing in APIM")
			calls = f.arm.total()
			Expect(f.reconcile()).To(BeZero())
			Expect(f.arm.total()).To(Equal(calls))
		})

		It("returns the backoff and no error when the failure cannot be recorded", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusConflict, "Conflict"))
			f.reconciler.Client = statusPatchInterceptor(
				deploymentWith(func(s apimv1.APIMAPIDeploymentStatus) bool { return s.Phase == phaseBackoff }),
				true, nil)

			result, err := f.reconciler.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key})

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: time.Minute}), "the requeue still waits out the backoff")
			Expect(f.get().Status.ConsecutiveFailures).To(BeZero(), "the count is lost with the patch")
			Expect(f.arm.count(depEnvImport)).To(Equal(1))
		})
	})

	Context("on the APIMAPI the team's ArgoCD app reads", func() {
		// importedOnce applies the deployment once, so its APIMAPI has a host and status OK.
		importedOnce := func(opts depEnvOptions) *depEnvFixture {
			GinkgoHelper()
			f := newDepEnvFixture(opts)
			Expect(f.reconcile()).To(BeZero())
			api := f.getAPI()
			Expect(api.Status.Status).To(Equal(apimAPIStatusOK))
			Expect(api.Status.ApiHost).NotTo(BeEmpty())
			return f
		}

		It("sets Error when a change goes Stalled, and OK again once it is applied", func() {
			f := importedOnce(depEnvOptions{tagIDs: []string{"t1"}})
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.ServiceURL = "https://backend-v2.depenv.net" })
			f.arm.on(depEnvTag, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))

			f.drive(4)
			Expect(f.get().Status.Phase).To(Equal(phaseBackoff))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusOK), "a write that is still backing off is not an error yet")
			f.drive(1)
			Expect(f.get().Status.Phase).To(Equal(phaseStalled))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusError))

			By("applying the change on the retry annotation")
			f.arm.on(depEnvTag, nil)
			f.annotate("fixed")
			Expect(f.reconcile()).To(BeZero())
			Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusOK))
		})

		It("sets Error when a change goes Invalid", func() {
			f := importedOnce(depEnvOptions{productIDs: []string{"p1"}})
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.RoutePrefix = "/depenv-v2" })
			f.arm.on(depEnvProduct, depEnvFail(http.StatusForbidden, "LinkedAuthorizationFailed"))

			Expect(f.reconcile()).To(BeZero())

			Expect(f.get().Status.Phase).To(Equal(phaseInvalid))
			api := f.getAPI()
			Expect(api.Status.Status).To(Equal(apimAPIStatusError))
			Expect(api.Status.ApiHost).To(Equal("https://gw.depenv.net"+depEnvRoutePrefix), "only status.status changes")
		})

		It("repairs Error to OK on an in-sync reconcile without calling APIM", func() {
			f := importedOnce(depEnvOptions{})
			api := f.getAPI()
			api.Status.Status = apimAPIStatusError
			Expect(k8sClient.Status().Update(f.ctx, api)).To(Succeed())
			calls := f.arm.total()

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.total()).To(Equal(calls))
			Expect(f.get().Status.Message).To(Equal("No changes detected; APIM is already in sync"))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusOK))
		})

		It("sets OK on an in-sync reconcile whatever the APIMAPI said before", func() {
			f := importedOnce(depEnvOptions{})
			api := f.getAPI()
			api.Status.Status = ""
			Expect(k8sClient.Status().Update(f.ctx, api)).To(Succeed())
			calls := f.arm.total()

			Expect(f.reconcile()).To(BeZero())

			Expect(f.arm.total()).To(Equal(calls))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusOK))
		})

		It("puts Error back on a held reconcile of a Stalled deployment, without calling APIM", func() {
			f := importedOnce(depEnvOptions{})
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.RoutePrefix = "/depenv-v2" })
			f.stall()
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusError))

			By("the Error not sticking, as when its patch failed when the write stopped")
			api := f.getAPI()
			api.Status.Status = apimAPIStatusOK
			Expect(k8sClient.Status().Update(f.ctx, api)).To(Succeed())

			f.clock.advance(time.Hour)
			Expect(f.reconcileWithoutAPIM()).To(BeZero())
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusError))
		})

		It("puts Error back on a held reconcile of an Invalid deployment", func() {
			f := importedOnce(depEnvOptions{})
			f.update(func(d *apimv1.APIMAPIDeployment) { d.Spec.RoutePrefix = "/depenv-v2" })
			f.arm.on(depEnvImport, depEnvFail(http.StatusBadRequest, "ValidationError"))
			Expect(f.reconcile()).To(BeZero())
			api := f.getAPI()
			api.Status.Status = apimAPIStatusOK
			Expect(k8sClient.Status().Update(f.ctx, api)).To(Succeed())

			Expect(f.reconcileWithoutAPIM()).To(BeZero())
			Expect(f.get().Status.Phase).To(Equal(phaseInvalid))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusError))
		})

		It("puts OK and the hosts back on an in-sync reconcile after the APIMAPI patch of a first success failed", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.reconciler.Client = statusPatchInterceptor(func(obj client.Object) bool {
				_, ok := obj.(*apimv1.APIMAPI)
				return ok
			}, true, nil)
			Expect(f.reconcile()).To(BeZero())

			f.reconciler.Client = k8sClient
			Expect(f.reconcile()).To(BeZero())

			api := f.getAPI()
			Expect(api.Status.Status).To(Equal(apimAPIStatusOK))
			Expect(api.Status.ApiHost).To(Equal("https://gw.depenv.net" + depEnvRoutePrefix))
		})

		It("sets Error on an APIMAPI that was never imported when its deployment goes Invalid", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusBadRequest, "ValidationError"))

			Expect(f.reconcile()).To(BeZero())

			Expect(f.get().Status.Phase).To(Equal(phaseInvalid))
			Expect(f.getAPI().Status.Status).To(Equal(apimAPIStatusError))
		})
	})
})

var _ = Describe("Recording the outcome of an APIMTag or APIMInboundPolicy write", func() {
	for _, k := range []tpeKind{tpeTagKind(), tpePolicyKind("")} {
		Context(k.kind, func() {
			// reconcilerWith builds the kind's reconciler on c.
			reconcilerWith := func(h *tpeHarness, c client.Client) reconcile.Reconciler {
				if k.kind == "APIMTag" {
					return &APIMTagReconciler{Client: c, Scheme: c.Scheme(), getToken: h.getToken, retry: h.policy}
				}
				return &APIMInboundPolicyReconciler{Client: c, Scheme: c.Scheme(), getToken: h.getToken, retry: h.policy}
			}
			// phaseIs selects status patches of the kind that set phase.
			phaseIs := func(phase string) func(client.Object) bool {
				return func(obj client.Object) bool {
					switch o := obj.(type) {
					case *apimv1.APIMTag:
						return o.Status.Phase == phase
					case *apimv1.APIMInboundPolicy:
						return o.Status.Phase == phase
					}
					return false
				}
			}

			It("returns no error and checks again after one backoff step when the success is not recorded", func() {
				h := newTpeHarness(k, nil)
				r := reconcilerWith(h, statusPatchInterceptor(phaseIs(phaseCreated), true, nil))

				result, err := r.Reconcile(h.ctx, reconcile.Request{NamespacedName: h.key})

				Expect(err).NotTo(HaveOccurred(), "an error would put the rate limiter in charge of writing again")
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: h.policy.BaseDelay}))
				Expect(h.puts()).To(Equal(1))
				Expect(h.view().Phase).NotTo(Equal(phaseCreated), "the success was not recorded")

				By("writing again and recording it once the API server takes the patch")
				Expect(h.reconcile()).To(BeZero())
				Expect(h.puts()).To(Equal(2))
				Expect(h.view().Phase).To(Equal(phaseCreated))
			})

			It("returns the backoff and no error when the failure cannot be recorded", func() {
				h := newTpeHarness(k, nil)
				h.arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
				r := reconcilerWith(h, statusPatchInterceptor(phaseIs(phaseBackoff), true, nil))

				result, err := r.Reconcile(h.ctx, reconcile.Request{NamespacedName: h.key})

				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))
				Expect(h.view().Retry.ConsecutiveFailures).To(BeZero(), "the count is lost with the patch")
				Expect(h.puts()).To(Equal(1))
			})
		})
	}
})

var _ = Describe("APIMAPIDeployment during a rolling update", func() {
	// rolloutOf makes the fixture's ReplicaSet revision 2 of a Deployment and adds revision
	// 1 of the same Deployment with readyOld ready pods.
	rolloutOf := func(f *depEnvFixture, readyOld int32) *appsv1.ReplicaSet {
		GinkgoHelper()
		yes := true
		owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: f.name, UID: types.UID(f.name + "-uid"), Controller: &yes}

		current := &appsv1.ReplicaSet{}
		Expect(k8sClient.Get(f.ctx, types.NamespacedName{Name: f.rsName, Namespace: f.ns}, current)).To(Succeed())
		current.OwnerReferences = []metav1.OwnerReference{owner}
		current.Annotations = map[string]string{replicaSetRevisionAnnotation: "2"}
		Expect(k8sClient.Update(f.ctx, current)).To(Succeed())

		one := int32(1)
		old := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: f.name + "-old", Namespace: f.ns,
				Labels:          map[string]string{"app.kubernetes.io/name": f.name},
				Annotations:     map[string]string{replicaSetRevisionAnnotation: "1"},
				OwnerReferences: []metav1.OwnerReference{owner},
			},
			Spec: appsv1.ReplicaSetSpec{
				Replicas: &one,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": f.name, "rev": "1"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": f.name, "rev": "1"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}}},
				},
			},
		}
		Expect(k8sClient.Create(f.ctx, old)).To(Succeed())
		old.Status.Replicas = readyOld
		old.Status.ReadyReplicas = readyOld
		Expect(k8sClient.Status().Update(f.ctx, old)).To(Succeed())
		return old
	}

	It("does not fetch or import while the old revision still has ready pods, then imports once it is gone", func() {
		f := newDepEnvFixture(depEnvOptions{})
		old := rolloutOf(f, 1)

		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: requeueWaitingForRollout}))
		Expect(requeueWaitingForRollout).To(Equal(30 * time.Second))

		Expect(f.doc.fetches()).To(BeZero(), "the Service may still answer with the old version's document")
		Expect(f.arm.total()).To(BeZero())
		status := f.get().Status
		Expect(status.Phase).To(Equal(apimDeploymentPhaseWaitingForRollout))
		Expect(status.Status).To(Equal(apimDeploymentStatusPending))
		Expect(status.Message).To(ContainSubstring(old.Name))
		Expect(status.ConsecutiveFailures).To(BeZero())

		By("the old revision draining")
		old.Status.ReadyReplicas = 0
		Expect(k8sClient.Status().Update(f.ctx, old)).To(Succeed())
		Expect(f.reconcile()).To(BeZero())
		Expect(f.doc.fetches()).To(Equal(1))
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
		Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseSucceeded))
	})

	It("imports at once when the older revision has no ready pods", func() {
		f := newDepEnvFixture(depEnvOptions{})
		rolloutOf(f, 0)

		Expect(f.reconcile()).To(BeZero())
		Expect(f.arm.count(depEnvImport)).To(Equal(1))
	})

	It("looks again every requeueWaitingForWorkload while no pod is ready", func() {
		f := newDepEnvFixture(depEnvOptions{})
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(f.ctx, types.NamespacedName{Name: f.name + "-pod", Namespace: f.ns}, pod)).To(Succeed())
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		Expect(k8sClient.Status().Update(f.ctx, pod)).To(Succeed())

		Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: requeueWaitingForWorkload}))
		Expect(requeueWaitingForWorkload).To(Equal(2 * time.Minute))
		Expect(f.get().Status.Phase).To(Equal(apimDeploymentPhaseWaitingForReadyPod))
		Expect(f.arm.total()).To(BeZero())
	})
})

var _ = Describe("ensureAPIMAPIDeployment and the APIMAPI's workload selector", func() {
	newAPI := func(ns string, selector *metav1.LabelSelector) *apimv1.APIMAPI {
		GinkgoHelper()
		// The deployment takes its subscription and resource group from the APIMService,
		// so it has to exist whatever order the specs run in.
		depEnvEnsureService(context.Background())
		api := &apimv1.APIMAPI{
			ObjectMeta: metav1.ObjectMeta{Name: "target-api", Namespace: ns},
			Spec: apimv1.APIMAPISpec{
				APIID: "target-api-id", APIMService: depEnvService, RoutePrefix: "/target",
				ServiceURL: depEnvBackend, OpenAPIDefinitionURL: "https://example.com/openapi.json",
			},
		}
		if selector != nil {
			api.Spec.Target = &apimv1.APIMAPITarget{Selector: selector}
		}
		Expect(k8sClient.Create(context.Background(), api)).To(Succeed())
		return api
	}
	newNamespace := func() string {
		GinkgoHelper()
		ns := fmt.Sprintf("target-%d", depEnvCounter.Add(1))
		Expect(k8sClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	It("records the selector, and signals the deployment when it changes", func() {
		ctx := context.Background()
		api := newAPI(newNamespace(), &metav1.LabelSelector{MatchLabels: map[string]string{"app": "v1"}})

		deployment, err := ensureAPIMAPIDeployment(ctx, k8sClient, api)
		Expect(err).NotTo(HaveOccurred())
		want := apimAPITargetSignature(api)
		Expect(want).To(HaveLen(64), "a sha256 of the selector")
		Expect(deployment.Annotations).To(HaveKeyWithValue(apimDeploymentTargetAnnotation, want))
		Expect(deployment.Annotations).NotTo(HaveKey(apimDeploymentSignalAnnotation), "a new deployment needs no signal")

		By("leaving the deployment alone while the selector stays the same")
		before := deployment.ResourceVersion
		deployment, err = ensureAPIMAPIDeployment(ctx, k8sClient, api)
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.ResourceVersion).To(Equal(before))

		By("changing the selector")
		api.Spec.Target.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "v2"}}
		generation := deployment.Generation
		deployment, err = ensureAPIMAPIDeployment(ctx, k8sClient, api)
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Annotations).To(HaveKeyWithValue(apimDeploymentTargetAnnotation, apimAPITargetSignature(api)))
		Expect(deployment.Annotations[apimDeploymentTargetAnnotation]).NotTo(Equal(want))
		signal := deployment.Annotations[apimDeploymentSignalAnnotation]
		Expect(signal).NotTo(BeEmpty(), "the selector is not in the spec, so the signal makes the deployment look again")
		Expect(apimAPIDeploymentPredicate().Update(event.UpdateEvent{
			ObjectOld: &apimv1.APIMAPIDeployment{ObjectMeta: metav1.ObjectMeta{Generation: generation}},
			ObjectNew: deployment,
		})).To(BeTrue(), "the predicate lets the signal through")

		stored := &apimv1.APIMAPIDeployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), stored)).To(Succeed())
		Expect(stored.Annotations).To(HaveKeyWithValue(apimDeploymentSignalAnnotation, signal))
		Expect(stored.Generation).To(Equal(generation), "no spec change")
	})

	It("records an empty target for a legacy APIMAPI and leaves an older deployment without one untouched", func() {
		ctx := context.Background()
		ns := newNamespace()
		api := newAPI(ns, nil)
		Expect(apimAPITargetSignature(api)).To(BeEmpty())

		deployment, err := ensureAPIMAPIDeployment(ctx, k8sClient, api)
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Annotations).To(HaveKeyWithValue(apimDeploymentTargetAnnotation, ""))

		By("an older deployment that never had the annotation")
		delete(deployment.Annotations, apimDeploymentTargetAnnotation)
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		before := deployment.ResourceVersion
		deployment, err = ensureAPIMAPIDeployment(ctx, k8sClient, api)
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.ResourceVersion).To(Equal(before), "no signal for a selector that did not change")
		Expect(deployment.Annotations).NotTo(HaveKey(apimDeploymentSignalAnnotation))
	})
})
