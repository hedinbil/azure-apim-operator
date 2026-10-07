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
	"reflect"
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
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs cover two things every controller that writes to APIM shares and that the
// per-kind specs cannot see, because they call Reconcile one object at a time with an
// uncached client:
//
//   - The retry gate decides on the API server's copy of the resource, not on an
//     informer cache that has not yet seen the failure the previous reconcile patched.
//   - Each of the four controllers runs exactly maxConcurrentAPIMWrites reconciles at
//     once in a real manager, so one slow import no longer holds up every other
//     resource of its kind, and no more than four writes of a kind hit APIM together.
//
// Every identifier is prefixed wcc so nothing collides with the other specs.

// wccStaleClient answers Gets of one object with a snapshot taken earlier, like an
// informer cache that has not caught up with the status patch the previous reconcile
// wrote. Every other read and every write goes to the embedded client.
type wccStaleClient struct {
	client.Client
	snapshot client.Object
}

func (c wccStaleClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == client.ObjectKeyFromObject(c.snapshot) && reflect.TypeOf(obj) == reflect.TypeOf(c.snapshot) {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(c.snapshot.DeepCopyObject()).Elem())
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// wccSeq keeps namespaces and names unique per spec.
var wccSeq atomic.Int32

var _ = Describe("APIM writers: cache staleness and concurrency", func() {
	Context("when the informer cache has not seen the last failure yet", func() {
		It("APIMAPIDeployment: does not import again before nextAttemptAt", func() {
			f := newDepEnvFixture(depEnvOptions{})
			f.arm.on(depEnvImport, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: time.Minute}))

			By("taking the cache's view: one failure, its nextAttemptAt now due")
			f.toNextAttempt()
			stale := f.get()
			Expect(stale.Status.ConsecutiveFailures).To(Equal(int32(1)))

			By("failing the second import, which the cache does not see")
			Expect(f.reconcile()).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
			Expect(f.arm.count(depEnvImport)).To(Equal(2))
			f.arm.on(depEnvImport, nil)

			By("reconciling at once, as the workqueue does for a key marked dirty during the import")
			f.reconciler.Client = wccStaleClient{Client: k8sClient, snapshot: stale}
			f.reconciler.apiReader = k8sClient
			Expect(f.reconcileWithoutAPIM()).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
			Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(2)))

			By("the same stale cache on its own, without the API reader, would import again")
			f.reconciler.apiReader = nil
			f.reconcile()
			Expect(f.arm.count(depEnvImport)).To(Equal(3), "the stale snapshot must really let a write through, or this spec proves nothing")
		})

		for _, k := range tpeKinds() {
			It(k.label()+": does not write again before nextAttemptAt", func() {
				h := newTpeHarness(k, nil)
				build := func(c client.Client, reader client.Reader) reconcile.Reconciler {
					if k.kind == "APIMTag" {
						return &APIMTagReconciler{Client: c, Scheme: c.Scheme(), getToken: h.getToken, retry: h.policy, apiReader: reader}
					}
					return &APIMInboundPolicyReconciler{Client: c, Scheme: c.Scheme(), getToken: h.getToken, retry: h.policy, apiReader: reader}
				}
				run := func(r reconcile.Reconciler) ctrl.Result {
					GinkgoHelper()
					result, err := r.Reconcile(h.ctx, reconcile.Request{NamespacedName: h.key})
					Expect(err).NotTo(HaveOccurred())
					return result
				}

				h.arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
				h.clock.advance(h.reconcile().RequeueAfter)
				stale := h.object()
				Expect(h.reconcile()).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
				Expect(h.puts()).To(Equal(2))
				h.arm.reply(tpeOK)

				Expect(run(build(wccStaleClient{Client: k8sClient, snapshot: stale}, k8sClient))).
					To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
				Expect(h.puts()).To(Equal(2), "the API server says the next attempt is two minutes away")

				run(build(wccStaleClient{Client: k8sClient, snapshot: stale}, nil))
				Expect(h.puts()).To(Equal(3), "the stale snapshot alone lets the write through")
			})
		}

		It("APIMProduct: does not write again before nextAttemptAt", func() {
			n := wccSeq.Add(1)
			ctx := context.Background()
			service := fmt.Sprintf("wcc-product-svc-%d", n)
			key := types.NamespacedName{Name: fmt.Sprintf("wcc-product-%d", n), Namespace: "default"}
			DeferCleanup(stubAzureIdentityEnv())
			Expect(k8sClient.Create(ctx, &apimv1.APIMService{
				ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: getOperatorNamespace()},
				Spec:       apimv1.APIMServiceSpec{Name: service, Subscription: depEnvSubscription, ResourceGroup: depEnvResourceGroup},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &apimv1.APIMProduct{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec:       apimv1.APIMProductSpec{ProductID: key.Name, DisplayName: "WCC", APIMService: service},
			})).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &apimv1.APIMProduct{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})
				_ = k8sClient.Delete(ctx, &apimv1.APIMService{ObjectMeta: metav1.ObjectMeta{Name: service, Namespace: getOperatorNamespace()}})
			})

			clock := &fakeClock{t: tpeStart}
			var upserts atomic.Int32
			var failing atomic.Bool
			failing.Store(true)
			build := func(c client.Client, reader client.Reader) *APIMProductReconciler {
				return &APIMProductReconciler{
					Client: c, Scheme: c.Scheme(), apiReader: reader,
					getToken: func(context.Context, string, string) (string, error) { return "tok", nil },
					upsertProduct: func(context.Context, apim.APIMProductConfig) error {
						upserts.Add(1)
						if failing.Load() {
							return &apim.Error{Operation: "upsert product", Method: http.MethodPut,
								StatusCode: http.StatusServiceUnavailable, Code: "ServiceUnavailable"}
						}
						return nil
					},
					retry: &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now},
				}
			}
			run := func(r *APIMProductReconciler) ctrl.Result {
				GinkgoHelper()
				result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				return result
			}
			get := func() *apimv1.APIMProduct {
				GinkgoHelper()
				product := &apimv1.APIMProduct{}
				Expect(k8sClient.Get(ctx, key, product)).To(Succeed())
				return product
			}

			clock.advance(run(build(k8sClient, nil)).RequeueAfter)
			stale := get()
			Expect(run(build(k8sClient, nil))).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
			Expect(upserts.Load()).To(Equal(int32(2)))
			failing.Store(false)

			Expect(run(build(wccStaleClient{Client: k8sClient, snapshot: stale}, k8sClient))).
				To(Equal(ctrl.Result{RequeueAfter: 2 * time.Minute}))
			Expect(upserts.Load()).To(Equal(int32(2)))
			Expect(get().Status.ConsecutiveFailures).To(Equal(int32(2)))

			run(build(wccStaleClient{Client: k8sClient, snapshot: stale}, nil))
			Expect(upserts.Load()).To(Equal(int32(3)), "the stale snapshot alone lets the write through")
		})
	})

	Context("with the controllers running in a manager", func() {
		// wccWriterKind adapts one APIM-writing controller to the concurrency spec.
		type wccWriterKind struct {
			name string
			// setup registers the kind's reconciler with mgr and returns the API reader
			// SetupWithManager wired into it.
			setup func(mgr ctrl.Manager) (client.Reader, error)
			// prepare creates whatever object i needs besides itself (a deployment's
			// APIMAPI, ReplicaSet and ready pod).
			prepare func(ctx context.Context, ns string, i int)
			// create creates object i.
			create func(ctx context.Context, ns string, i int)
			// isWrite picks out the request that is held until the spec lets go.
			isWrite func(r *http.Request) bool
		}
		const objects = 6
		token := func(context.Context, string, string) (string, error) { return "wcc-token", nil }
		// A clock-free policy that never gets in the way: every write here succeeds.
		policy := &retryPolicy{BaseDelay: time.Minute, MaxDelay: time.Minute, MaxAttempts: 5}
		var doc *depEnvDocServer

		kinds := []wccWriterKind{
			{
				name: "APIMAPIDeployment",
				setup: func(mgr ctrl.Manager) (client.Reader, error) {
					r := &APIMAPIDeploymentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
						fetcher: testOpenAPIFetcher(), getToken: token, retry: policy}
					err := r.SetupWithManager(mgr)
					return r.apiReader, err
				},
				prepare: func(ctx context.Context, ns string, i int) {
					name := fmt.Sprintf("wcc-dep-%d", i)
					Expect(k8sClient.Create(ctx, &apimv1.APIMAPI{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
						Spec: apimv1.APIMAPISpec{APIID: name, APIMService: depEnvService, RoutePrefix: "/" + name,
							ServiceURL: depEnvBackend, OpenAPIDefinitionURL: doc.server.URL},
					})).To(Succeed())
					(&depEnvFixture{ctx: ctx, ns: ns, name: name, rsName: name + "-rs"}).createReplicaSetWithReadyPod()
				},
				create: func(ctx context.Context, ns string, i int) {
					name := fmt.Sprintf("wcc-dep-%d", i)
					Expect(k8sClient.Create(ctx, &apimv1.APIMAPIDeployment{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
						Spec: apimv1.APIMAPIDeploymentSpec{APIID: name, APIMService: depEnvService, APIMAPIName: name,
							Subscription: depEnvSubscription, ResourceGroup: depEnvResourceGroup, RoutePrefix: "/" + name,
							ServiceURL: depEnvBackend, OpenAPIDefinitionURL: doc.server.URL, SubscriptionRequired: true},
					})).To(Succeed())
				},
				isWrite: func(r *http.Request) bool {
					return r.Method == http.MethodPut && r.URL.Query().Get("import") == "true"
				},
			},
			{
				name: "APIMProduct",
				setup: func(mgr ctrl.Manager) (client.Reader, error) {
					r := &APIMProductReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), getToken: token, retry: policy}
					err := r.SetupWithManager(mgr)
					return r.apiReader, err
				},
				create: func(ctx context.Context, ns string, i int) {
					Expect(k8sClient.Create(ctx, &apimv1.APIMProduct{
						ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wcc-product-%d", i), Namespace: ns},
						Spec: apimv1.APIMProductSpec{ProductID: fmt.Sprintf("wcc-product-%d", i), DisplayName: "WCC",
							APIMService: depEnvService},
					})).To(Succeed())
				},
				isWrite: func(r *http.Request) bool {
					return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/products/")
				},
			},
			{
				name: "APIMTag",
				setup: func(mgr ctrl.Manager) (client.Reader, error) {
					r := &APIMTagReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), getToken: token, retry: policy}
					err := r.SetupWithManager(mgr)
					return r.apiReader, err
				},
				create: func(ctx context.Context, ns string, i int) {
					Expect(k8sClient.Create(ctx, &apimv1.APIMTag{
						ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wcc-tag-%d", i), Namespace: ns},
						Spec: apimv1.APIMTagSpec{TagID: fmt.Sprintf("wcc-tag-%d", i), DisplayName: "WCC",
							APIMService: depEnvService},
					})).To(Succeed())
				},
				isWrite: func(r *http.Request) bool {
					return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/tags/")
				},
			},
			{
				name: "APIMInboundPolicy",
				setup: func(mgr ctrl.Manager) (client.Reader, error) {
					r := &APIMInboundPolicyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), getToken: token, retry: policy}
					err := r.SetupWithManager(mgr)
					return r.apiReader, err
				},
				create: func(ctx context.Context, ns string, i int) {
					Expect(k8sClient.Create(ctx, &apimv1.APIMInboundPolicy{
						ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("wcc-policy-%d", i), Namespace: ns},
						Spec: apimv1.APIMInboundPolicySpec{APIID: fmt.Sprintf("wcc-api-%d", i), APIMService: depEnvService,
							PolicyContent: tpePolicyXML},
					})).To(Succeed())
				},
				isWrite: func(r *http.Request) bool {
					return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/policies/policy")
				},
			},
		}

		for _, k := range kinds {
			It(fmt.Sprintf("%s: writes %d resources at once, never more, and wires the API reader", k.name, maxConcurrentAPIMWrites), func() {
				ctx := context.Background()
				ns := fmt.Sprintf("wcc-%d", wccSeq.Add(1))
				Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
				depEnvEnsureService(ctx)
				DeferCleanup(stubAzureIdentityEnv())

				By("serving a fake ARM that holds every write until the spec lets go")
				var (
					inFlight, peak, done atomic.Int32
					release              = make(chan struct{})
					releaseOnce          sync.Once
				)
				arm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					switch {
					case k.isWrite(r):
						now := inFlight.Add(1)
						for {
							was := peak.Load()
							if now <= was || peak.CompareAndSwap(was, now) {
								break
							}
						}
						select {
						case <-release:
						case <-r.Context().Done():
						}
						inFlight.Add(-1)
						done.Add(1)
						depEnvWriteJSON(w, http.StatusOK, `{}`)
					case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/apis/"):
						depEnvWriteError(w, http.StatusNotFound, "ResourceNotFound", "API not found")
					case r.Method == http.MethodGet:
						depEnvWriteJSON(w, http.StatusOK, `{"properties":{"hostnameConfigurations":[`+
							`{"type":"Proxy","hostName":"gw.wcc.net"},{"type":"DeveloperPortal","hostName":"portal.wcc.net"}]}}`)
					default:
						depEnvWriteJSON(w, http.StatusOK, `{}`)
					}
				}))
				doc = newDepEnvDocServer(depEnvDocV1)
				// Cleanups run last-registered first: let go of the held writes, stop the
				// manager, then close the servers.
				DeferCleanup(arm.Close)
				DeferCleanup(doc.server.Close)
				DeferCleanup(apim.UseEndpoint(arm.URL, arm.Client()))

				By("starting a manager with only this controller, reading through its cache")
				skip := true
				mgr, err := ctrl.NewManager(cfg, ctrl.Options{
					Scheme:                 scheme.Scheme,
					Logger:                 tpeQuietLogger(),
					Metrics:                metricsserver.Options{BindAddress: "0"},
					HealthProbeBindAddress: "0",
					Controller:             ctrlconfig.Controller{SkipNameValidation: &skip},
					Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{
						ns: {}, getOperatorNamespace(): {},
					}},
				})
				Expect(err).NotTo(HaveOccurred())
				reader, err := k.setup(mgr)
				Expect(err).NotTo(HaveOccurred())
				Expect(reader).To(BeIdenticalTo(mgr.GetAPIReader()),
					"SetupWithManager must wire the uncached reader the retry gate reads through")

				mgrCtx, stop := context.WithCancel(context.Background())
				stopped := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					defer close(stopped)
					Expect(mgr.Start(mgrCtx)).To(Succeed())
				}()
				DeferCleanup(func() {
					stop()
					Eventually(stopped, 30*time.Second).Should(BeClosed())
				})
				letGo := func() { releaseOnce.Do(func() { close(release) }) }
				DeferCleanup(letGo)

				for i := range objects {
					if k.prepare != nil {
						k.prepare(ctx, ns, i)
					}
				}
				for i := range objects {
					k.create(ctx, ns, i)
				}

				By(fmt.Sprintf("seeing exactly %d writes held at once", maxConcurrentAPIMWrites))
				Eventually(inFlight.Load, 20*time.Second, 10*time.Millisecond).Should(BeEquivalentTo(maxConcurrentAPIMWrites),
					"one held write must not block the others of its kind")
				Consistently(inFlight.Load, 500*time.Millisecond, 10*time.Millisecond).Should(BeEquivalentTo(maxConcurrentAPIMWrites),
					"the remaining resources wait for a free worker")
				Expect(peak.Load()).To(BeEquivalentTo(maxConcurrentAPIMWrites))
				Expect(done.Load()).To(BeZero())

				By("letting go: every resource gets its write, still never more than four at once")
				letGo()
				Eventually(done.Load, 20*time.Second, 10*time.Millisecond).Should(BeNumerically(">=", objects))
				Expect(peak.Load()).To(BeEquivalentTo(maxConcurrentAPIMWrites))
			})
		}
	})
})
