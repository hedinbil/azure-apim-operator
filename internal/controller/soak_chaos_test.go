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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These specs soak every controller that writes to APIM at once: 30 APIMAPIDeployments,
// 10 APIMProducts, 10 APIMTags and 10 APIMInboundPolicies, reconciled by four workers per
// kind against a fake ARM that fails a seeded share of requests with the errors seen in
// the Sep 2026 incident (412, 422 "Management API timed out", 202 imports that never
// finish or die with DeadOperationMonitor) and with outright rejections (400, 401, 403,
// 404). After every reconcile the oracle in soak_chaos_helpers_test.go checks the result
// against the agreed design; at the end every object must have settled in exactly one of
// Succeeded/Created, Stalled or Invalid.
//
// Invariants checked on every run:
//   - never two ARM requests in flight for one object, and none outside its reconcile;
//   - no reconcile writes to APIM while backing off before nextAttemptAt, or while
//     Stalled or Invalid without a spec change or a new retry annotation value;
//   - at most five failed attempts in a row, at most five attempts per spec version for a
//     deployment, exactly one import PUT per deployment attempt, nothing after a failing
//     step;
//   - each failure's class, phase, consecutiveFailures, nextAttemptAt, backoff length and
//     RequeueAfter match the design;
//   - no Reconcile error, no panic, no data race (run with -race), at most four reconciles
//     of one kind at a time;
//   - the "🛑 APIM write stalled" line Datadog watches is logged once per stall, with
//     kind, namespace, name, attempts and lastError.
//
// Run just these with:
//
//	go test -race ./internal/controller/ -ginkgo.focus "APIM write soak"

const (
	soakSubscription  = "00000000-0000-0000-0000-00000000c0a5"
	soakResourceGroup = "rg-soak"
	soakToken         = "soak-token"
	soakNamespace     = "default"
	soakOpenAPIDoc    = `{"openapi":"3.0.0","info":{"title":"soak","version":"1.0.0"},"paths":{}}`
)

// soakRunCounter keeps the resource names of every run unique.
var soakRunCounter atomic.Int32

// soakRun is one finished soak.
type soakRun struct {
	plan    *soakPlan
	objects []*soakObject
	driver  *soakDriver
	arm     *soakARM
	lines   []logLine
}

// byKind returns the objects of kind.
func (r *soakRun) byKind(kind soakKind) []*soakObject {
	var out []*soakObject
	for _, o := range r.objects {
		if o.kind == kind {
			out = append(out, o)
		}
	}
	return out
}

// traces maps each object's logical name to what happened to it, for comparing runs.
func (r *soakRun) traces() map[string]string {
	out := map[string]string{}
	for _, o := range r.objects {
		out[o.logical] = strings.Join(o.trace, " ")
	}
	return out
}

// phases counts the final model phases.
func (r *soakRun) phases() map[string]int {
	out := map[string]int{}
	for _, o := range r.objects {
		out[o.phase]++
	}
	return out
}

// soakReconcilers are the reconcilers an object is reconciled by.
type soakReconcilers struct {
	deployment *APIMAPIDeploymentReconciler
	product    *APIMProductReconciler
	tag        *APIMTagReconciler
	policy     *APIMInboundPolicyReconciler
}

// soakDefaultPlan is the full-size soak with the given chaos settings.
func soakDefaultPlan(name string, seed uint64) soakPlan {
	return soakPlan{name: name, seed: seed, deployments: 30, products: 10, tags: 10, policies: 10}
}

