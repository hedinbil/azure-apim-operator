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
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

// This file holds the machinery of the soak and chaos specs in soak_chaos_test.go:
//
//   - soakARM, a fake Azure Resource Manager for many APIM services at once. Every object
//     under test gets its own APIM service, so the service name in a request path says
//     which object the request belongs to. It fails a configurable share of requests
//     with transient or permanent errors, adds small random delays, and checks that no
//     object ever has two requests in flight or one outside a reconcile.
//   - soakDriver, a stand-in for controller-runtime's workqueue with a virtual clock. It
//     keeps client-go's semantics (one waiting timer per key that keeps the earliest time,
//     an object added while it is processed is processed again right after, never two
//     workers on one key) and runs four workers per kind, like MaxConcurrentReconciles 4
//     on each controller. Time only moves when every worker is idle, so a whole backoff
//     sequence (1, 2, 4, 8 minutes) costs nothing.
//   - an oracle: a model of the agreed retry design, checked after every reconcile
//     against what the fake ARM saw and what the controller wrote to the status.
//
// Every random choice is a hash of the seed, the object's logical name and how many
// times it was reconciled, never of the order goroutines happen to run in, so a seed
// replays the same story for every object.

// soakKind is a kind of resource that writes to APIM.
type soakKind string

const (
	soakDeployment soakKind = "APIMAPIDeployment"
	soakProduct    soakKind = "APIMProduct"
	soakTag        soakKind = "APIMTag"
	soakPolicy     soakKind = "APIMInboundPolicy"
)

// soakKinds lists the kinds in a fixed order.
var soakKinds = []soakKind{soakDeployment, soakProduct, soakTag, soakPolicy}

// soakSuccessPhase is the phase a kind reaches when its APIM write succeeded.
func soakSuccessPhase(kind soakKind) string {
	if kind == soakDeployment {
		return apimDeploymentPhaseSucceeded
	}
	return phaseCreated
}

// soakMaxAttempts is the retry policy's MaxAttempts in every soak.
const soakMaxAttempts = 5

// soakHash mixes the parts into a well-spread 64-bit number.
func soakHash(parts ...any) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%v\x00", p)
	}
	x := h.Sum64() + 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// soakChance is a number in [0, 1) derived from the parts.
func soakChance(parts ...any) float64 {
	return float64(soakHash(parts...)>>11) / float64(1<<53)
}

// soakOutcome is what one ARM answer means for the write it belongs to.
type soakOutcome int

const (
	soakOK soakOutcome = iota
	soakTransient
	soakPermanent
)

func (o soakOutcome) String() string {
	switch o {
	case soakOK:
		return "ok"
	case soakTransient:
		return "transient"
	default:
		return "permanent"
	}
}

// soakFaultMode is how the fake ARM misbehaves.
type soakFaultMode int

const (
	// soakModeHTTP answers with status and body.
	soakModeHTTP soakFaultMode = iota
	// soakModeHangUp closes the connection without answering (a transport error).
	soakModeHangUp
	// soakModeAcceptedTimeout accepts an import with 202 and never finishes it.
	soakModeAcceptedTimeout
	// soakModeAcceptedFailed accepts an import with 202; the operation then fails with
	// InternalServerError "DeadOperationMonitor", as seen in Sep 2026.
	soakModeAcceptedFailed
	// soakModeGarbage200 answers 200 with a body that is not JSON.
	soakModeGarbage200
)

// soakFault is one way a request can fail. expected is how the agreed design classifies
// it, written down here independently of classifyAPIMError; 404 depends on the request
// (a read, a delete, a write under another resource, or a resource's own path) and is
// decided in soakExpectedOutcome.
type soakFault struct {
	name     string
	mode     soakFaultMode
	status   int
	body     string
	expected soakOutcome
}

func soakARMError(code, message string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
}

var (
	// soakTransientHTTP are answers that must lead to Backoff and, five in a row, Stalled.
	soakTransientHTTP = []soakFault{
		{name: "409 Conflict", status: http.StatusConflict, body: soakARMError("Conflict", "operation in progress"), expected: soakTransient},
		{name: "412 PreconditionFailed", status: http.StatusPreconditionFailed, body: soakARMError("PreconditionFailed", "etag mismatch"), expected: soakTransient},
		{name: "422 ManagementApiRequestFailed", status: http.StatusUnprocessableEntity, body: soakARMError("ManagementApiRequestFailed", "Management API timed out"), expected: soakTransient},
		{name: "429 TooManyRequests", status: http.StatusTooManyRequests, body: soakARMError("TooManyRequests", "throttled"), expected: soakTransient},
		{name: "500 InternalServerError", status: http.StatusInternalServerError, body: soakARMError("InternalServerError", "boom"), expected: soakTransient},
		{name: "502 html", status: http.StatusBadGateway, body: "<html><body>502 Bad Gateway</body></html>", expected: soakTransient},
		{name: "503 ServiceUnavailable", status: http.StatusServiceUnavailable, body: soakARMError("ServiceUnavailable", "busy"), expected: soakTransient},
		{name: "504 GatewayTimeout", status: http.StatusGatewayTimeout, body: soakARMError("GatewayTimeout", "slow"), expected: soakTransient},
		// An Azure code that means "busy" wins over a 400.
		{name: "400 code InternalServerError", status: http.StatusBadRequest, body: soakARMError("InternalServerError", "transient despite 400"), expected: soakTransient},
		{name: "400 detail PreconditionFailed", status: http.StatusBadRequest,
			body:     `{"error":{"code":"BadRequest","message":"bad","details":[{"code":"PreconditionFailed","message":"etag"}]}}`,
			expected: soakTransient},
	}
	// soakPermanentHTTP are answers that must lead to Invalid at once.
	soakPermanentHTTP = []soakFault{
		{name: "400 ValidationError", status: http.StatusBadRequest, body: soakARMError("ValidationError", "One or more fields contain incorrect values"), expected: soakPermanent},
		{name: "401 InvalidAuthenticationToken", status: http.StatusUnauthorized, body: soakARMError("InvalidAuthenticationToken", "expired"), expected: soakPermanent},
		{name: "403 AuthorizationFailed", status: http.StatusForbidden, body: soakARMError("AuthorizationFailed", "no access"), expected: soakPermanent},
	}
	soakNotFound        = soakFault{name: "404 ResourceNotFound", status: http.StatusNotFound, body: soakARMError("ResourceNotFound", "not found")}
	soakHangUp          = soakFault{name: "connection closed", mode: soakModeHangUp, expected: soakTransient}
	soakAcceptedTimeout = soakFault{name: "202 never finishes", mode: soakModeAcceptedTimeout, expected: soakTransient}
	soakAcceptedFailed  = soakFault{name: "202 then DeadOperationMonitor", mode: soakModeAcceptedFailed, expected: soakTransient}
	soakGarbage200      = soakFault{name: "200 not JSON", mode: soakModeGarbage200, expected: soakTransient}
	soakUnavailable     = soakTransientHTTP[6]
	soakValidationError = soakPermanentHTTP[0]
)

