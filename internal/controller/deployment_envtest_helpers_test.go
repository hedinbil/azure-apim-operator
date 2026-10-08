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
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// Helpers for apimapideployment_retry_envtest_test.go: a fake ARM that counts every
// request by step and by path, a switchable OpenAPI document server, a settable clock,
// and a fixture that wires an APIMAPIDeployment (with its APIMAPI, APIMService,
// ReplicaSet and ready pod) to a reconciler that talks to the fake. Every identifier is
// prefixed depEnv so nothing collides with the other specs of the package.

const (
	depEnvSubscription  = "00000000-0000-0000-0000-0000000000de"
	depEnvResourceGroup = "rg-depenv"
	depEnvService       = "depenv-apim-service"
	depEnvToken         = "depenv-token"
	depEnvRoutePrefix   = "/depenv"
	depEnvBackend       = "https://backend.depenv.net"
	depEnvWSBackend     = "wss://backend.depenv.net/hub"
	depEnvDocV1         = `{"openapi":"3.0.0","info":{"title":"depenv","version":"1.0.0"},"paths":{}}`
	depEnvDocV2         = `{"openapi":"3.0.0","info":{"title":"depenv","version":"2.0.0"},"paths":{}}`
)

// Steps of one deployment reconcile, as depEnvARM names the requests it receives.
const (
	depEnvGetAPI    = "GET api"
	depEnvImport    = "PUT import"
	depEnvWebSocket = "PUT websocket"
	// depEnvPatchAPI is a PATCH of the API itself, which the controller no longer sends:
	// serviceUrl and subscriptionRequired travel in the import (or websocket) PUT.
	depEnvPatchAPI       = "PATCH api"
	depEnvProduct        = "PUT product"
	depEnvTag            = "PUT tag"
	depEnvServiceDetails = "GET service"
	depEnvPoll           = "GET poll"
	depEnvUnknown        = "unknown"
)

// depEnvAsyncPath is where the fake's 202 answers point the operation. The controller
// reads it on the reconciles after the one that sent the write, never within it.
const depEnvAsyncPath = "/depenv-asyncops/op-1"

// depEnvStart is the fake clock's starting point, on a whole second so the delays the
// retry policy computes come back exactly.
var depEnvStart = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// depEnvRequest is one request the fake ARM received.
type depEnvRequest struct {
	Step   string
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   string
}

// depEnvResponder answers a request instead of the fake's default success. It returns
// false to fall through to the default (e.g. a responder that only fails one product).
type depEnvResponder func(w http.ResponseWriter, r *http.Request) bool

// depEnvARM stands in for the Azure Management API of one APIM service.
type depEnvARM struct {
	server      *httptest.Server
	servicePath string
	apiPath     string

	mu         sync.Mutex
	requests   []depEnvRequest
	responders map[string]depEnvResponder
}

func newDepEnvARM(apiID string) *depEnvARM {
	f := &depEnvARM{
		servicePath: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s",
			depEnvSubscription, depEnvResourceGroup, depEnvService),
		responders: map[string]depEnvResponder{},
	}
	f.apiPath = f.servicePath + "/apis/" + apiID
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *depEnvARM) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	step := f.classify(r, string(body))

	f.mu.Lock()
	f.requests = append(f.requests, depEnvRequest{
		Step:   step,
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
		Body:   string(body),
	})
	responder := f.responders[step]
	f.mu.Unlock()

	if responder != nil && responder(w, r) {
		return
	}

	switch step {
	case depEnvGetAPI:
		depEnvWriteError(w, http.StatusNotFound, "ResourceNotFound", "API not found")
	case depEnvImport, depEnvWebSocket:
		depEnvWriteJSON(w, http.StatusCreated, `{"name":"api"}`)
	case depEnvServiceDetails:
		depEnvWriteJSON(w, http.StatusOK, `{"properties":{"hostnameConfigurations":[`+
			`{"type":"Proxy","hostName":"gw.depenv.net"},{"type":"DeveloperPortal","hostName":"portal.depenv.net"}]}}`)
	case depEnvPoll:
		depEnvWriteJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
	case depEnvUnknown:
		depEnvWriteError(w, http.StatusTeapot, "NotRoutedByFake", r.Method+" "+r.URL.Path)
	default:
		depEnvWriteJSON(w, http.StatusOK, `{}`)
	}
}