// buildSoakObjects lays out the objects of a run. Their APIM ids are the same in every
// run; only the Kubernetes and APIM service names carry the run number.
func buildSoakObjects(plan *soakPlan, run int32) []*soakObject {
	var objects []*soakObject
	add := func(kind soakKind, prefix string, i int) *soakObject {
		logical := fmt.Sprintf("%s-%02d", prefix, i)
		name := fmt.Sprintf("soak%d-%s", run, logical)
		o := &soakObject{
			kind:                kind,
			logical:             logical,
			key:                 types.NamespacedName{Name: name, Namespace: soakNamespace},
			svc:                 name,
			maxWritesPerAttempt: 1,
			noiseLeft:           3,
		}
		if plan.events {
			if soakChance(plan.seed, logical, "plan-spec") < 0.3 {
				o.specChangeAfter = 1 + int(soakHash(plan.seed, logical, "spec-at")%3)
			}
			if soakChance(plan.seed, logical, "plan-midflight") < 0.2 {
				o.midFlightAt = 1 + int(soakHash(plan.seed, logical, "midflight-at")%3)
			}
			o.annotationsLeft = 2
			o.backoffAnnotation = soakChance(plan.seed, logical, "plan-backoff-annotation") < 0.15
		}
		objects = append(objects, o)
		return o
	}
	for i := 0; i < plan.deployments; i++ {
		o := add(soakDeployment, "dep", i)
		o.apiID = fmt.Sprintf("api-%02d", i)
		switch i % 3 {
		case 0:
			o.productIDs = []string{"p-a", "p-b"}
		case 1:
			o.productIDs = []string{"p-a"}
		}
		if i%2 == 0 {
			o.tagIDs = []string{"t-x"}
		}
		o.maxWritesPerAttempt = 3 + len(o.productIDs) + len(o.tagIDs)
	}
	for i := 0; i < plan.products; i++ {
		o := add(soakProduct, "prd", i)
		o.productID = fmt.Sprintf("prod-%02d", i)
		o.deleteWhenSettled = plan.deleteProducts
	}
	for i := 0; i < plan.tags; i++ {
		o := add(soakTag, "tag", i)
		o.tagID = fmt.Sprintf("tag-%02d", i)
	}
	for i := 0; i < plan.policies; i++ {
		o := add(soakPolicy, "pol", i)
		o.apiID = fmt.Sprintf("pol-api-%02d", i)
		if i%2 == 1 {
			o.operationID = fmt.Sprintf("op-%02d", i)
		}
	}
	return objects
}

// soakPolicyContent is the inbound policy XML of a spec version.
func soakPolicyContent(version int) string {
	return fmt.Sprintf(`<policies><inbound><base /><set-header name="x-soak" exists-action="override"><value>v%d</value></set-header></inbound></policies>`, version)
}

// createSoakObjects creates everything the objects need in the cluster.
func createSoakObjects(ctx context.Context, plan *soakPlan, objects []*soakObject, docURL string) {
	GinkgoHelper()
	for _, o := range objects {
		service := &apimv1.APIMService{
			ObjectMeta: metav1.ObjectMeta{Name: o.svc, Namespace: soakNamespace},
			Spec:       apimv1.APIMServiceSpec{Name: o.svc, Subscription: soakSubscription, ResourceGroup: soakResourceGroup},
		}
		Expect(k8sClient.Create(ctx, service)).To(Succeed())
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), service)) })

		var obj client.Object
		switch o.kind {
		case soakDeployment:
			// Every fifth deployment imports a new revision, which skips the GET of the API.
			revision := ""
			if strings.HasSuffix(o.logical, "4") || strings.HasSuffix(o.logical, "9") {
				revision = "2"
			}
			api := &apimv1.APIMAPI{
				ObjectMeta: metav1.ObjectMeta{Name: o.key.Name, Namespace: soakNamespace},
				Spec: apimv1.APIMAPISpec{
					APIID:                o.apiID,
					APIMService:          o.svc,
					RoutePrefix:          "/soak/" + o.apiID,
					ServiceURL:           "https://backend.example.net",
					OpenAPIDefinitionURL: docURL,
				},
			}
			Expect(k8sClient.Create(ctx, api)).To(Succeed())
			DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), api)) })
			rs := createReplicaSet(ctx, o.key.Name+"-rs", map[string]string{"app.kubernetes.io/name": o.key.Name}, map[string]string{"app": o.key.Name})
			createReadyPodForReplicaSet(ctx, rs, o.key.Name+"-pod")
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: o.key.Name + "-pod", Namespace: soakNamespace}}
			DeferCleanup(func() {
				_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), pod))
				_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), rs))
			})
			obj = &apimv1.APIMAPIDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: o.key.Name, Namespace: soakNamespace},
				Spec: apimv1.APIMAPIDeploymentSpec{
					APIID:                o.apiID,
					APIMService:          o.svc,
					APIMAPIName:          o.key.Name,
					Subscription:         soakSubscription,
					ResourceGroup:        soakResourceGroup,
					RoutePrefix:          "/soak/" + o.apiID,
					ServiceURL:           "https://backend.example.net",
					OpenAPIDefinitionURL: docURL,
					Revision:             revision,
					SubscriptionRequired: true,
					ProductIDs:           o.productIDs,
					TagIDs:               o.tagIDs,
				},
			}
		case soakProduct:
			policy := apimv1.DeletionPolicyRetain
			if plan.deleteProducts {
				policy = apimv1.DeletionPolicyDelete
			}
			obj = &apimv1.APIMProduct{
				ObjectMeta: metav1.ObjectMeta{Name: o.key.Name, Namespace: soakNamespace},
				Spec: apimv1.APIMProductSpec{
					ProductID:      o.productID,
					DisplayName:    "Soak product v0",
					APIMService:    o.svc,
					Published:      true,
					DeletionPolicy: policy,
				},
			}
		case soakTag:
			obj = &apimv1.APIMTag{
				ObjectMeta: metav1.ObjectMeta{Name: o.key.Name, Namespace: soakNamespace},
				Spec:       apimv1.APIMTagSpec{APIMService: o.svc, TagID: o.tagID, DisplayName: "Soak tag v0"},
			}
		case soakPolicy:
			obj = &apimv1.APIMInboundPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: o.key.Name, Namespace: soakNamespace},
				Spec: apimv1.APIMInboundPolicySpec{
					APIMService:   o.svc,
					APIID:         o.apiID,
					OperationID:   o.operationID,
					PolicyContent: soakPolicyContent(0),
				},
			}
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		key := o.key
		kind := o.kind
		DeferCleanup(func() {
			if kind == soakProduct {
				forceDeleteProduct(context.Background(), key)
				return
			}
			_ = client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj))
		})
	}
}