// ARM steps as the fake names them.
const (
	soakStepGetAPI         = "get API"
	soakStepImport         = "import"
	soakStepServiceURL     = "patch serviceUrl"
	soakStepSubscription   = "patch subscriptionRequired"
	soakStepAssignProduct  = "assign product"
	soakStepAssignTag      = "assign tag"
	soakStepServiceDetails = "service details"
	soakStepProductUpsert  = "product upsert"
	soakStepProductDelete  = "product delete"
	soakStepTagUpsert      = "tag upsert"
	soakStepPolicyUpsert   = "policy upsert"
	soakStepPoll           = "poll"
	soakStepUnknown        = "unknown"
)

// soakIsWrite reports whether a step changes APIM.
func soakIsWrite(step string) bool {
	switch step {
	case soakStepGetAPI, soakStepServiceDetails, soakStepPoll, soakStepUnknown:
		return false
	}
	return true
}

// soakFaultsFor lists the transient and permanent faults that can hit step.
func soakFaultsFor(step string) (transient, permanent []soakFault) {
	transient = append([]soakFault(nil), soakTransientHTTP...)
	permanent = append([]soakFault(nil), soakPermanentHTTP...)
	switch step {
	case soakStepGetAPI:
		// Whatever happens here, the import goes ahead with If-Match: *.
		transient = append(transient, soakGarbage200)
		permanent = append(permanent, soakNotFound)
	case soakStepServiceDetails:
		// A read that finds nothing may be early: 404 on a GET is transient.
		transient = append(transient, soakGarbage200, soakNotFound)
	case soakStepImport:
		transient = append(transient, soakHangUp, soakAcceptedTimeout, soakAcceptedFailed)
		permanent = append(permanent, soakNotFound)
	case soakStepProductDelete:
		// 404 on a delete means already gone: a success.
		transient = append(transient, soakHangUp)
		permanent = append(permanent, soakNotFound)
	case soakStepServiceURL, soakStepSubscription, soakStepAssignProduct, soakStepAssignTag, soakStepPolicyUpsert:
		// These write under a resource another custom resource creates (the API, the
		// product, the tag); a 404 means it is not in APIM yet and is retried.
		transient = append(transient, soakHangUp, soakNotFound)
	default:
		transient = append(transient, soakHangUp)
		permanent = append(permanent, soakNotFound)
	}
	return transient, permanent
}

// soakExpectedOutcome is what the agreed design makes of fault on step.
func soakExpectedOutcome(step string, fault *soakFault) soakOutcome {
	if fault == nil || step == soakStepGetAPI {
		// ifMatchForUpsert falls back to If-Match: * on any error.
		return soakOK
	}
	if fault.status == http.StatusNotFound && fault.mode == soakModeHTTP {
		switch step {
		case soakStepProductDelete:
			return soakOK
		case soakStepServiceDetails:
			return soakTransient
		case soakStepServiceURL, soakStepSubscription, soakStepAssignProduct, soakStepAssignTag, soakStepPolicyUpsert:
			// The API, product or tag this write hangs off is not in APIM yet.
			return soakTransient
		default:
			return soakPermanent
		}
	}
	return fault.expected
}

// soakPlan describes one soak run.
type soakPlan struct {
	name string
	seed uint64

	deployments, products, tags, policies int

	// failRate is the share of requests that fail; permanentShare the share of those
	// failures that are permanent.
	failRate       float64
	permanentShare float64
	// asyncShare is the share of successful imports answered 202 and then polled.
	asyncShare float64
	// force, when set, decides every request's fault instead of failRate (nil = success).
	force func(step string) *soakFault

	// jitter is the retry policy's jitter.
	jitter float64
	// batchWindow makes the clock jump this far past the earliest timer, so timers due
	// close together fire together and the four workers per kind stay busy. With jitter
	// every object's timers differ by seconds; without a window each would run alone.
	// Nothing ever fires before its time; an attempt is at most this late.
	batchWindow time.Duration
	// noiseRate is the chance that a reconcile is followed by a spurious event (a resync,
	// an unrelated annotation) for an object that is backing off, Stalled or Invalid.
	noiseRate float64
	// events plans spec changes, retry annotations and mid-flight spec changes.
	events bool
	// deleteProducts makes products use deletionPolicy Delete and deletes each one once
	// it has settled.
	deleteProducts bool
	// sharedReconcilers runs one reconciler per kind for all its objects, as the manager
	// does, with one jitter source shared by the workers. Jitter values then land on
	// objects in scheduling order, so only plans whose outcome cannot depend on timing
	// use it. Otherwise every object gets its own reconciler and jitter source and the
	// whole run is a function of the seed.
	sharedReconcilers bool
}

