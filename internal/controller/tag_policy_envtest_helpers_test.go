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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// The helpers in this file let one matrix of envtest specs run against both APIMTag and
// APIMInboundPolicy. Each kind is described once by a tpeKind; a tpeHarness owns the fake
// ARM, the fake clock, the token counter and the resource for one spec. Everything is
// prefixed tpe ("tag/policy envtest") so it cannot collide with the other test files.

const (
	tpeSubscription  = "00000000-0000-0000-0000-0000000000e7"
	tpeResourceGroup = "rg-tpe"
	tpeToken         = "tpe-token"
	tpeTagID         = "tpe-tag-id"
	tpeAPIID         = "tpe-api-id"
	tpePolicyXML     = "<policies><inbound><base /></inbound><backend><base /></backend><outbound><base /></outbound><on-error><base /></on-error></policies>"
)

// tpeStart is where every spec's fake clock begins.
var tpeStart = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// tpeSeq makes resource names unique across specs, so a spec never sees a leftover.
var tpeSeq atomic.Int64

// tpeReply is one canned ARM answer. hangup closes the connection without answering,
// which the client sees as a transport error.
type tpeReply struct {
	status int
	body   string
	hangup bool
}

// tpeOK is a plain 200.
var tpeOK = tpeReply{status: http.StatusOK, body: `{}`}

// tpeFail is an ARM error reply with an Azure code (none when code is empty).
func tpeFail(status int, code string) tpeReply {
	if code == "" {
		return tpeReply{status: status, body: `{}`}
	}
	return tpeReply{status: status, body: fmt.Sprintf(`{"error":{"code":%q,"message":"tpe says no"}}`, code)}
}

// tpeFailDetail is an ARM error whose top-level code is generic and whose first detail
// carries the meaningful code.
func tpeFailDetail(status int, code, detail string) tpeReply {
	return tpeReply{status: status, body: fmt.Sprintf(
		`{"error":{"code":%q,"message":"outer","details":[{"code":%q,"message":"inner"}]}}`, code, detail)}
}

// tpeHangup drops the connection.
var tpeHangup = tpeReply{hangup: true}

// tpeRequest is one request the fake ARM saw.
type tpeRequest struct {
	method  string
	path    string
	query   string
	auth    string
	ifMatch string
	body    string
}

// tpeFakeARM stands in for Azure Resource Manager. Each request takes the next queued
// reply; the last one repeats.
type tpeFakeARM struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  []tpeReply
	requests []tpeRequest
}

func newTpeFakeARM() *tpeFakeARM {
	f := &tpeFakeARM{replies: []tpeReply{tpeOK}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, tpeRequest{
			method:  r.Method,
			path:    r.URL.Path,
			query:   r.URL.RawQuery,
			auth:    r.Header.Get("Authorization"),
			ifMatch: r.Header.Get("If-Match"),
			body:    string(body),
		})
		reply := f.replies[0]
		if len(f.replies) > 1 {
			f.replies = f.replies[1:]
		}
		f.mu.Unlock()
		if reply.hangup {
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusTeapot)
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	return f
}

// reply makes every following request get these replies in order, the last repeating.
func (f *tpeFakeARM) reply(replies ...tpeReply) {
	Expect(replies).NotTo(BeEmpty())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append([]tpeReply(nil), replies...)
}

// calls is how many requests ARM has seen.
func (f *tpeFakeARM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// all returns a copy of the requests seen so far.
func (f *tpeFakeARM) all() []tpeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tpeRequest(nil), f.requests...)
}

// tpeView is the part of a tag or policy the matrix asserts on.
type tpeView struct {
	Generation         int64
	Annotations        map[string]string
	Phase              string
	Message            string
	ObservedGeneration int64
	Retry              apimv1.RetryStatus
}