// classify names the step a request belongs to. The websocket body also carries
// serviceUrl and subscriptionRequired, so the method is checked before the body.
// importEnvelope is the JSON body of an API import: the document in properties.value,
// with the path and backend serviceUrl set in the same write.
type importEnvelope struct {
	Properties struct {
		Format               string `json:"format"`
		Value                string `json:"value"`
		Path                 string `json:"path"`
		ServiceURL           string `json:"serviceUrl"`
		SubscriptionRequired *bool  `json:"subscriptionRequired"`
	} `json:"properties"`
}

// decodeImportEnvelope parses a PUT body as an import envelope; ok is false for any other
// body, e.g. a websocket API's properties or a policy.
func decodeImportEnvelope(body []byte) (importEnvelope, bool) {
	var env importEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return env, false
	}
	switch env.Properties.Format {
	case "openapi+json", "swagger-json":
		return env, true
	}
	// Other PUT bodies carry a format too, e.g. a policy's "rawxml".
	return env, false
}

// isImportEnvelope reports whether a PUT on an API is an import rather than a websocket
// API upsert.
func isImportEnvelope(body []byte) bool {
	_, ok := decodeImportEnvelope(body)
	return ok
}

// importedDocument returns the OpenAPI document an import envelope carries.
func importedDocument(body string) string {
	env, _ := decodeImportEnvelope([]byte(body))
	return env.Properties.Value
}

// importedServiceURL returns the backend serviceUrl an import envelope sets.
func importedServiceURL(body string) string {
	env, _ := decodeImportEnvelope([]byte(body))
	return env.Properties.ServiceURL
}

func (f *depEnvARM) classify(r *http.Request, body string) string {
	p := r.URL.Path
	isAPI := p == f.apiPath || strings.HasPrefix(p, f.apiPath+";rev=")
	switch {
	case strings.HasPrefix(p, "/depenv-asyncops/") && r.Method == http.MethodGet:
		return depEnvPoll
	case p == f.servicePath && r.Method == http.MethodGet:
		return depEnvServiceDetails
	case isAPI && r.Method == http.MethodGet:
		return depEnvGetAPI
	case isAPI && r.Method == http.MethodPut && isImportEnvelope([]byte(body)):
		return depEnvImport
	case isAPI && r.Method == http.MethodPut:
		return depEnvWebSocket
	case isAPI && r.Method == http.MethodPatch:
		return depEnvPatchAPI
	case strings.HasPrefix(p, f.servicePath+"/products/") && strings.HasSuffix(p, strings.TrimPrefix(f.apiPath, f.servicePath)) &&
		r.Method == http.MethodPut:
		return depEnvProduct
	case strings.HasPrefix(p, f.apiPath+"/tags/") && r.Method == http.MethodPut:
		return depEnvTag
	}
	return depEnvUnknown
}

// on makes step answer with responder from now on; nil restores the default success.
func (f *depEnvARM) on(step string, responder depEnvResponder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if responder == nil {
		delete(f.responders, step)
		return
	}
	f.responders[step] = responder
}

// total is the number of requests so far.
func (f *depEnvARM) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// count is the number of requests of one step so far.
func (f *depEnvARM) count(step string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Step == step {
			n++
		}
	}
	return n
}

// countPath is the number of requests with method to the service-relative path rel,
// e.g. countPath(http.MethodPut, "/products/p1/apis/<apiID>").
func (f *depEnvARM) countPath(method, rel string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method && r.Path == f.servicePath+rel {
			n++
		}
	}
	return n
}

// counts maps every step seen so far to how often it was requested.
func (f *depEnvARM) counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, r := range f.requests {
		out[r.Step]++
	}
	return out
}

// stepsSince lists the steps requested after the first n requests.
func (f *depEnvARM) stepsSince(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests)-n)
	for _, r := range f.requests[n:] {
		out = append(out, r.Step)
	}
	return out
}

// last returns the most recent request of step; it fails the spec when there is none.
func (f *depEnvARM) last(step string) depEnvRequest {
	GinkgoHelper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].Step == step {
			return f.requests[i]
		}
	}
	Fail("no request of step " + step)
	return depEnvRequest{}
}

func depEnvWriteJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func depEnvWriteError(w http.ResponseWriter, status int, code, message string) {
	depEnvWriteJSON(w, status, fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message))
}

// depEnvFail answers with an ARM error body.
func depEnvFail(status int, code string) depEnvResponder {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		depEnvWriteError(w, status, code, "injected by the depenv test")
		return true
	}
}