// soakObject is one resource under test and the oracle's model of it. The model fields
// are guarded by soakDriver.mu.
type soakObject struct {
	kind    soakKind
	logical string // run-independent name, e.g. "dep-07"; every random choice hashes it
	key     types.NamespacedName
	svc     string // the object's own APIM service

	apiID       string   // deployment and policy
	productIDs  []string // deployment
	tagIDs      []string // deployment
	productID   string
	tagID       string
	operationID string

	maxWritesPerAttempt int

	// Planned events.
	specChangeAfter   int // after this many attempts the spec changes (0 = never)
	midFlightAt       int // during this attempt the spec changes (0 = never)
	annotationsLeft   int // retry annotations left for a Stalled or Invalid object
	backoffAnnotation bool
	noiseLeft         int
	deleteWhenSettled bool

	// Model.
	reconciles        int
	attempts          int
	phase             string
	consecutive       int32
	next              time.Time
	resetPending      bool
	specResetPending  bool
	midFlightArmed    bool
	midFlightFired    bool
	deleting          bool
	gone              bool
	specVersion       int
	annotationVersion int
	epoch             int
	attemptsInEpoch   int
	successesInEpoch  int
	runTimes          []time.Time // attempt times of the current run of failures
	runGaps           [][]time.Duration
	stalls            int
	rejections        int
	successes         int
	importPuts        int
	trace             []string
}

// soakObserved is the status a reconcile left behind.
type soakObserved struct {
	found     bool
	phase     string
	retry     apimv1.RetryStatus
	deleting  bool
	lastError string
	message   string
}

// soakServed is one request the fake ARM answered.
type soakServed struct {
	step    string
	method  string
	path    string
	fault   *soakFault
	outcome soakOutcome
}

// soakOp is an async operation the fake ARM started with a 202.
type soakOp struct {
	mode  soakFaultMode // soakModeHTTP means it succeeds
	polls int
}

// soakARM is the fake ARM shared by every object of one soak run.
type soakARM struct {
	plan   *soakPlan
	server *httptest.Server
	token  string

	mu         sync.Mutex
	objects    map[string]*soakObject // by APIM service name
	current    map[string]int         // service -> reconcile index in progress
	served     map[string][]soakServed
	inflight   map[string]int
	ops        map[string]*soakOp
	hooks      map[string]func()
	violations []string
	requests   int
	opCounter  int
}

func newSoakARM(plan *soakPlan, token string) *soakARM {
	a := &soakARM{
		plan:     plan,
		token:    token,
		objects:  map[string]*soakObject{},
		current:  map[string]int{},
		served:   map[string][]soakServed{},
		inflight: map[string]int{},
		ops:      map[string]*soakOp{},
		hooks:    map[string]func(){},
	}
	a.server = httptest.NewServer(a)
	return a
}

func (a *soakARM) register(o *soakObject) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.objects[o.svc] = o
}

func (a *soakARM) violate(format string, args ...any) {
	a.violations = append(a.violations, fmt.Sprintf(format, args...))
}

// begin marks a reconcile of the object with APIM service svc as running.
func (a *soakARM) begin(svc string, idx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.current[svc] = idx
	a.served[svc] = nil
}

// end marks it finished and returns what the fake served during it.
func (a *soakARM) end(svc string) []soakServed {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.current, svc)
	delete(a.hooks, svc)
	out := a.served[svc]
	delete(a.served, svc)
	return out
}

// setHook runs hook once, during the next write for svc.
func (a *soakARM) setHook(svc string, hook func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hooks[svc] = hook
}

func (a *soakARM) snapshotViolations() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.violations...)
}

// soakSplitPath returns the APIM service name and the path below it.
func soakSplitPath(path string) (svc, rest string, ok bool) {
	if strings.HasPrefix(path, "/asyncops/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/asyncops/"), "/", 2)
		if len(parts) != 2 {
			return "", "", false
		}
		return parts[0], path, true
	}
	const marker = "/providers/Microsoft.ApiManagement/service/"
	i := strings.Index(path, marker)
	if i < 0 {
		return "", "", false
	}
	tail := path[i+len(marker):]
	if j := strings.Index(tail, "/"); j >= 0 {
		return tail[:j], tail[j:], true
	}
	return tail, "", true
}

// soakRoutes maps a request's shape (method, path below the service with every id
// replaced by "*", and a marker for what tells two PUTs or PATCHes apart) to its step.
var soakRoutes = map[string]string{
	"GET ":                                    soakStepServiceDetails,
	"PUT products/*":                          soakStepProductUpsert,
	"DELETE products/*":                       soakStepProductDelete,
	"PUT products/*/apis/*":                   soakStepAssignProduct,
	"PUT tags/*":                              soakStepTagUpsert,
	"GET apis/*":                              soakStepGetAPI,
	"PUT apis/* import":                       soakStepImport,
	"PATCH apis/* serviceUrl":                 soakStepServiceURL,
	"PATCH apis/* subscriptionRequired":       soakStepSubscription,
	"PUT apis/*/tags/*":                       soakStepAssignTag,
	"PUT apis/*/policies/policy":              soakStepPolicyUpsert,
	"PUT apis/*/operations/*/policies/policy": soakStepPolicyUpsert,
}