// tpeKind adapts one APIM-writing kind to the matrix.
type tpeKind struct {
	// kind is the name in logs, e.g. "APIMTag".
	kind string
	// idKey and idValue are the extra log key the controller adds (tagID or apiID).
	idKey   string
	idValue string
	// operationID, for a policy, targets one operation instead of the whole API.
	operationID string
	// successMessage is status.message after a successful write.
	successMessage string
	// failurePrefix starts status.message after a failed write.
	failurePrefix string
	// armPath is the path the upsert PUTs to.
	armPath func(service string) string
	// newObject returns an empty object of the kind.
	newObject func() client.Object
	// create creates the resource.
	create func(ctx context.Context, key types.NamespacedName, service string, annotations map[string]string)
	// view reads the fields the matrix asserts on.
	view func(obj client.Object) tpeView
	// setRetry overwrites phase, observedGeneration and the retry status in memory.
	setRetry func(obj client.Object, phase string, observedGeneration int64, rs apimv1.RetryStatus)
	// bumpSpec changes the spec in memory so the generation goes up.
	bumpSpec func(obj client.Object, n int)
	// reconciler builds the kind's reconciler with the given seams.
	reconciler func(getToken managementTokenFunc, policy *retryPolicy) reconcile.Reconciler
	// setup registers the reconciler with a manager.
	setup func(mgr ctrl.Manager, getToken managementTokenFunc, policy *retryPolicy) error
	// updateFilter is the update predicate SetupWithManager installs.
	updateFilter func(e event.UpdateEvent) bool
}

func tpeServicePath(service string) string {
	return "/subscriptions/" + tpeSubscription + "/resourceGroups/" + tpeResourceGroup +
		"/providers/Microsoft.ApiManagement/service/" + service
}

// tpeTagKind describes APIMTag.
func tpeTagKind() tpeKind {
	return tpeKind{
		kind:           "APIMTag",
		idKey:          "tagID",
		idValue:        tpeTagID,
		successMessage: "Tag created or updated",
		failurePrefix:  "Failed to create or update tag in APIM",
		armPath: func(service string) string {
			return tpeServicePath(service) + "/tags/" + tpeTagID
		},
		newObject: func() client.Object { return &apimv1.APIMTag{} },
		create: func(ctx context.Context, key types.NamespacedName, service string, annotations map[string]string) {
			GinkgoHelper()
			Expect(k8sClient.Create(ctx, &apimv1.APIMTag{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Annotations: annotations},
				Spec: apimv1.APIMTagSpec{
					APIMService: service,
					TagID:       tpeTagID,
					DisplayName: "TPE Tag",
				},
			})).To(Succeed())
		},
		view: func(obj client.Object) tpeView {
			t := obj.(*apimv1.APIMTag)
			return tpeView{
				Generation: t.Generation, Annotations: t.Annotations,
				Phase: t.Status.Phase, Message: t.Status.Message,
				ObservedGeneration: t.Status.ObservedGeneration, Retry: t.Status.RetryStatus,
			}
		},
		setRetry: func(obj client.Object, phase string, observedGeneration int64, rs apimv1.RetryStatus) {
			t := obj.(*apimv1.APIMTag)
			t.Status.Phase = phase
			t.Status.ObservedGeneration = observedGeneration
			t.Status.RetryStatus = rs
		},
		bumpSpec: func(obj client.Object, n int) {
			obj.(*apimv1.APIMTag).Spec.DisplayName = fmt.Sprintf("TPE Tag v%d", n)
		},
		reconciler: func(getToken managementTokenFunc, policy *retryPolicy) reconcile.Reconciler {
			return &APIMTagReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: getToken, retry: policy}
		},
		setup: func(mgr ctrl.Manager, getToken managementTokenFunc, policy *retryPolicy) error {
			return (&APIMTagReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), getToken: getToken, retry: policy}).SetupWithManager(mgr)
		},
		updateFilter: specOrDeletionChanged().Update,
	}
}