// depEnvFailBody answers with status and a raw body.
func depEnvFailBody(status int, body string) depEnvResponder {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		depEnvWriteJSON(w, status, body)
		return true
	}
}

// depEnvFailPath fails only requests whose path contains fragment.
func depEnvFailPath(fragment string, status int, code string) depEnvResponder {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, fragment) {
			return false
		}
		depEnvWriteError(w, status, code, "injected by the depenv test")
		return true
	}
}

// depEnvAccepted answers 202 with a relative Azure-AsyncOperation URL and, when
// retryAfter is not empty, a Retry-After header.
func depEnvAccepted(retryAfter string) depEnvResponder {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Azure-AsyncOperation", depEnvAsyncPath+"?api-version=2021-08-01")
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusAccepted)
		return true
	}
}

// depEnvHangUp closes the connection without answering, as a dropped connection or a
// timed-out gateway would.
func depEnvHangUp() depEnvResponder {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return false
		}
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
		return true
	}
}

// depEnvPollAnswer answers a poll with status and body.
func depEnvPollAnswer(status int, body string) depEnvResponder {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		depEnvWriteJSON(w, status, body)
		return true
	}
}

// depEnvDocServer serves the OpenAPI document, counts the fetches and can be made to fail.
type depEnvDocServer struct {
	server *httptest.Server

	mu      sync.Mutex
	content string
	failing bool
	hits    int
	// cycle, when set, is served in turn instead of content, one document per fetch.
	cycle []string
}

func newDepEnvDocServer(content string) *depEnvDocServer {
	d := &depEnvDocServer{content: content}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		content, failing := d.content, d.failing
		if len(d.cycle) > 0 {
			content = d.cycle[d.hits%len(d.cycle)]
		}
		d.hits++
		d.mu.Unlock()
		if failing {
			http.Error(w, "document temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, content)
	}))
	return d
}

// publishNewVersion serves depEnvDocV2 from now on, a different document and so a new
// desired hash.
func (d *depEnvDocServer) publishNewVersion() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.content = depEnvDocV2
}

// alternate serves docs in turn from now on, a different one on every fetch, like two
// app versions behind one Service or a generator that stamps the time into the document.
func (d *depEnvDocServer) alternate(docs ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cycle = append([]string(nil), docs...)
	d.hits = 0
}

func (d *depEnvDocServer) fail(failing bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing = failing
}

func (d *depEnvDocServer) fetches() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hits
}