// soakClassify names the step of a request.
func soakClassify(method, rest string, body []byte) string {
	if strings.HasPrefix(rest, "/asyncops/") {
		return soakStepPoll
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	for i := 1; i < len(parts); i += 2 {
		// Ids sit at odd positions: products/{id}/apis/{id}, apis/{id}/tags/{id}, ...
		if parts[i] != "policy" {
			parts[i] = "*"
		}
	}
	shape := method + " " + strings.Join(parts, "/")
	switch {
	case method == http.MethodPut && isImportEnvelope(body):
		shape += " import"
	case method == http.MethodPatch && strings.Contains(string(body), "serviceUrl"):
		shape += " serviceUrl"
	case method == http.MethodPatch && strings.Contains(string(body), "subscriptionRequired"):
		shape += " subscriptionRequired"
	}
	if step, ok := soakRoutes[shape]; ok {
		return step
	}
	return soakStepUnknown
}

// decide picks the fault for one request, or nil for success. It depends only on the
// seed, the object, the reconcile index and the request, never on timing.
func (a *soakARM) decide(o *soakObject, idx int, step, method, rest string) *soakFault {
	if a.plan.force != nil {
		return a.plan.force(step)
	}
	if soakChance(a.plan.seed, o.logical, idx, method, rest, "fail") >= a.plan.failRate {
		return nil
	}
	transient, permanent := soakFaultsFor(step)
	pick := soakHash(a.plan.seed, o.logical, idx, method, rest, "which")
	if soakChance(a.plan.seed, o.logical, idx, method, rest, "permanent") < a.plan.permanentShare {
		f := permanent[pick%uint64(len(permanent))]
		return &f
	}
	f := transient[pick%uint64(len(transient))]
	return &f
}

// soakRequest is one ARM request as the fake decided to answer it.
type soakRequest struct {
	o     *soakObject
	svc   string
	idx   int
	step  string
	rest  string
	fault *soakFault
	// op is the async operation a poll asks about; opURL is set when an import is
	// answered with 202 and starts one.
	op    *soakOp
	polls int
	opURL string
	hook  func()
	delay time.Duration
}

// ServeHTTP answers one ARM request.
func (a *soakARM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req, ok := a.admit(r, body)
	if !ok {
		writeARMError(w, http.StatusTeapot, "NotRoutedBySoak", r.URL.Path)
		return
	}

	time.Sleep(req.delay)
	if req.hook != nil {
		req.hook()
	}

	// Leave the in-flight count before answering: the client cannot send its next
	// request before it has this answer.
	a.mu.Lock()
	a.inflight[req.svc]--
	a.mu.Unlock()

	a.respond(w, req)
}

// admit attributes a request to its object, checks the invariants that concern a single
// request, and decides how to answer it.
func (a *soakARM) admit(r *http.Request, body []byte) (*soakRequest, bool) {
	svc, rest, ok := soakSplitPath(r.URL.Path)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests++
	o := a.objects[svc]
	if !ok || o == nil {
		a.violate("request the soak cannot attribute: %s %s", r.Method, r.URL.Path)
		return nil, false
	}
	a.inflight[svc]++
	if n := a.inflight[svc]; n > 1 {
		a.violate("%s %s: %d requests in flight at once (%s %s)", o.kind, o.logical, n, r.Method, r.URL.Path)
	}
	idx, inReconcile := a.current[svc]
	if !inReconcile {
		a.violate("%s %s: ARM request outside any reconcile: %s %s", o.kind, o.logical, r.Method, r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+a.token {
		a.violate("%s %s: Authorization %q", o.kind, o.logical, got)
	}
	req := &soakRequest{o: o, svc: svc, idx: idx, rest: rest, step: soakClassify(r.Method, rest, body)}
	req.delay = time.Duration(soakHash(a.plan.seed, o.logical, idx, r.Method, rest, "delay")%3) * time.Millisecond

	switch req.step {
	case soakStepUnknown:
		a.violate("%s %s: unclassified request %s %s", o.kind, o.logical, r.Method, r.URL.Path)
	case soakStepPoll:
		if req.op = a.ops[rest]; req.op == nil {
			a.violate("%s %s: poll of an unknown operation %s", o.kind, o.logical, rest)
		} else {
			req.op.polls++
			req.polls = req.op.polls
		}
		return req, true
	}

	req.fault = a.decide(o, idx, req.step, r.Method, rest)
	if req.fault != nil && req.step != soakStepImport &&
		(req.fault.mode == soakModeAcceptedTimeout || req.fault.mode == soakModeAcceptedFailed) {
		req.fault = nil
	}
	a.served[svc] = append(a.served[svc], soakServed{
		step: req.step, method: r.Method, path: rest, fault: req.fault, outcome: soakExpectedOutcome(req.step, req.fault),
	})
	if soakIsWrite(req.step) && a.hooks[svc] != nil {
		req.hook = a.hooks[svc]
		delete(a.hooks, svc)
	}
	if req.step == soakStepImport {
		a.startOpLocked(req)
	}
	return req, true
}

// startOpLocked decides whether an import is answered 202, and how its operation ends.
func (a *soakARM) startOpLocked(req *soakRequest) {
	mode := soakFaultMode(-1)
	switch {
	case req.fault != nil && (req.fault.mode == soakModeAcceptedTimeout || req.fault.mode == soakModeAcceptedFailed):
		mode = req.fault.mode
	case req.fault == nil && soakChance(a.plan.seed, req.o.logical, req.idx, "async") < a.plan.asyncShare:
		mode = soakModeHTTP
	}
	if mode < 0 {
		return
	}
	a.opCounter++
	req.opURL = fmt.Sprintf("/asyncops/%s/op-%d", req.svc, a.opCounter)
	a.ops[req.opURL] = &soakOp{mode: mode}
}

// respond writes the answer admit decided on.
func (a *soakARM) respond(w http.ResponseWriter, req *soakRequest) {
	switch {
	case req.step == soakStepPoll:
		soakRespondPoll(w, req)
	case req.opURL != "":
		w.Header().Set("Azure-AsyncOperation", req.opURL+"?api-version=2021-08-01")
		w.WriteHeader(http.StatusAccepted)
	case req.fault != nil:
		soakRespondFault(w, req.fault)
	default:
		a.succeed(w, req.o, req.idx, req.step, req.rest)
	}
}

// soakRespondPoll answers a poll of an async operation: one that never finishes stays
// InProgress, one that fails dies like the Sep 2026 imports did, and one that succeeds
// is InProgress once and then Succeeded.
func soakRespondPoll(w http.ResponseWriter, req *soakRequest) {
	switch {
	case req.op == nil:
		writeARMError(w, http.StatusNotFound, "NotFound", "unknown operation")
	case req.op.mode == soakModeAcceptedTimeout:
		writeARMJSON(w, http.StatusOK, `{"status":"InProgress"}`)
	case req.op.mode == soakModeAcceptedFailed:
		writeARMJSON(w, http.StatusOK, `{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor"}}`)
	case req.polls == 1:
		writeARMJSON(w, http.StatusOK, `{"status":"InProgress"}`)
	default:
		writeARMJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
	}
}

// soakRespondFault writes a failure.
func soakRespondFault(w http.ResponseWriter, fault *soakFault) {
	switch fault.mode {
	case soakModeHangUp:
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		writeARMError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "could not hang up")
	case soakModeGarbage200:
		writeARMJSON(w, http.StatusOK, "<html>not json</html>")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fault.status)
		_, _ = io.WriteString(w, fault.body)
	}
}

// succeed writes the answer of a successful request.
func (a *soakARM) succeed(w http.ResponseWriter, o *soakObject, idx int, step, rest string) {
	switch step {
	case soakStepGetAPI:
		if soakChance(a.plan.seed, o.logical, idx, "exists") < 0.5 {
			writeARMError(w, http.StatusNotFound, "ResourceNotFound", "API not found")
			return
		}
		w.Header().Set("ETag", `W/"etag-`+o.logical+`"`)
		writeARMJSON(w, http.StatusOK, `{"name":"`+o.apiID+`"}`)
	case soakStepImport:
		writeARMJSON(w, http.StatusCreated, `{"name":"`+o.apiID+`"}`)
	case soakStepServiceDetails:
		writeARMJSON(w, http.StatusOK, `{"properties":{"hostnameConfigurations":[`+
			`{"type":"Proxy","hostName":"`+o.svc+`.azure-api.net"},{"type":"DeveloperPortal","hostName":"`+o.svc+`.developer.azure-api.net"}]}}`)
	default:
		_ = rest
		writeARMJSON(w, http.StatusOK, `{}`)
	}
}

// soakEvent is something that happens to an object between or during reconciles.
type soakEvent int

const (
	soakEventNoise soakEvent = iota
	soakEventSpecChange
	soakEventMidFlightSpecChange
	soakEventRetryAnnotation
	soakEventDelete
)

func (e soakEvent) String() string {
	return [...]string{"noise", "spec change", "mid-flight spec change", "retry annotation", "delete"}[e]
}

// soakDriver runs reconciles like controller-runtime's workqueue would, on a virtual clock.
type soakDriver struct {
	plan    *soakPlan
	arm     *soakARM
	workers int

	reconcile func(ctx context.Context, o *soakObject) (ctrl.Result, error)
	observe   func(ctx context.Context, o *soakObject) (soakObserved, error)
	// mutate makes ev happen in the cluster; version numbers the change per object.
	mutate func(ctx context.Context, o *soakObject, ev soakEvent, version int) error

	clock atomic.Int64 // virtual time, UnixNano

	mu             sync.Mutex
	cond           *sync.Cond
	queues         map[soakKind][]*soakObject
	queued         map[*soakObject]bool
	processing     map[*soakObject]bool
	dirty          map[*soakObject]bool
	waiting        map[*soakObject]time.Time
	inflight       map[soakKind]int
	inflightTotal  int
	peak           map[soakKind]int
	busySum        int // sum over reconciles of how many ran at the time, for the mean
	stopped        bool
	reconcileLimit int
	reconcileCount int
	violations     []string
	reconcileErrs  int
	panics         int
	objects        []*soakObject
}

func newSoakDriver(plan *soakPlan, arm *soakARM, start time.Time, workers int) *soakDriver {
	d := &soakDriver{
		plan:       plan,
		arm:        arm,
		workers:    workers,
		queues:     map[soakKind][]*soakObject{},
		queued:     map[*soakObject]bool{},
		processing: map[*soakObject]bool{},
		dirty:      map[*soakObject]bool{},
		waiting:    map[*soakObject]time.Time{},
		inflight:   map[soakKind]int{},
		peak:       map[soakKind]int{},
	}
	d.cond = sync.NewCond(&d.mu)
	d.clock.Store(start.UnixNano())
	return d
}

// now is the virtual clock the retry policy reads.
func (d *soakDriver) now() time.Time {
	return time.Unix(0, d.clock.Load()).UTC()
}

// soakJitterSource is a retry policy's jitter source: seeded, safe for concurrent
// callers, and discrete (-20 %, 0 or +20 %) so that timers of different objects still
// coincide and the workers stay busy. Per object (parts include its logical name) it
// makes every timeline independent of scheduling.
func soakJitterSource(parts ...any) func() float64 {
	var n atomic.Uint64
	values := [...]float64{0, 0.5, 0.9999}
	return func() float64 {
		return values[soakHash(append(parts, n.Add(1))...)%uint64(len(values))]
	}
}

func (d *soakDriver) violate(o *soakObject, idx int, format string, args ...any) {
	prefix := ""
	if o != nil {
		prefix = fmt.Sprintf("%s %s reconcile #%d: ", o.kind, o.logical, idx)
	}
	d.violations = append(d.violations, prefix+fmt.Sprintf(format, args...))
}

// enqueueLocked is a watch event: queue o now, or once more right after the reconcile in
// progress.
func (d *soakDriver) enqueueLocked(o *soakObject) {
	if d.processing[o] {
		d.dirty[o] = true
		return
	}
	if d.queued[o] {
		return
	}
	d.queued[o] = true
	d.queues[o.kind] = append(d.queues[o.kind], o)
}

// addAfterLocked is AddAfter: one waiting timer per object, keeping the earliest time.
func (d *soakDriver) addAfterLocked(o *soakObject, after time.Duration) {
	due := d.now().Add(after)
	if existing, ok := d.waiting[o]; ok && !due.Before(existing) {
		return
	}
	d.waiting[o] = due
}

func (d *soakDriver) queuesEmptyLocked() bool {
	for _, q := range d.queues {
		if len(q) > 0 {
			return false
		}
	}
	return true
}

// advanceLocked moves the clock to the earliest timer (plus the batch window) and
// queues what is due.
func (d *soakDriver) advanceLocked() {
	var earliest time.Time
	for _, due := range d.waiting {
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	if target := earliest.Add(d.plan.batchWindow); target.After(d.now()) {
		d.clock.Store(target.UnixNano())
	}
	due := make([]*soakObject, 0)
	for o, at := range d.waiting {
		if !at.After(d.now()) {
			due = append(due, o)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].logical < due[j].logical })
	for _, o := range due {
		delete(d.waiting, o)
		d.enqueueLocked(o)
	}
}

// next blocks until an object of kind is ready, and returns nil once nothing is left.
func (d *soakDriver) next(kind soakKind) *soakObject {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if d.stopped {
			return nil
		}
		if q := d.queues[kind]; len(q) > 0 {
			o := q[0]
			d.queues[kind] = q[1:]
			delete(d.queued, o)
			d.processing[o] = true
			d.inflight[kind]++
			d.inflightTotal++
			if d.inflight[kind] > d.peak[kind] {
				d.peak[kind] = d.inflight[kind]
			}
			d.busySum += d.inflightTotal
			return o
		}
		if d.inflightTotal == 0 && d.queuesEmptyLocked() {
			if len(d.waiting) == 0 {
				d.stopped = true
				d.cond.Broadcast()
				return nil
			}
			d.advanceLocked()
			d.cond.Broadcast()
			continue
		}
		d.cond.Wait()
	}
}