// tpePolicyKind describes APIMInboundPolicy, at API level or, with operationID, at
// operation level.
func tpePolicyKind(operationID string) tpeKind {
	k := tpeKind{
		kind:           "APIMInboundPolicy",
		idKey:          "apiID",
		idValue:        tpeAPIID,
		operationID:    operationID,
		successMessage: "APIM Inbound Policy created or updated",
		failurePrefix:  "Failed to create or update inbound policy in APIM",
		armPath: func(service string) string {
			if operationID != "" {
				return tpeServicePath(service) + "/apis/" + tpeAPIID + "/operations/" + operationID + "/policies/policy"
			}
			return tpeServicePath(service) + "/apis/" + tpeAPIID + "/policies/policy"
		},
		newObject: func() client.Object { return &apimv1.APIMInboundPolicy{} },
		create: func(ctx context.Context, key types.NamespacedName, service string, annotations map[string]string) {
			GinkgoHelper()
			Expect(k8sClient.Create(ctx, &apimv1.APIMInboundPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Annotations: annotations},
				Spec: apimv1.APIMInboundPolicySpec{
					APIMService:   service,
					APIID:         tpeAPIID,
					OperationID:   operationID,
					PolicyContent: tpePolicyXML,
				},
			})).To(Succeed())
		},
		view: func(obj client.Object) tpeView {
			p := obj.(*apimv1.APIMInboundPolicy)
			return tpeView{
				Generation: p.Generation, Annotations: p.Annotations,
				Phase: p.Status.Phase, Message: p.Status.Message,
				ObservedGeneration: p.Status.ObservedGeneration, Retry: p.Status.RetryStatus,
			}
		},
		setRetry: func(obj client.Object, phase string, observedGeneration int64, rs apimv1.RetryStatus) {
			p := obj.(*apimv1.APIMInboundPolicy)
			p.Status.Phase = phase
			p.Status.ObservedGeneration = observedGeneration
			p.Status.RetryStatus = rs
		},
		bumpSpec: func(obj client.Object, n int) {
			obj.(*apimv1.APIMInboundPolicy).Spec.PolicyContent = strings.Replace(tpePolicyXML,
				"<inbound><base /></inbound>", fmt.Sprintf(`<inbound><base /><set-header name="x-tpe" exists-action="override"><value>%d</value></set-header></inbound>`, n), 1)
		},
		reconciler: func(getToken managementTokenFunc, policy *retryPolicy) reconcile.Reconciler {
			return &APIMInboundPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), getToken: getToken, retry: policy}
		},
		setup: func(mgr ctrl.Manager, getToken managementTokenFunc, policy *retryPolicy) error {
			return (&APIMInboundPolicyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), getToken: getToken, retry: policy}).SetupWithManager(mgr)
		},
		updateFilter: apimInboundPolicyUpdateFilter(),
	}
	if operationID != "" {
		k.successMessage = "APIM Inbound Policy created or updated for operation " + operationID
	}
	return k
}

// tpeKinds are the kinds the matrix runs for: the tag, and the policy at API and at
// operation level.
func tpeKinds() []tpeKind {
	return []tpeKind{tpeTagKind(), tpePolicyKind(""), tpePolicyKind("tpe-operation")}
}

// tpeLabel names a kind in spec descriptions.
func (k tpeKind) label() string {
	if k.operationID != "" {
		return k.kind + " (operation level)"
	}
	return k.kind
}

// tpeHarness is one spec's world: a resource of one kind, its APIMService, a fake ARM, a
// fake clock and a reconciler wired to all of them.
type tpeHarness struct {
	k       tpeKind
	ctx     context.Context
	key     types.NamespacedName
	service string
	arm     *tpeFakeARM
	clock   *fakeClock
	policy  *retryPolicy
	logs    func() []logLine

	tokenMu    sync.Mutex
	tokenCalls int
	tokenErr   error

	restore []func()
}

// newTpeHarness creates the APIMService and the resource and points the apim package at a
// fresh fake ARM. annotations go on the resource at creation. Cleanup is registered with
// DeferCleanup.
func newTpeHarness(k tpeKind, annotations map[string]string) *tpeHarness {
	GinkgoHelper()
	n := tpeSeq.Add(1)
	logger, lines := captureLogs()
	h := &tpeHarness{
		k:       k,
		ctx:     log.IntoContext(context.Background(), logger),
		logs:    lines,
		key:     types.NamespacedName{Name: fmt.Sprintf("tpe-%s-%d", strings.ToLower(k.kind), n), Namespace: "default"},
		service: fmt.Sprintf("tpe-svc-%d", n),
		arm:     newTpeFakeARM(),
		clock:   &fakeClock{t: tpeStart},
	}
	h.policy = &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: h.clock.now}
	h.restore = append(h.restore, stubAzureIdentityEnv(), apim.UseEndpoint(h.arm.srv.URL, h.arm.srv.Client()))

	Expect(k8sClient.Create(h.ctx, &apimv1.APIMService{
		ObjectMeta: metav1.ObjectMeta{Name: h.service, Namespace: getOperatorNamespace()},
		Spec: apimv1.APIMServiceSpec{
			Name:          h.service,
			ResourceGroup: tpeResourceGroup,
			Subscription:  tpeSubscription,
		},
	})).To(Succeed())
	k.create(h.ctx, h.key, h.service, annotations)

	DeferCleanup(h.close)
	return h
}