// soakObserve reads the status a reconcile left behind.
func soakObserve(ctx context.Context, o *soakObject) (soakObserved, error) {
	var (
		obs soakObserved
		obj client.Object
	)
	switch o.kind {
	case soakDeployment:
		obj = &apimv1.APIMAPIDeployment{}
	case soakProduct:
		obj = &apimv1.APIMProduct{}
	case soakTag:
		obj = &apimv1.APIMTag{}
	case soakPolicy:
		obj = &apimv1.APIMInboundPolicy{}
	}
	if err := k8sClient.Get(ctx, o.key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return obs, nil
		}
		return obs, err
	}
	obs.found = true
	obs.deleting = !obj.GetDeletionTimestamp().IsZero()
	switch v := obj.(type) {
	case *apimv1.APIMAPIDeployment:
		obs.phase, obs.retry, obs.lastError, obs.message = v.Status.Phase, v.Status.RetryStatus, v.Status.LastError, v.Status.Message
	case *apimv1.APIMProduct:
		obs.phase, obs.retry, obs.message = v.Status.Phase, v.Status.RetryStatus, v.Status.Message
	case *apimv1.APIMTag:
		obs.phase, obs.retry, obs.message = v.Status.Phase, v.Status.RetryStatus, v.Status.Message
	case *apimv1.APIMInboundPolicy:
		obs.phase, obs.retry, obs.message = v.Status.Phase, v.Status.RetryStatus, v.Status.Message
	}
	return obs, nil
}

