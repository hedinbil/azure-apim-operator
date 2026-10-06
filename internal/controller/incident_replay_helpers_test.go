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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

// Helpers for incident_replay_test.go: a settable clock, a fake ARM that behaves like
// APIM did on 25-28 Sep 2026, a tee that reads back the operator's zap log lines, and a
// driver that plays controller-runtime's workqueue over 69 hours of fake time.

// incidentStart is when the Sep 2026 import loop began (25 Sep 10:00 UTC, rounded).
var incidentStart = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// incidentHorizon is how long the loop ran before someone stopped it: 69 hours.
const incidentHorizon = 69 * time.Hour

// What 0.30.0 (before the fix) did with every failed import: wait up to oldAsyncWait for
// the 202 to finish, then return ctrl.Result{RequeueAfter: oldRequeue}, nil whatever the
// error, with no cap. Used only for the analytic comparison, never to run old code.
const (
	oldAsyncWait = 3 * time.Minute
	oldRequeue   = 60 * time.Second
)

// oldImportPUTsFloor is the fewest import PUTs 0.30.0 could have sent in 69 hours: even
// if every attempt was the slowest kind (a 202 that runs into the full 3 minute wait,
// then the fixed 60 s requeue), it re-imports every 4 minutes, 69 h / 4 min = 1035
// times. Fast 412 and 422 answers only shorten the cycle and push the count up; the
// incident itself logged about 800 imports, slowed down by APIM's own response times.
const oldImportPUTsFloor = int(incidentHorizon / (oldAsyncWait + oldRequeue))

// oldImportPUTsForScript is what 0.30.0 would have sent against incidentImportARM in its
// Sep 2026 shape (an accepted import keeps running 10 minutes, every third PUT is a 422,
// a PUT during a running import is a 412), worked out by hand rather than by running old
// code:
//
//   - an accepted PUT (202) costs the 3 minute wait plus the 60 s requeue: 4 minutes;
//   - every other PUT fails at once and costs the 60 s requeue: 1 minute;
//   - the import keeps running for 10 minutes, so after a 202 at t the PUTs at t+4 ... t+9
//     all fail fast (412, or 422 on every third), and the one at t+10 starts the next
//     import, unless it is a third and gets a 422, which moves the next 202 to t+11.
//
// That is 7 PUTs per 10 minutes, or 8 per 11 when the 422 lands on the free slot:
// between 4140 min * 7/10 = 2898 and 4140 min * 8/11 = 3011 PUTs in 69 hours. Walking the
// windows through the mod-3 pattern gives 2956 exactly (395 x 202, 1576 x 412, 985 x 422).
const oldImportPUTsForScript = 2956

// incidentClock is a settable clock shared by the retry policy and the fake ARM, which
// moves it forward to stand in for the time the operator spends waiting on an import.
type incidentClock struct {
	mu sync.Mutex
	t  time.Time
}

func newIncidentClock() *incidentClock { return &incidentClock{t: incidentStart} }