// run starts every object and blocks until the queues and timers are drained.
func (d *soakDriver) run(ctx context.Context) {
	d.mu.Lock()
	d.reconcileLimit = 60 * len(d.objects)
	for _, o := range d.objects {
		d.enqueueLocked(o)
	}
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, kind := range soakKinds {
		for i := 0; i < d.workers; i++ {
			wg.Add(1)
			go func(kind soakKind) {
				defer wg.Done()
				for {
					o := d.next(kind)
					if o == nil {
						return
					}
					d.process(ctx, o)
				}
			}(kind)
		}
	}
	wg.Wait()
}

// call runs one reconcile and turns a panic into a violation.
func (d *soakDriver) call(ctx context.Context, o *soakObject) (res ctrl.Result, panicked any, err error) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
		}
	}()
	res, err = d.reconcile(ctx, o)
	return res, nil, err
}

// process reconciles o once, checks the result against the model and schedules what
// controller-runtime would schedule.
func (d *soakDriver) process(ctx context.Context, o *soakObject) {
	d.mu.Lock()
	idx := o.reconciles
	o.reconciles++
	now := d.now()
	expectWrite := o.expectWrite(now)
	if d.plan.events && expectWrite && o.midFlightAt > 0 && o.attempts+1 == o.midFlightAt {
		o.midFlightAt = 0
		o.midFlightArmed = true
		d.arm.setHook(o.svc, func() { d.apply(ctx, o, soakEventMidFlightSpecChange) })
	}
	d.mu.Unlock()

	d.arm.begin(o.svc, idx)
	res, panicked, err := d.call(ctx, o)
	served := d.arm.end(o.svc)
	observed, obsErr := d.observe(ctx, o)

	d.mu.Lock()
	d.reconcileCount++
	if d.reconcileCount > d.reconcileLimit {
		d.violate(nil, 0, "more than %d reconciles: a requeue loop", d.reconcileLimit)
		d.stopped = true
	}
	if panicked != nil {
		d.panics++
		d.violate(o, idx, "panic: %v", panicked)
	}
	if err != nil {
		d.reconcileErrs++
		d.violate(o, idx, "Reconcile returned an error (an APIM failure must come back as a result): %v", err)
	}
	if obsErr != nil {
		d.violate(o, idx, "reading the status: %v", obsErr)
	} else {
		d.check(o, idx, now, expectWrite, served, res, observed)
	}
	events := d.pickEvents(o, idx, served)
	d.mu.Unlock()

	for _, ev := range events {
		d.apply(ctx, o, ev)
	}

	d.mu.Lock()
	delete(d.processing, o)
	d.inflight[o.kind]--
	d.inflightTotal--
	switch {
	case err != nil:
		d.addAfterLocked(o, time.Second)
	case res.RequeueAfter > 0:
		d.addAfterLocked(o, res.RequeueAfter)
	}
	if d.dirty[o] {
		delete(d.dirty, o)
		d.enqueueLocked(o)
	}
	d.cond.Broadcast()
	d.mu.Unlock()
}