// depEnvClock is a settable clock for the retry policy.
type depEnvClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *depEnvClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *depEnvClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// setTo moves the clock to an RFC3339 time such as status.nextAttemptAt.
func (c *depEnvClock) setTo(rfc3339 string) {
	GinkgoHelper()
	t, err := time.Parse(time.RFC3339, rfc3339)
	Expect(err).NotTo(HaveOccurred())
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// depEnvOptions shape the deployment a fixture creates.
type depEnvOptions struct {
	webSocket   bool
	revision    string
	productIDs  []string
	tagIDs      []string
	annotations map[string]string
}

// depEnvCounter keeps namespaces and names unique per spec.
var depEnvCounter atomic.Int32

// depEnvFixture is one APIMAPIDeployment wired to a fake ARM through a reconciler.
type depEnvFixture struct {
	ctx        context.Context
	ns         string
	name       string
	apiID      string
	rsName     string
	key        types.NamespacedName
	arm        *depEnvARM
	doc        *depEnvDocServer
	clock      *depEnvClock
	reconciler *APIMAPIDeploymentReconciler
	tokenCalls atomic.Int32
}

// newDepEnvFixture creates, in a namespace of its own, an APIMAPI, a ReplicaSet with a
// ready pod and the APIMAPIDeployment, plus the shared APIMService in the operator
// namespace. The reconciler gets an injected token, the fake ARM, and the production
// retry delays without jitter on a fake clock. Everything is undone by DeferCleanup.
func newDepEnvFixture(opts depEnvOptions) *depEnvFixture {
	GinkgoHelper()
	n := depEnvCounter.Add(1)
	f := &depEnvFixture{
		ctx:   context.Background(),
		ns:    fmt.Sprintf("depenv-%d", n),
		name:  fmt.Sprintf("depenv-api-%d", n),
		apiID: fmt.Sprintf("depenv-api-id-%d", n),
		clock: &depEnvClock{t: depEnvStart},
	}
	f.rsName = f.name + "-rs"
	f.key = types.NamespacedName{Name: f.name, Namespace: f.ns}

	By("serving the OpenAPI document and a fake ARM")
	f.doc = newDepEnvDocServer(depEnvDocV1)
	DeferCleanup(f.doc.server.Close)
	f.arm = newDepEnvARM(f.apiID)
	DeferCleanup(f.arm.server.Close)
	DeferCleanup(apim.UseEndpoint(f.arm.server.URL, f.arm.server.Client()))

	By("setting placeholder workload identity variables")
	DeferCleanup(unsetAzureIdentityEnvVars())
	Expect(os.Setenv("AZURE_CLIENT_ID", "depenv-client-id")).To(Succeed())
	Expect(os.Setenv("AZURE_TENANT_ID", "depenv-tenant-id")).To(Succeed())

	By("creating the namespace, APIMService, APIMAPI, ReplicaSet, ready pod and APIMAPIDeployment")
	Expect(k8sClient.Create(f.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.ns}})).To(Succeed())
	depEnvEnsureService(f.ctx)

	api := &apimv1.APIMAPI{
		ObjectMeta: metav1.ObjectMeta{Name: f.name, Namespace: f.ns},
		Spec: apimv1.APIMAPISpec{
			APIID:                f.apiID,
			APIMService:          depEnvService,
			RoutePrefix:          depEnvRoutePrefix,
			ServiceURL:           depEnvBackend,
			OpenAPIDefinitionURL: f.doc.server.URL,
		},
	}
	if opts.webSocket {
		api.Spec.Type = apimv1.APITypeWebSocket
		api.Spec.ServiceURL = depEnvWSBackend
		api.Spec.OpenAPIDefinitionURL = ""
	}
	Expect(k8sClient.Create(f.ctx, api)).To(Succeed())

	f.createReplicaSetWithReadyPod()

	deployment := &apimv1.APIMAPIDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: f.name, Namespace: f.ns, Annotations: opts.annotations},
		Spec: apimv1.APIMAPIDeploymentSpec{
			APIID:                f.apiID,
			APIMService:          depEnvService,
			APIMAPIName:          f.name,
			Subscription:         depEnvSubscription,
			ResourceGroup:        depEnvResourceGroup,
			RoutePrefix:          depEnvRoutePrefix,
			ServiceURL:           depEnvBackend,
			OpenAPIDefinitionURL: f.doc.server.URL,
			SubscriptionRequired: true,
			Revision:             opts.revision,
			ProductIDs:           opts.productIDs,
			TagIDs:               opts.tagIDs,
		},
	}
	if opts.webSocket {
		deployment.Spec.Type = apimv1.APITypeWebSocket
		deployment.Spec.ServiceURL = depEnvWSBackend
		deployment.Spec.OpenAPIDefinitionURL = ""
	}
	Expect(k8sClient.Create(f.ctx, deployment)).To(Succeed())

	By("building a reconciler with an injected token and a fake clock")
	f.reconciler = &APIMAPIDeploymentReconciler{
		Client:  k8sClient,
		Scheme:  k8sClient.Scheme(),
		fetcher: testOpenAPIFetcher(),
		getToken: func(context.Context, string, string) (string, error) {
			f.tokenCalls.Add(1)
			return depEnvToken, nil
		},
		// Production delays and attempts, without jitter, on a clock the specs move.
		retry: &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: f.clock.now},
	}
	return f
}

// depEnvEnsureService creates the APIMService in the operator namespace unless it exists.
func depEnvEnsureService(ctx context.Context) {
	GinkgoHelper()
	service := &apimv1.APIMService{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: depEnvService, Namespace: getOperatorNamespace()}, service)
	if err == nil {
		return
	}
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "unexpected error reading the APIMService: %v", err)
	service = &apimv1.APIMService{
		ObjectMeta: metav1.ObjectMeta{Name: depEnvService, Namespace: getOperatorNamespace()},
		Spec: apimv1.APIMServiceSpec{
			Name:          depEnvService,
			Subscription:  depEnvSubscription,
			ResourceGroup: depEnvResourceGroup,
		},
	}
	if err := k8sClient.Create(ctx, service); !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// createReplicaSetWithReadyPod creates the ReplicaSet the APIMAPI matches by name and one