func (c *incidentClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *incidentClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *incidentClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// incidentImportAnswer is one import PUT the fake ARM answered.
type incidentImportAnswer struct {
	at     time.Time
	status int
	bytes  int
}

// incidentImportARM is the Azure Management API of one APIM service during the incident,
// for one API. An import PUT is answered:
//
//   - 201 once recoverAfter imports have been answered (APIM has recovered), when set;
//   - 422 Management API request timed out on every third PUT, when every3rd422 is set;
//   - 412 PreconditionFailed while an import it accepted earlier is still running;
//   - otherwise 202 with an Azure-AsyncOperation URL; the import then runs opDuration of
//     fake time, and polls answer InProgress until it ends, then Failed with
//     InternalServerError / DeadOperationMonitor.
//
// The real wait in the apim package is shortened to milliseconds, so the fake moves the
// clock by what the wait would have cost in production: until the import ends, at most
// waitCost (apim.AsyncWaitTimeout before the test shortened it).
type incidentImportARM struct {
	server      *httptest.Server
	clock       *incidentClock
	servicePath string
	apiPath     string

	opDuration   time.Duration
	waitCost     time.Duration
	every3rd422  bool
	retryAfter   string
	recoverAfter int

	mu       sync.Mutex
	opEnd    time.Time
	answers  []incidentImportAnswer
	etagGets int
	polls    int
	others   []string
}

func newIncidentImportARM(clock *incidentClock, subscription, resourceGroup, service, apiID string) *incidentImportARM {
	f := &incidentImportARM{
		clock: clock,
		servicePath: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s",
			subscription, resourceGroup, service),
	}
	f.apiPath = f.servicePath + "/apis/" + apiID
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *incidentImportARM) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/incident-ops/"):
		f.polls++
		if f.clock.now().Before(f.opEnd) {
			f.setRetryAfter(w)
			writeIncidentJSON(w, http.StatusAccepted, `{"status":"InProgress"}`)
			return
		}
		writeIncidentJSON(w, http.StatusOK, `{"status":"Failed","error":{"code":"InternalServerError",`+
			`"message":"DeadOperationMonitor: the import operation stopped responding"}}`)

	case r.URL.Path == f.apiPath && r.Method == http.MethodGet:
		// The API exists from an earlier deploy, so every import is a conditional update.
		f.etagGets++
		w.Header().Set("ETag", `W/"incident-etag"`)
		writeIncidentJSON(w, http.StatusOK, `{"name":"api"}`)

	case r.URL.Path == f.apiPath && r.Method == http.MethodPut && r.URL.Query().Get("import") == "true":
		now := f.clock.now()
		k := len(f.answers) + 1
		answer := incidentImportAnswer{at: now, bytes: len(body)}
		switch {
		case f.recoverAfter > 0 && k > f.recoverAfter:
			answer.status = http.StatusCreated
			writeIncidentJSON(w, http.StatusCreated, `{"name":"api"}`)
		case f.every3rd422 && k%3 == 0:
			answer.status = http.StatusUnprocessableEntity
			writeIncidentJSON(w, http.StatusUnprocessableEntity, `{"error":{"code":"ManagementApiRequestFailed",`+
				`"message":"Management API request timed out.","details":[{"code":"Timeout","message":"Request timed out"}]}}`)
		case now.Before(f.opEnd):
			answer.status = http.StatusPreconditionFailed
			writeIncidentJSON(w, http.StatusPreconditionFailed, `{"error":{"code":"PreconditionFailed",`+
				`"message":"Resource was modified since last retrieval. Please retrieve latest version."}}`)
		default:
			answer.status = http.StatusAccepted
			f.opEnd = now.Add(f.opDuration)
			wait := f.waitCost
			if f.opDuration < wait {
				wait = f.opDuration
			}
			f.clock.advance(wait)
			w.Header().Set("Azure-AsyncOperation", fmt.Sprintf("/incident-ops/op-%d?api-version=2021-08-01", k))
			f.setRetryAfter(w)
			w.WriteHeader(http.StatusAccepted)
		}
		f.answers = append(f.answers, answer)

	case r.URL.Path == f.servicePath && r.Method == http.MethodGet:
		f.others = append(f.others, "service details")
		writeIncidentJSON(w, http.StatusOK, `{"properties":{"hostnameConfigurations":[`+
			`{"type":"Proxy","hostName":"gw.example.net"},{"type":"DeveloperPortal","hostName":"portal.example.net"}]}}`)

	case r.URL.Path == f.apiPath && r.Method == http.MethodPatch:
		f.others = append(f.others, "patch")
		writeIncidentJSON(w, http.StatusOK, `{}`)

	default:
		f.others = append(f.others, "unrouted "+r.Method+" "+r.URL.Path)
		writeIncidentJSON(w, http.StatusTeapot, `{"error":{"code":"NotRoutedByFake","message":"unexpected request"}}`)
	}
}

// setRetryAfter adds the configured Retry-After header. Caller holds f.mu.
func (f *incidentImportARM) setRetryAfter(w http.ResponseWriter) {
	if f.retryAfter != "" {
		w.Header().Set("Retry-After", f.retryAfter)
	}
}

// preset makes APIM busy with an import started before the replay, until busyUntil.
func (f *incidentImportARM) preset(busyUntil time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opEnd = busyUntil
}

func (f *incidentImportARM) importAnswers() []incidentImportAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]incidentImportAnswer(nil), f.answers...)
}

func (f *incidentImportARM) importCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.answers)
}

func (f *incidentImportARM) counts() (etagGets, polls int, others []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.etagGets, f.polls, append([]string(nil), f.others...)
}

func writeIncidentJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// incidentTransientAnswer is one answer from the incident's error mix, all transient.
type incidentTransientAnswer struct {
	status int
	body   string
}

// incidentTransientScript is the mix of answers APIM gave product, tag and policy writes
// while the import loop kept it busy; the fake cycles through it.
var incidentTransientScript = []incidentTransientAnswer{
	{http.StatusPreconditionFailed, `{"error":{"code":"PreconditionFailed","message":"Resource was modified since last retrieval."}}`},
	{http.StatusUnprocessableEntity, `{"error":{"code":"ManagementApiRequestFailed","message":"Management API request timed out.",` +
		`"details":[{"code":"Timeout","message":"Request timed out"}]}}`},
	{http.StatusConflict, `{"error":{"code":"Conflict","message":"Another operation is in progress."}}`},
	{http.StatusTooManyRequests, `{"error":{"code":"TooManyRequests","message":"Rate limit exceeded."}}`},
	{http.StatusInternalServerError, `{"error":{"code":"InternalServerError","message":"DeadOperationMonitor"}}`},
	{http.StatusServiceUnavailable, ``},
}