// soakMutate makes an event happen in the cluster, as a person or a pipeline would. It
// uses merge patches, which never conflict with the controllers' status patches.
func soakMutate(ctx context.Context, o *soakObject, ev soakEvent, version int) error {
	var obj client.Object
	switch o.kind {
	case soakDeployment:
		obj = &apimv1.APIMAPIDeployment{}
	case soakProduct:
		obj = &apimv1.APIMProduct{}
	case soakTag:
		obj = &apimv1.APIMTag{}
	case soakPolicy:
		obj = &apimv1.APIMInboundPolicy{}
	}
	if err := k8sClient.Get(ctx, o.key, obj); err != nil {
		return err
	}
	if ev == soakEventDelete {
		return k8sClient.Delete(ctx, obj)
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	switch ev {
	case soakEventRetryAnnotation:
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[retryAnnotation] = fmt.Sprintf("soak-%d", version)
		obj.SetAnnotations(annotations)
	case soakEventSpecChange, soakEventMidFlightSpecChange:
		switch v := obj.(type) {
		case *apimv1.APIMAPIDeployment:
			v.Spec.ServiceURL = fmt.Sprintf("https://backend-v%d.example.net", version)
		case *apimv1.APIMProduct:
			v.Spec.DisplayName = fmt.Sprintf("Soak product v%d", version)
		case *apimv1.APIMTag:
			v.Spec.DisplayName = fmt.Sprintf("Soak tag v%d", version)
		case *apimv1.APIMInboundPolicy:
			v.Spec.PolicyContent = soakPolicyContent(version)
		}
	default:
		return fmt.Errorf("unexpected event %s", ev)
	}
	return k8sClient.Patch(ctx, obj, patch)
}

// runSoak runs plan to the end and checks every invariant that holds for all plans.
func runSoak(ctx context.Context, plan soakPlan) *soakRun {
	GinkgoHelper()
	run := soakRunCounter.Add(1)

	docServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, soakOpenAPIDoc)
	}))
	DeferCleanup(docServer.Close)

	arm := newSoakARM(&plan, soakToken)
	DeferCleanup(arm.server.Close)
	DeferCleanup(apim.UseEndpoint(arm.server.URL, arm.server.Client()))

	objects := buildSoakObjects(&plan, run)
	for _, o := range objects {
		arm.register(o)
	}
	createSoakObjects(ctx, &plan, objects, docServer.URL)

	driver := newSoakDriver(&plan, arm, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC), maxConcurrentAPIMWrites)
	driver.objects = objects

	token := func(context.Context, string, string) (string, error) { return soakToken, nil }
	build := func(random func() float64) *soakReconcilers {
		policy := &retryPolicy{
			BaseDelay:   time.Minute,
			MaxDelay:    30 * time.Minute,
			MaxAttempts: soakMaxAttempts,
			Jitter:      plan.jitter,
			Now:         driver.now,
			Random:      random,
		}
		return &soakReconcilers{
			deployment: &APIMAPIDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), fetcher: testOpenAPIFetcher(), getToken: token, retry: policy},
			product:    &APIMProductReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: policy},
			tag:        &APIMTagReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: policy},
			policy:     &APIMInboundPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: token, retry: policy},
		}
	}
	reconcilers := map[*soakObject]*soakReconcilers{}
	shared := build(soakJitterSource(plan.seed, "shared"))
	for _, o := range objects {
		reconcilers[o] = shared
		if !plan.sharedReconcilers {
			reconcilers[o] = build(soakJitterSource(plan.seed, o.logical))
		}
	}

	logger, lines := captureLogs()
	driver.reconcile = func(ctx context.Context, o *soakObject) (ctrl.Result, error) {
		req := reconcile.Request{NamespacedName: o.key}
		ctx = logf.IntoContext(ctx, logger.WithValues("controller", string(o.kind)))
		rs := reconcilers[o]
		switch o.kind {
		case soakDeployment:
			return rs.deployment.Reconcile(ctx, req)
		case soakProduct:
			return rs.product.Reconcile(ctx, req)
		case soakTag:
			return rs.tag.Reconcile(ctx, req)
		default:
			return rs.policy.Reconcile(ctx, req)
		}
	}
	driver.observe = soakObserve
	driver.mutate = soakMutate

	started := time.Now()
	driver.run(ctx)
	elapsed := time.Since(started)

	r := &soakRun{plan: &plan, objects: objects, driver: driver, arm: arm, lines: lines()}

	meanBusy := 0.0
	if driver.reconcileCount > 0 {
		meanBusy = float64(driver.busySum) / float64(driver.reconcileCount)
	}
	By(fmt.Sprintf("soak %q: %d reconciles, %d ARM requests, virtual time %s, wall time %s, peak workers %v, mean concurrent reconciles %.1f, final phases %v",
		plan.name, driver.reconcileCount, arm.requests, driver.now().Sub(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)),
		elapsed.Round(time.Millisecond), driver.peak, meanBusy, r.phases()))

	soakExpectNoViolations("fake ARM", arm.snapshotViolations())
	soakExpectNoViolations("oracle", driver.violations)
	Expect(driver.reconcileErrs).To(BeZero(), "Reconcile errors")
	Expect(driver.panics).To(BeZero(), "panics")
	// The soak driver runs its own maxConcurrentAPIMWrites workers per kind, so these
	// check that the soak really reconciled objects side by side (the reconcilers must be
	// safe for that), not that the controllers are configured for it. The manager-based
	// spec in writer_cache_concurrency_envtest_test.go checks the controllers' wiring.
	for kind, peak := range driver.peak {
		Expect(peak).To(BeNumerically("<=", maxConcurrentAPIMWrites), "soak driver workers busy at once for %s", kind)
	}
	if plan.deployments >= 2*maxConcurrentAPIMWrites {
		Expect(driver.peak[soakDeployment]).To(BeNumerically(">=", 2), "the soak must actually reconcile deployments side by side")
	}

	By("checking that every object settled in exactly one terminal state")
	for _, o := range objects {
		obs, err := soakObserve(ctx, o)
		Expect(err).NotTo(HaveOccurred())
		if o.gone {
			Expect(obs.found).To(BeFalse(), "%s %s was removed from APIM but still exists", o.kind, o.logical)
			continue
		}
		Expect(obs.found).To(BeTrue(), "%s %s disappeared", o.kind, o.logical)
		Expect(obs.phase).To(BeElementOf(soakSuccessPhase(o.kind), phaseStalled, phaseInvalid),
			"%s %s ended in %q (%s)", o.kind, o.logical, obs.phase, obs.message)
		Expect(obs.phase).To(Equal(o.phase), "%s %s", o.kind, o.logical)
		Expect(obs.retry.NextAttemptAt).To(BeEmpty(), "%s %s: a settled object has no next attempt", o.kind, o.logical)
		switch obs.phase {
		case phaseStalled:
			Expect(obs.retry.ConsecutiveFailures).To(Equal(int32(soakMaxAttempts)), "%s %s", o.kind, o.logical)
		case phaseInvalid:
			Expect(obs.retry.ConsecutiveFailures).To(BeNumerically(">=", 1), "%s %s", o.kind, o.logical)
		default:
			Expect(obs.retry.ConsecutiveFailures).To(BeZero(), "%s %s", o.kind, o.logical)
		}
		if plan.deleteProducts && o.kind == soakProduct {
			Expect(obs.deleting).To(BeTrue(), "%s %s should be deleting", o.kind, o.logical)
		}
	}

	By("checking the write log lines of products, tags and policies")
	soakExpectLogLines(r)
	return r
}