// close undoes everything newTpeHarness set up.
func (h *tpeHarness) close() {
	for i := len(h.restore) - 1; i >= 0; i-- {
		h.restore[i]()
	}
	h.arm.srv.Close()
	obj := h.k.newObject()
	obj.SetName(h.key.Name)
	obj.SetNamespace(h.key.Namespace)
	Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), obj))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), &apimv1.APIMService{
		ObjectMeta: metav1.ObjectMeta{Name: h.service, Namespace: getOperatorNamespace()},
	}))).To(Succeed())
}

// getToken is the reconciler's token seam: it counts calls and returns tokenErr if set.
func (h *tpeHarness) getToken(context.Context, string, string) (string, error) {
	h.tokenMu.Lock()
	defer h.tokenMu.Unlock()
	h.tokenCalls++
	if h.tokenErr != nil {
		return "", h.tokenErr
	}
	return tpeToken, nil
}

// tokens is how many tokens were asked for.
func (h *tpeHarness) tokens() int {
	h.tokenMu.Lock()
	defer h.tokenMu.Unlock()
	return h.tokenCalls
}

// reconcile runs one reconcile. A failed APIM write must never come back as an error:
// that would put controller-runtime's own rate limiter back in charge.
func (h *tpeHarness) reconcile() ctrl.Result {
	GinkgoHelper()
	result, err := h.k.reconciler(h.getToken, h.policy).Reconcile(h.ctx, reconcile.Request{NamespacedName: h.key})
	Expect(err).NotTo(HaveOccurred(), "a failed APIM write must never be handed to the rate limiter")
	return result
}

// object reads the resource.
func (h *tpeHarness) object() client.Object {
	GinkgoHelper()
	obj := h.k.newObject()
	Expect(k8sClient.Get(h.ctx, h.key, obj)).To(Succeed())
	return obj
}

// view reads the resource's asserted fields.
func (h *tpeHarness) view() tpeView {
	GinkgoHelper()
	return h.k.view(h.object())
}

// puts is how many PUTs ARM saw for the resource's own path.
func (h *tpeHarness) puts() int {
	n := 0
	for _, r := range h.arm.all() {
		if r.method == http.MethodPut && r.path == h.k.armPath(h.service) {
			n++
		}
	}
	return n
}

// setStatus writes phase, observedGeneration and the retry status directly. A negative
// observedGeneration means "the current generation".
func (h *tpeHarness) setStatus(phase string, observedGeneration int64, rs apimv1.RetryStatus) {
	GinkgoHelper()
	obj := h.object()
	if observedGeneration < 0 {
		observedGeneration = obj.GetGeneration()
	}
	h.k.setRetry(obj, phase, observedGeneration, rs)
	Expect(k8sClient.Status().Update(h.ctx, obj)).To(Succeed())
}

// changeSpec bumps the spec, and with it the generation.
func (h *tpeHarness) changeSpec(n int) {
	GinkgoHelper()
	obj := h.object()
	before := obj.GetGeneration()
	h.k.bumpSpec(obj, n)
	Expect(k8sClient.Update(h.ctx, obj)).To(Succeed())
	Expect(h.view().Generation).To(BeNumerically(">", before), "a spec change bumps the generation")
}

// annotate sets the retry annotation; an empty value removes it.
func (h *tpeHarness) annotate(value string) {
	GinkgoHelper()
	obj := h.object()
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if value == "" {
		delete(annotations, retryAnnotation)
	} else {
		annotations[retryAnnotation] = value
	}
	obj.SetAnnotations(annotations)
	Expect(k8sClient.Update(h.ctx, obj)).To(Succeed())
}