// incidentWriteARM answers every request whose method is failMethod with the next entry
// of incidentTransientScript, and everything else with 200. It counts the failing writes.
type incidentWriteARM struct {
	server     *httptest.Server
	failMethod string

	mu       sync.Mutex
	writes   []string // "METHOD path" of every failMethod request
	statuses []int
	others   int
}

func newIncidentWriteARM(failMethod string) *incidentWriteARM {
	f := &incidentWriteARM{failMethod: failMethod}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != f.failMethod {
			f.others++
			writeIncidentJSON(w, http.StatusOK, `{}`)
			return
		}
		answer := incidentTransientScript[len(f.writes)%len(incidentTransientScript)]
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		f.statuses = append(f.statuses, answer.status)
		writeIncidentJSON(w, answer.status, answer.body)
	}))
	return f
}

func (f *incidentWriteARM) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *incidentWriteARM) answered() (writes []string, statuses []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...), append([]int(nil), f.statuses...)
}

// incidentLogTee collects what the suite's zap logger writes to GinkgoWriter, so a spec
// can read back log lines of controllers that log through ctrl.Log rather than the
// context (the deployment controller).
type incidentLogTee struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *incidentLogTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}

// entries returns the key/values of every log line whose message is exactly msg and
// whose "name" key is name. zap's development console encoder writes
// time<TAB>LEVEL<TAB>logger<TAB>message<TAB>{json}, so the message is matched as a whole
// field: a line that merely contains msg does not count.
func (t *incidentLogTee) entries(msg, name string) []map[string]any {
	t.mu.Lock()
	text := t.buf.String()
	t.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Split(line, "\t")
		for i, field := range fields {
			if field != msg || i+1 >= len(fields) {
				continue
			}
			kv := map[string]any{}
			if err := json.Unmarshal([]byte(fields[i+1]), &kv); err != nil {
				continue
			}
			if kv["name"] == name {
				out = append(out, kv)
			}
			break
		}
	}
	return out
}

// countContaining counts log lines that contain s anywhere, for a check that a text
// appears nowhere else either.
func (t *incidentLogTee) countContaining(s, name string) int {
	t.mu.Lock()
	text := t.buf.String()
	t.mu.Unlock()
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, s) && strings.Contains(line, `"name": "`+name+`"`) {
			n++
		}
	}
	return n
}

// incidentStep is one reconcile of a replay.
type incidentStep struct {
	// at is the fake time the reconcile started; after is the fake time it ended (later
	// when the fake ARM moved the clock for an import wait).
	at, after time.Time
	// wrote is whether the reconcile sent at least one write to APIM.
	wrote bool
	// realDuration is how long the reconcile took on the wall clock.
	realDuration time.Duration
	result       ctrl.Result
	phase        string
	retry        apimv1.RetryStatus
}

// incidentNoise is how often something other than the requeue makes the resource
// reconcile again during a replay: every 60 s for the first hour (the old fixed requeue
// cadence, as if a ReplicaSet signal kept arriving), which covers every backoff window
// and the first half hour of a stall, then hourly to the end of the 69 hours.
func incidentNoise(elapsed time.Duration) time.Duration {
	if elapsed < time.Hour {
		return time.Minute
	}
	return time.Hour
}

// replayIncident plays controller-runtime's workqueue for horizon of fake time from the
// clock's current time. A reconcile runs at the earliest of the requeue the last one
// asked for and the next noise event; a requeue and a noise event at the same time run
// once, as the workqueue would. reconcileOnce runs one reconcile, writes reports how many
// writes have reached APIM so far, and status reads the resource's phase and retry
// state back after each reconcile.
func replayIncident(
	clock *incidentClock,
	horizon time.Duration,
	noise func(elapsed time.Duration) time.Duration,
	reconcileOnce func() ctrl.Result,
	writes func() int,
	status func() (string, apimv1.RetryStatus),
) []incidentStep {
	start := clock.now()
	end := start.Add(horizon)
	due := start // the create event
	nextNoise := start.Add(noise(0))

	var steps []incidentStep
	for i := 0; i < 100000; i++ {
		at := nextNoise
		if !due.IsZero() && due.Before(at) {
			at = due
		}
		if !at.Before(end) {
			break
		}
		if at.After(clock.now()) {
			clock.set(at)
		}
		at = clock.now()

		before := writes()
		realStart := time.Now()
		result := reconcileOnce()
		realDuration := time.Since(realStart)
		after := clock.now()
		phase, retry := status()
		steps = append(steps, incidentStep{
			at: at, after: after, wrote: writes() > before, realDuration: realDuration,
			result: result, phase: phase, retry: retry,
		})

		due = time.Time{}
		if result.RequeueAfter > 0 {
			due = after.Add(result.RequeueAfter)
		}
		for !nextNoise.After(after) {
			nextNoise = nextNoise.Add(noise(nextNoise.Sub(start)))
		}
	}
	return steps
}