// apply makes an event happen in the cluster and queues the object, as its watch would.
func (d *soakDriver) apply(ctx context.Context, o *soakObject, ev soakEvent) {
	var err error
	if ev != soakEventNoise {
		d.mu.Lock()
		version := 0
		switch ev {
		case soakEventSpecChange, soakEventMidFlightSpecChange:
			o.specVersion++
			version = o.specVersion
		case soakEventRetryAnnotation:
			o.annotationVersion++
			version = o.annotationVersion
		}
		d.mu.Unlock()
		err = d.mutate(ctx, o, ev, version)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.violate(o, o.reconciles-1, "%s: %v", ev, err)
		return
	}
	o.trace = append(o.trace, "event:"+ev.String())
	switch ev {
	case soakEventMidFlightSpecChange:
		// Takes effect for the model once the attempt in progress is accounted for.
		o.midFlightFired = true
	case soakEventSpecChange, soakEventDelete:
		o.resetPending = true
		o.specResetPending = true
		o.startEpochLocked()
	case soakEventRetryAnnotation:
		o.resetPending = true
		o.startEpochLocked()
	}
	if ev == soakEventDelete {
		o.deleting = true
	}
	d.enqueueLocked(o)
	d.cond.Broadcast()
}

func (o *soakObject) startEpochLocked() {
	o.epoch++
	o.attemptsInEpoch = 0
	o.successesInEpoch = 0
}