// label sets a label, a metadata-only change that leaves the generation alone.
func (h *tpeHarness) label(value string) {
	GinkgoHelper()
	obj := h.object()
	before := obj.GetGeneration()
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["tpe"] = value
	obj.SetLabels(labels)
	Expect(k8sClient.Update(h.ctx, obj)).To(Succeed())
	Expect(h.view().Generation).To(Equal(before), "a label does not bump the generation")
}

// failTransiently fails the write times in a row with a 503, reconciling each time the
// previous backoff is over, and returns the requeues.
func (h *tpeHarness) failTransiently(times int) []time.Duration {
	GinkgoHelper()
	h.arm.reply(tpeFail(http.StatusServiceUnavailable, "ServiceUnavailable"))
	waits := make([]time.Duration, 0, times)
	for range times {
		result := h.reconcile()
		waits = append(waits, result.RequeueAfter)
		h.clock.advance(result.RequeueAfter)
	}
	return waits
}

// failNow fails one write with reply without moving the clock, so the resource is left
// in the middle of its backoff.
func (h *tpeHarness) failNow(reply tpeReply) {
	GinkgoHelper()
	h.arm.reply(reply)
	Expect(h.reconcile().RequeueAfter).To(BeNumerically(">", 0), "a transient failure backs off")
}

// linesWith returns the captured log lines with exactly this message.
func (h *tpeHarness) linesWith(msg string) []logLine {
	var out []logLine
	for _, l := range h.logs() {
		if l.msg == msg {
			out = append(out, l)
		}
	}
	return out
}

// expectIdentity checks the keys every APIM write log line carries.
func (h *tpeHarness) expectIdentity(l logLine) {
	GinkgoHelper()
	Expect(l.kv).To(HaveKeyWithValue("kind", h.k.kind))
	Expect(l.kv).To(HaveKeyWithValue("namespace", h.key.Namespace))
	Expect(l.kv).To(HaveKeyWithValue("name", h.key.Name))
	Expect(l.kv).To(HaveKeyWithValue(h.k.idKey, h.k.idValue))
	if h.k.operationID != "" {
		Expect(l.kv).To(HaveKeyWithValue("operationID", h.k.operationID))
	}
}

// tpeQuietLogger keeps a manager's own logging out of the spec output.
func tpeQuietLogger() logr.Logger { return logr.Discard() }

// failNow403 makes the resource Invalid with a 403.
func (h *tpeHarness) failNow403() {
	GinkgoHelper()
	h.arm.reply(tpeFail(http.StatusForbidden, "LinkedAuthorizationFailed"))
	Expect(h.reconcile()).To(BeZero())
}

// tpeDeleteService deletes the harness's APIMService.
func tpeDeleteService(h *tpeHarness) {
	GinkgoHelper()
	Expect(k8sClient.Delete(h.ctx, &apimv1.APIMService{
		ObjectMeta: metav1.ObjectMeta{Name: h.service, Namespace: getOperatorNamespace()},
	})).To(Succeed())
}

// tpeRecreateService creates the harness's APIMService again.
func tpeRecreateService(h *tpeHarness) {
	GinkgoHelper()
	Expect(k8sClient.Create(h.ctx, &apimv1.APIMService{
		ObjectMeta: metav1.ObjectMeta{Name: h.service, Namespace: getOperatorNamespace()},
		Spec: apimv1.APIMServiceSpec{
			Name:          h.service,
			ResourceGroup: tpeResourceGroup,
			Subscription:  tpeSubscription,
		},
	})).To(Succeed())
}

// tpeUnsetIdentity removes AZURE_CLIENT_ID; the harness restores the env at cleanup.
func tpeUnsetIdentity(*tpeHarness) {
	GinkgoHelper()
	Expect(os.Unsetenv("AZURE_CLIENT_ID")).To(Succeed())
}

// tpeRestoreIdentity sets AZURE_CLIENT_ID again.
func tpeRestoreIdentity(*tpeHarness) {
	GinkgoHelper()
	Expect(os.Setenv("AZURE_CLIENT_ID", "test-client-id")).To(Succeed())
}