// soakExpectNoViolations fails with the first violations, if any.
func soakExpectNoViolations(source string, violations []string) {
	GinkgoHelper()
	if len(violations) == 0 {
		return
	}
	shown := violations
	if len(shown) > 25 {
		shown = shown[:25]
	}
	Fail(fmt.Sprintf("%s: %d invariant violations, first %d:\n  %s", source, len(violations), len(shown), strings.Join(shown, "\n  ")))
}

// soakExpectLogLines checks the log contract: one ▶️ per attempt, one 💚 per success, one
// 💔 rejected per Invalid, and the stable 🛑 line Datadog watches once per stall. The
// deployment controller logs through ctrl.Log rather than the context logger, so only
// products, tags and policies are captured here.
func soakExpectLogLines(r *soakRun) {
	GinkgoHelper()
	type counts struct{ starting, succeeded, rejected, stalled int }
	got := map[string]*counts{}
	for _, line := range r.lines {
		name, _ := line.kv["name"].(string)
		if name == "" {
			continue
		}
		c := got[name]
		if c == nil {
			c = &counts{}
			got[name] = c
		}
		switch line.msg {
		case msgWriteStarting:
			c.starting++
		case msgWriteSucceeded:
			c.succeeded++
		case msgWriteRejected:
			c.rejected++
			Expect(line.err).To(BeTrue(), "the rejected line is logged as an error")
		case msgWriteStalled:
			c.stalled++
			Expect(line.err).To(BeTrue(), "the stalled line is logged as an error")
			Expect(line.kv).To(HaveKeyWithValue("namespace", soakNamespace))
			Expect(line.kv).To(HaveKeyWithValue("attempts", int32(soakMaxAttempts)))
			Expect(line.kv).To(HaveKey("kind"))
			lastError, _ := line.kv["lastError"].(string)
			Expect(lastError).NotTo(BeEmpty(), "the stalled line carries lastError")
		}
	}
	for _, o := range r.objects {
		if o.kind == soakDeployment {
			continue
		}
		c := got[o.key.Name]
		if c == nil {
			c = &counts{}
		}
		Expect(c.starting).To(Equal(o.attempts), "%s %s: one ▶️ per attempt", o.kind, o.logical)
		Expect(c.succeeded).To(Equal(o.successes), "%s %s: one 💚 per success", o.kind, o.logical)
		Expect(c.rejected).To(Equal(o.rejections), "%s %s: one 💔 rejected per Invalid", o.kind, o.logical)
		Expect(c.stalled).To(Equal(o.stalls), "%s %s: one 🛑 per stall", o.kind, o.logical)
	}
	for _, line := range r.lines {
		if line.msg != msgWriteStalled {
			continue
		}
		name, _ := line.kv["name"].(string)
		for _, o := range r.objects {
			if o.key.Name != name {
				continue
			}
			Expect(line.kv).To(HaveKeyWithValue("kind", string(o.kind)))
			switch o.kind {
			case soakProduct:
				Expect(line.kv).To(HaveKeyWithValue("productID", o.productID))
			case soakTag:
				Expect(line.kv).To(HaveKeyWithValue("tagID", o.tagID))
			case soakPolicy:
				Expect(line.kv).To(HaveKeyWithValue("apiID", o.apiID))
			}
		}
	}
}