// expectWrite is the oracle's answer to "may this reconcile write to APIM?".
func (o *soakObject) expectWrite(now time.Time) bool {
	if o.gone {
		return false
	}
	if o.resetPending {
		// A retry annotation on a deployment that is already in sync changes nothing:
		// the in-sync check comes before the gate.
		if o.kind == soakDeployment && o.phase == apimDeploymentPhaseSucceeded && !o.specResetPending {
			return false
		}
		return true
	}
	switch o.phase {
	case "":
		return true
	case soakSuccessPhase(o.kind):
		// A deployment whose applied hash matches stays put; the other kinds upsert again.
		return o.kind != soakDeployment
	case phaseBackoff:
		return !now.Before(o.next)
	default: // Stalled, Invalid
		return false
	}
}

// soakBaseDelay is the un-jittered wait after the n-th consecutive failure.
func soakBaseDelay(n int32) time.Duration {
	d := time.Minute
	for i := int32(1); i < n && d < 30*time.Minute; i++ {
		d *= 2
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

// check compares one reconcile with the model and moves the model on.
func (d *soakDriver) check(o *soakObject, idx int, now time.Time, expectWrite bool, served []soakServed, res ctrl.Result, obs soakObserved) {
	wrote := false
	for _, s := range served {
		wrote = wrote || soakIsWrite(s.step)
	}
	if wrote != expectWrite {
		d.violate(o, idx, "wrote to APIM = %v, the design says %v (model phase %q, consecutive %d, next %s, now %s, resetPending %v, steps %v)",
			wrote, expectWrite, o.phase, o.consecutive, o.next.Format(time.RFC3339), now.Format(time.RFC3339), o.resetPending, soakSteps(served))
	}
	if wrote {
		d.checkAttempt(o, idx, now, served, res, obs)
	} else {
		d.checkHeld(o, idx, now, served, res, obs)
	}
	if o.midFlightFired {
		// The spec changed while this reconcile was writing: the attempt counted for the
		// old spec, the next one starts over.
		o.midFlightFired = false
		o.resetPending = true
		o.specResetPending = true
		o.startEpochLocked()
	}
}

// checkHeld checks a reconcile that did not write: backing off, Stalled, Invalid, gone,
// or a deployment already in sync. Nothing about the retry state may change.
func (d *soakDriver) checkHeld(o *soakObject, idx int, now time.Time, served []soakServed, res ctrl.Result, obs soakObserved) {
	if len(served) > 0 {
		d.violate(o, idx, "read APIM without writing: %v", soakSteps(served))
	}
	if o.resetPending && o.kind == soakDeployment && o.phase == apimDeploymentPhaseSucceeded && !o.specResetPending {
		o.resetPending = false
	}
	o.trace = append(o.trace, fmt.Sprintf("%d:held:%s:%d", idx, o.phase, o.consecutive))
	if o.gone {
		if obs.found {
			d.violate(o, idx, "still exists after its product was removed")
		}
		if !res.IsZero() {
			d.violate(o, idx, "a gone object requeued: %+v", res)
		}
		return
	}
	if o.phase != "" {
		d.expectStatus(o, idx, obs)
	}
	switch {
	case o.phase == phaseBackoff:
		if want := o.next.Sub(now); res.RequeueAfter != want || want <= 0 {
			d.violate(o, idx, "skipped while backing off: RequeueAfter %s, want the remaining %s", res.RequeueAfter, want)
		}
	case !res.IsZero():
		d.violate(o, idx, "a %q object that was not written requeued: %+v", o.phase, res)
	}
}

// checkAttempt checks a reconcile that wrote to APIM and applies its outcome to the model.
func (d *soakDriver) checkAttempt(o *soakObject, idx int, now time.Time, served []soakServed, res ctrl.Result, obs soakObserved) {
	if o.resetPending {
		o.consecutive = 0
		o.runTimes = nil
		o.resetPending = false
		o.specResetPending = false
	}
	o.attempts++
	o.attemptsInEpoch++
	d.checkWrites(o, idx, served)

	outcome := soakOK
	for i, s := range served {
		if s.outcome == soakOK {
			continue
		}
		outcome = s.outcome
		if i != len(served)-1 {
			d.violate(o, idx, "kept calling APIM after %s failed (%s): %v", s.step, s.fault.name, soakSteps(served))
		}
		break
	}
	o.applyOutcome(outcome, now)

	if o.consecutive > soakMaxAttempts {
		d.violate(o, idx, "%d consecutive failed attempts, the cap is %d", o.consecutive, soakMaxAttempts)
	}
	limit := soakMaxAttempts + o.successesInEpoch
	if o.kind == soakDeployment {
		limit = soakMaxAttempts
	}
	if o.attemptsInEpoch > limit {
		d.violate(o, idx, "%d attempts for one spec version (%d successes), at most %d allowed", o.attemptsInEpoch, o.successesInEpoch, limit)
	}
	o.trace = append(o.trace, fmt.Sprintf("%d:attempt:%s:%s:%d", idx, outcome, o.phase, o.consecutive))

	switch {
	case o.gone:
		if obs.found {
			d.violate(o, idx, "the product delete succeeded but the resource is still there")
		}
		if !res.IsZero() {
			d.violate(o, idx, "requeued after the product was removed: %+v", res)
		}
		return
	case o.phase == phaseBackoff:
		d.checkBackoff(o, idx, now, res, obs)
	case !res.IsZero():
		d.violate(o, idx, "phase %s must not requeue: %+v", o.phase, res)
	}
	d.expectStatus(o, idx, obs)
}

// checkWrites checks which writes one attempt sent.
func (d *soakDriver) checkWrites(o *soakObject, idx int, served []soakServed) {
	writes, imports := 0, 0
	for _, s := range served {
		if !soakIsWrite(s.step) {
			continue
		}
		writes++
		if s.step == soakStepImport {
			imports++
		}
		if o.deleting != (s.step == soakStepProductDelete) {
			d.violate(o, idx, "%s while deleting=%v", s.step, o.deleting)
		}
	}
	o.importPuts += imports
	if o.kind == soakDeployment && imports != 1 {
		d.violate(o, idx, "%d import PUTs in one attempt, want exactly 1 (steps %v)", imports, soakSteps(served))
	}
	if writes > o.maxWritesPerAttempt {
		d.violate(o, idx, "%d writes in one attempt, at most %d expected: %v", writes, o.maxWritesPerAttempt, soakSteps(served))
	}
}

// applyOutcome moves the model on after an attempt.
func (o *soakObject) applyOutcome(outcome soakOutcome, now time.Time) {
	o.runTimes = append(o.runTimes, now)
	switch outcome {
	case soakOK:
		o.consecutive = 0
		o.next = time.Time{}
		o.successesInEpoch++
		o.successes++
		o.runTimes = nil
		o.phase = soakSuccessPhase(o.kind)
		if o.deleting {
			o.gone = true
			o.phase = "gone"
		}
	case soakPermanent:
		o.consecutive++
		o.next = time.Time{}
		o.phase = phaseInvalid
		o.rejections++
		o.runTimes = nil
	case soakTransient:
		o.consecutive++
		o.phase = phaseBackoff
		if o.consecutive >= soakMaxAttempts {
			o.phase = phaseStalled
			o.next = time.Time{}
			o.stalls++
			gaps := make([]time.Duration, 0, len(o.runTimes)-1)
			for i := 1; i < len(o.runTimes); i++ {
				gaps = append(gaps, o.runTimes[i].Sub(o.runTimes[i-1]))
			}
			o.runGaps = append(o.runGaps, gaps)
			o.runTimes = nil
		}
	}
}

// checkBackoff checks the wait after a transient failure: nextAttemptAt within the
// jittered doubling delay, and a requeue for exactly that moment.
func (d *soakDriver) checkBackoff(o *soakObject, idx int, now time.Time, res ctrl.Result, obs soakObserved) {
	if obs.retry.NextAttemptAt == "" {
		d.violate(o, idx, "Backoff without nextAttemptAt")
		return
	}
	next, err := time.Parse(time.RFC3339, obs.retry.NextAttemptAt)
	if err != nil {
		d.violate(o, idx, "unreadable nextAttemptAt %q", obs.retry.NextAttemptAt)
		return
	}
	wait := next.Sub(now)
	base := soakBaseDelay(o.consecutive)
	low := time.Duration(float64(base) * (1 - d.plan.jitter))
	high := time.Duration(float64(base)*(1+d.plan.jitter)) + time.Second
	if wait < low || wait > high {
		d.violate(o, idx, "backoff after failure %d is %s, want %s..%s", o.consecutive, wait, low, high)
	}
	if res.RequeueAfter != wait {
		d.violate(o, idx, "RequeueAfter %s does not match nextAttemptAt (%s away)", res.RequeueAfter, wait)
	}
	o.next = next
}

// expectStatus compares the persisted retry status with the model.
func (d *soakDriver) expectStatus(o *soakObject, idx int, obs soakObserved) {
	if !obs.found {
		d.violate(o, idx, "the resource is gone, the model says %q", o.phase)
		return
	}
	if obs.phase != o.phase {
		d.violate(o, idx, "phase %q, the model says %q (message %q)", obs.phase, o.phase, obs.message)
	}
	if obs.retry.ConsecutiveFailures != o.consecutive {
		d.violate(o, idx, "consecutiveFailures %d, the model says %d", obs.retry.ConsecutiveFailures, o.consecutive)
	}
	if (o.phase == phaseBackoff) != (obs.retry.NextAttemptAt != "") {
		d.violate(o, idx, "phase %q with nextAttemptAt %q", o.phase, obs.retry.NextAttemptAt)
	}
	if o.kind == soakDeployment {
		failed := o.phase == phaseBackoff || o.phase == phaseStalled || o.phase == phaseInvalid
		if failed != (obs.lastError != "") {
			d.violate(o, idx, "phase %q with lastError %q", o.phase, obs.lastError)
		}
	}
}

// pickEvents decides what happens to o after reconcile idx. Every choice hashes the
// seed, the object and idx, so it does not depend on scheduling.
func (d *soakDriver) pickEvents(o *soakObject, idx int, served []soakServed) []soakEvent {
	if o.gone || d.stopped {
		return nil
	}
	wrote := false
	for _, s := range served {
		if soakIsWrite(s.step) {
			wrote = true
		}
	}
	seed := d.plan.seed
	var events []soakEvent
	if d.plan.events && !o.deleting {
		switch {
		case wrote && o.specChangeAfter > 0 && o.attempts == o.specChangeAfter:
			o.specChangeAfter = 0
			events = append(events, soakEventSpecChange)
		case (o.phase == phaseStalled || o.phase == phaseInvalid) && o.annotationsLeft > 0 &&
			soakChance(seed, o.logical, idx, "annotate") < 0.6:
			o.annotationsLeft--
			events = append(events, soakEventRetryAnnotation)
		case wrote && o.phase == phaseBackoff && o.backoffAnnotation:
			o.backoffAnnotation = false
			events = append(events, soakEventRetryAnnotation)
		}
	}
	if d.plan.deleteProducts && o.kind == soakProduct && o.deleteWhenSettled && !o.deleting &&
		(o.phase == phaseCreated || o.phase == phaseStalled || o.phase == phaseInvalid) {
		o.deleteWhenSettled = false
		events = append(events, soakEventDelete)
	}
	if len(events) == 0 && d.plan.noiseRate > 0 && o.noiseLeft > 0 &&
		(o.kind == soakDeployment || o.phase != soakSuccessPhase(o.kind)) &&
		soakChance(seed, o.logical, idx, "noise") < d.plan.noiseRate {
		o.noiseLeft--
		events = append(events, soakEventNoise)
	}
	return events
}

// soakSteps lists the steps of served requests with their faults.
func soakSteps(served []soakServed) []string {
	out := make([]string, 0, len(served))
	for _, s := range served {
		if s.fault != nil {
			out = append(out, s.step+"["+s.fault.name+"]")
		} else {
			out = append(out, s.step)
		}
	}
	return out
}