// running, ready pod owned by it.
func (f *depEnvFixture) createReplicaSetWithReadyPod() {
	GinkgoHelper()
	podLabels := map[string]string{"app": f.name}
	one := int32(1)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      f.rsName,
			Namespace: f.ns,
			Labels:    map[string]string{"app.kubernetes.io/name": f.name},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}}},
			},
		},
	}
	Expect(k8sClient.Create(f.ctx, rs)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            f.name + "-pod",
			Namespace:       f.ns,
			Labels:          podLabels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}}},
	}
	Expect(k8sClient.Create(f.ctx, pod)).To(Succeed())
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	Expect(k8sClient.Status().Update(f.ctx, pod)).To(Succeed())
}

// reconcile runs one reconcile; every APIM failure must come back as a result, never as
// an error.
func (f *depEnvFixture) reconcile() ctrl.Result {
	GinkgoHelper()
	result, err := f.reconciler.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.key})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func (f *depEnvFixture) get() *apimv1.APIMAPIDeployment {
	GinkgoHelper()
	deployment := &apimv1.APIMAPIDeployment{}
	Expect(k8sClient.Get(f.ctx, f.key, deployment)).To(Succeed())
	return deployment
}

func (f *depEnvFixture) getAPI() *apimv1.APIMAPI {
	GinkgoHelper()
	api := &apimv1.APIMAPI{}
	Expect(k8sClient.Get(f.ctx, f.key, api)).To(Succeed())
	return api
}

// update re-reads the deployment, applies mutate and writes it back.
func (f *depEnvFixture) update(mutate func(*apimv1.APIMAPIDeployment)) {
	GinkgoHelper()
	deployment := f.get()
	mutate(deployment)
	Expect(k8sClient.Update(f.ctx, deployment)).To(Succeed())
}

// annotate sets the retry annotation to value.
func (f *depEnvFixture) annotate(value string) {
	GinkgoHelper()
	f.update(func(d *apimv1.APIMAPIDeployment) {
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[retryAnnotation] = value
	})
}

func (f *depEnvFixture) removeRetryAnnotation() {
	GinkgoHelper()
	f.update(func(d *apimv1.APIMAPIDeployment) { delete(d.Annotations, retryAnnotation) })
}

// reconcileWithoutAPIM reconciles and checks that neither ARM, the token source nor the
// status was touched, and returns the result.
func (f *depEnvFixture) reconcileWithoutAPIM() ctrl.Result {
	GinkgoHelper()
	before := f.get()
	calls, tokens := f.arm.total(), f.tokenCalls.Load()
	result := f.reconcile()
	Expect(f.arm.stepsSince(calls)).To(BeEmpty(), "no ARM request may be sent")
	Expect(f.tokenCalls.Load()).To(Equal(tokens), "no token may be requested")
	Expect(f.get().ResourceVersion).To(Equal(before.ResourceVersion), "the status must not be written")
	return result
}

// toNextAttempt moves the clock to status.nextAttemptAt when one is set.
func (f *depEnvFixture) toNextAttempt() {
	GinkgoHelper()
	if next := f.get().Status.NextAttemptAt; next != "" {
		f.clock.setTo(next)
	}
}

// drive reconciles n times, moving the clock to each nextAttemptAt first, and returns
// the RequeueAfter of every reconcile.
func (f *depEnvFixture) drive(n int) []time.Duration {
	GinkgoHelper()
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		f.toNextAttempt()
		out = append(out, f.reconcile().RequeueAfter)
	}
	return out
}

// stall fails the import with a 412 until the deployment is Stalled.
func (f *depEnvFixture) stall() {
	GinkgoHelper()
	f.arm.on(depEnvImport, depEnvFail(http.StatusPreconditionFailed, "PreconditionFailed"))
	f.drive(5)
	Expect(f.get().Status.Phase).To(Equal(phaseStalled))
	Expect(f.get().Status.ConsecutiveFailures).To(Equal(int32(5)))
}

// desiredHash is the hash the controller should compute for the current spec and doc.
func (f *depEnvFixture) desiredHash(doc string) string {
	GinkgoHelper()
	deployment := f.get()
	openAPIHash := ""
	if doc != "" {
		openAPIHash = sha256Hex([]byte(doc))
	}
	hash, err := buildDesiredAPIMStateHash(&deployment.Spec, depEnvSubscription, depEnvResourceGroup, openAPIHash)
	Expect(err).NotTo(HaveOccurred())
	return hash
}

// rel is the service-relative path of the deployment's API plus suffix.
func (f *depEnvFixture) rel(suffix string) string {
	return "/apis/" + f.apiID + suffix
}

// at renders the fake clock's start plus d as status.nextAttemptAt would.
func depEnvAt(d time.Duration) string {
	return depEnvStart.Add(d).Format(time.RFC3339)
}