var _ = Describe("APIM write soak under chaos", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		DeferCleanup(stubAzureIdentityEnv())

		// Async imports wait milliseconds, not minutes.
		previousWait, previousPoll := apim.AsyncWaitTimeout, apim.AsyncPollInterval
		apim.AsyncWaitTimeout = 30 * time.Millisecond
		apim.AsyncPollInterval = 2 * time.Millisecond
		DeferCleanup(func() { apim.AsyncWaitTimeout, apim.AsyncPollInterval = previousWait, previousPoll })
	})

	It("sets four concurrent workers in the options every APIM writer uses", func() {
		Expect(apimWriterOptions().MaxConcurrentReconciles).To(Equal(4))
		Expect(maxConcurrentAPIMWrites).To(Equal(4))
	})

	It("settles a healthy APIM in one attempt per object, with spurious events changing nothing", func() {
		plan := soakDefaultPlan("healthy", 1)
		plan.sharedReconcilers = true
		plan.asyncShare = 0.4
		plan.noiseRate = 0.5
		plan.jitter = 0.2
		plan.batchWindow = 5 * time.Second
		r := runSoak(ctx, plan)

		for _, o := range r.objects {
			Expect(o.phase).To(Equal(soakSuccessPhase(o.kind)), "%s %s", o.kind, o.logical)
			Expect(o.attempts).To(Equal(1), "%s %s: one attempt", o.kind, o.logical)
		}
		for _, o := range r.byKind(soakDeployment) {
			Expect(o.importPuts).To(Equal(1), "%s: one import", o.logical)
		}
	})

	It("stalls every object after exactly five attempts 1, 2, 4 and 8 minutes apart when APIM is down", func() {
		plan := soakDefaultPlan("outage", 2)
		plan.sharedReconcilers = true
		plan.force = func(string) *soakFault { f := soakUnavailable; return &f }
		plan.noiseRate = 0.6
		r := runSoak(ctx, plan)

		for _, o := range r.objects {
			Expect(o.phase).To(Equal(phaseStalled), "%s %s", o.kind, o.logical)
			Expect(o.attempts).To(Equal(soakMaxAttempts), "%s %s", o.kind, o.logical)
			Expect(o.stalls).To(Equal(1), "%s %s", o.kind, o.logical)
			Expect(o.runGaps).To(Equal([][]time.Duration{{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}}),
				"%s %s: attempts must be spaced by the doubling backoff", o.kind, o.logical)
		}
		for _, o := range r.byKind(soakDeployment) {
			Expect(o.importPuts).To(Equal(soakMaxAttempts), "%s: imports", o.logical)
		}
		Expect(r.driver.now().Sub(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))).To(Equal(15*time.Minute),
			"the whole outage costs a quarter of an hour of virtual time and then stops")
	})

	It("marks every object Invalid after one attempt when APIM rejects every write", func() {
		plan := soakDefaultPlan("rejected", 3)
		plan.sharedReconcilers = true
		plan.force = func(string) *soakFault { f := soakValidationError; return &f }
		plan.noiseRate = 0.6
		r := runSoak(ctx, plan)

		for _, o := range r.objects {
			Expect(o.phase).To(Equal(phaseInvalid), "%s %s", o.kind, o.logical)
			Expect(o.attempts).To(Equal(1), "%s %s", o.kind, o.logical)
			Expect(o.rejections).To(Equal(1), "%s %s", o.kind, o.logical)
		}
	})

	It("replays the Sep 2026 incident: imports that never finish cost five imports, not 800", func() {
		plan := soakDefaultPlan("incident replay", 4)
		plan.sharedReconcilers = true
		plan.force = func(step string) *soakFault {
			if step == soakStepImport {
				f := soakAcceptedTimeout
				return &f
			}
			return nil
		}
		plan.jitter = 0.2
		plan.batchWindow = 5 * time.Second
		plan.noiseRate = 0.6
		r := runSoak(ctx, plan)

		for _, o := range r.byKind(soakDeployment) {
			Expect(o.phase).To(Equal(phaseStalled), o.logical)
			Expect(o.importPuts).To(Equal(soakMaxAttempts), "%s: import PUTs", o.logical)
		}
		for _, kind := range []soakKind{soakProduct, soakTag, soakPolicy} {
			for _, o := range r.byKind(kind) {
				Expect(o.phase).To(Equal(phaseCreated), "%s %s is not affected by slow imports", o.kind, o.logical)
				Expect(o.attempts).To(Equal(1), "%s %s", o.kind, o.logical)
			}
		}
	})

	DescribeTable("random transient and permanent failures, seeded",
		func(seed uint64, failRate, permanentShare float64) {
			plan := soakDefaultPlan(fmt.Sprintf("mixed seed %d", seed), seed)
			plan.failRate = failRate
			plan.permanentShare = permanentShare
			plan.asyncShare = 0.3
			plan.jitter = 0.2
			plan.noiseRate = 0.4
			r := runSoak(ctx, plan)

			phases := r.phases()
			if failRate > 0.2 && permanentShare > 0 {
				Expect(phases).To(HaveKey(phaseInvalid), "chaos with permanent errors rejects something")
			}
			if failRate > 0.2 && permanentShare < 0.5 {
				Expect(phases).To(HaveKey(phaseStalled), "chaos this heavy and mostly transient stalls something")
			}
			if failRate < 0.5 {
				Expect(phases[apimDeploymentPhaseSucceeded]+phases[phaseCreated]).To(BeNumerically(">", 0), "something gets through")
			}
		},
		Entry("light, transient only", uint64(11), 0.15, 0.0),
		Entry("moderate, mixed", uint64(12), 0.3, 0.25),
		Entry("heavy, mixed", uint64(13), 0.6, 0.3),
		Entry("heavy, mostly permanent", uint64(14), 0.5, 0.7),
	)

	It("handles spec changes, retry annotations and spec changes in the middle of a write", func() {
		plan := soakDefaultPlan("events", 21)
		plan.failRate = 0.4
		plan.permanentShare = 0.2
		plan.asyncShare = 0.3
		plan.jitter = 0.2
		plan.noiseRate = 0.3
		plan.events = true
		r := runSoak(ctx, plan)

		seen := map[string]int{}
		for _, o := range r.objects {
			for _, entry := range o.trace {
				if strings.HasPrefix(entry, "event:") {
					seen[strings.TrimPrefix(entry, "event:")]++
				}
			}
		}
		Expect(seen).To(HaveKey(soakEventSpecChange.String()))
		Expect(seen).To(HaveKey(soakEventRetryAnnotation.String()))
		Expect(seen).To(HaveKey(soakEventMidFlightSpecChange.String()))
		Expect(seen).To(HaveKey(soakEventNoise.String()))

		resumed := 0
		for _, o := range r.objects {
			if o.epoch > 0 && o.attempts > 1 {
				resumed++
			}
		}
		Expect(resumed).To(BeNumerically(">", 0), "a reset must lead to new attempts")
	})

	It("replays the same story for every object from the same seed", func() {
		plan := soakDefaultPlan("determinism", 31)
		plan.failRate = 0.4
		plan.permanentShare = 0.2
		plan.asyncShare = 0.3
		plan.noiseRate = 0.3
		plan.events = true
		plan.jitter = 0.2

		first := runSoak(ctx, plan)
		second := runSoak(ctx, plan)
		Expect(second.traces()).To(Equal(first.traces()))
		Expect(second.driver.now()).To(Equal(first.driver.now()), "the same virtual end time")
		Expect(second.driver.reconcileCount).To(Equal(first.driver.reconcileCount))

		other := plan
		other.seed = 32
		third := runSoak(ctx, other)
		Expect(third.traces()).NotTo(Equal(first.traces()), "a different seed tells a different story")
	})

	It("removes products from APIM under chaos, or holds the finalizer when Stalled or Invalid", func() {
		plan := soakPlan{name: "product deletes", seed: 41, products: 24}
		plan.deleteProducts = true
		plan.failRate = 0.35
		plan.permanentShare = 0.3
		plan.jitter = 0.2
		plan.noiseRate = 0.3
		r := runSoak(ctx, plan)

		gone, held := 0, 0
		for _, o := range r.objects {
			Expect(o.deleting).To(BeTrue(), "%s was deleted", o.logical)
			if o.gone {
				gone++
				continue
			}
			held++
			Expect(o.phase).To(BeElementOf(phaseStalled, phaseInvalid), o.logical)

			product := &apimv1.APIMProduct{}
			Expect(k8sClient.Get(ctx, o.key, product)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(product, productFinalizer)).To(BeTrue(),
				"%s: a product that could not be removed keeps its finalizer", o.logical)
			Expect(product.Status.Message).To(ContainSubstring("deletionPolicy"),
				"%s: the message says how to let go without APIM", o.logical)
		}
		Expect(gone).To(BeNumerically(">", 0))
		Expect(held).To(BeNumerically(">", 0))
	})

	It("still polls once at the deadline when Retry-After is longer than the remaining wait", func() {
		apim.AsyncWaitTimeout = 40 * time.Millisecond
		var polls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/asyncops/"):
				polls.Add(1)
				w.Header().Set("Retry-After", "3600")
				writeARMJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
			case r.Method == http.MethodGet:
				writeARMError(w, http.StatusNotFound, "ResourceNotFound", "no API yet")
			default:
				w.Header().Set("Azure-AsyncOperation", "/asyncops/slow?api-version=2021-08-01")
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusAccepted)
			}
		}))
		DeferCleanup(server.Close)
		DeferCleanup(apim.UseEndpoint(server.URL, server.Client()))

		started := time.Now()
		err := apim.ImportOpenAPIDefinitionToAPIM(ctx, apim.APIMDeploymentConfig{
			SubscriptionID: soakSubscription, ResourceGroup: soakResourceGroup, ServiceName: "slow",
			APIID: "slow-api", RoutePrefix: "/slow", BearerToken: soakToken,
		}, []byte(soakOpenAPIDoc))

		Expect(err).NotTo(HaveOccurred(), "an operation that finished during the wait is a success")
		Expect(polls.Load()).To(Equal(int32(1)))
		Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second), "Retry-After is bounded by the wait")
	})

	It("times out an async import that outlives the wait with a transient, distinguishable error", func() {
		apim.AsyncWaitTimeout = 20 * time.Millisecond
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/asyncops/"):
				w.Header().Set("Retry-After", "0")
				writeARMJSON(w, http.StatusOK, `{"status":"InProgress"}`)
			case r.Method == http.MethodGet:
				writeARMError(w, http.StatusNotFound, "ResourceNotFound", "no API yet")
			default:
				w.Header().Set("Location", "/asyncops/forever?api-version=2021-08-01")
				w.WriteHeader(http.StatusAccepted)
			}
		}))
		DeferCleanup(server.Close)
		DeferCleanup(apim.UseEndpoint(server.URL, server.Client()))

		err := apim.ImportOpenAPIDefinitionToAPIM(ctx, apim.APIMDeploymentConfig{
			SubscriptionID: soakSubscription, ResourceGroup: soakResourceGroup, ServiceName: "slow",
			APIID: "slow-api", RoutePrefix: "/slow", BearerToken: soakToken,
		}, []byte(soakOpenAPIDoc))

		Expect(errors.Is(err, apim.ErrImportWaitTimeout)).To(BeTrue(), "got %v", err)
		var apimErr *apim.Error
		Expect(errors.As(err, &apimErr)).To(BeTrue())
		Expect(classifyAPIMError(err)).To(Equal(errorClassTransient))
	})
})
