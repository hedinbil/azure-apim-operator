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
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

func TestClassifyAPIMError(t *testing.T) {
	httpErr := func(method string, status int, code string) error {
		return &apim.Error{Operation: "op", Method: method, StatusCode: status, Code: code}
	}
	cases := []struct {
		name string
		err  error
		want apimErrorClass
	}{
		{"wait timeout", &apim.Error{Operation: "import API", Err: apim.ErrImportWaitTimeout}, errorClassTransient},
		{"wait timeout wrapped again", fmt.Errorf("step: %w", &apim.Error{Err: apim.ErrImportWaitTimeout}), errorClassTransient},
		{"400 on a write", httpErr(http.MethodPut, 400, "ValidationError"), errorClassPermanent},
		{"401", httpErr(http.MethodPut, 401, "InvalidAuthenticationToken"), errorClassPermanent},
		{"403", httpErr(http.MethodPatch, 403, "AuthorizationFailed"), errorClassPermanent},
		{"404 on a PUT", httpErr(http.MethodPut, 404, "ResourceNotFound"), errorClassPermanent},
		{"404 on a DELETE", httpErr(http.MethodDelete, 404, "ResourceNotFound"), errorClassPermanent},
		{"404 on a GET", httpErr(http.MethodGet, 404, "ResourceNotFound"), errorClassTransient},
		// A product/tag assignment or a policy whose product, tag or API is not in APIM yet.
		{"404 on a dependent PUT", &apim.Error{Method: http.MethodPut, StatusCode: 404, Code: "ResourceNotFound",
			Err: apim.ErrDependencyNotFound}, errorClassTransient},
		{"404 on a dependent PATCH, wrapped by a caller", fmt.Errorf("step 5: %w", &apim.Error{Method: http.MethodPatch,
			StatusCode: 404, Err: apim.ErrDependencyNotFound}), errorClassTransient},
		{"409", httpErr(http.MethodPut, 409, "Conflict"), errorClassTransient},
		{"412", httpErr(http.MethodPut, 412, "PreconditionFailed"), errorClassTransient},
		{"422 management API timed out", httpErr(http.MethodPut, 422, "ManagementApiRequestFailed"), errorClassTransient},
		{"429", httpErr(http.MethodPut, 429, "TooManyRequests"), errorClassTransient},
		{"500", httpErr(http.MethodPut, 500, "InternalServerError"), errorClassTransient},
		{"502 without a code", httpErr(http.MethodPut, 502, ""), errorClassTransient},
		{"503", httpErr(http.MethodPut, 503, ""), errorClassTransient},
		{"504", httpErr(http.MethodPut, 504, "GatewayTimeout"), errorClassTransient},
		{"400 with a transient code", httpErr(http.MethodPut, 400, "PreconditionFailed"), errorClassTransient},
		{"400 with a transient detail code", &apim.Error{Method: http.MethodPut, StatusCode: 400, Code: "BadRequest", DetailCode: "Timeout"},
			errorClassTransient},
		{"403 with Conflict code", httpErr(http.MethodPut, 403, "Conflict"), errorClassTransient},
		{"async DeadOperationMonitor", &apim.Error{Code: "InternalServerError", DetailCode: "DeadOperationMonitor", Err: apim.ErrAsyncOperationFailed},
			errorClassTransient},
		{"async failure with an unknown code", &apim.Error{Code: "ValidationError", Err: apim.ErrAsyncOperationFailed}, errorClassTransient},
		{"context deadline", fmt.Errorf("wait: %w", context.DeadlineExceeded), errorClassTransient},
		{"context canceled", context.Canceled, errorClassTransient},
		{"transport", fmt.Errorf("upsert tag: %w", &url.Error{Op: "Put", URL: "https://x", Err: errors.New("connection reset by peer")}),
			errorClassTransient},
		{"unknown", errors.New("something odd"), errorClassTransient},
		{"nil", nil, errorClassTransient},
		{"permanent wrapped by a caller", fmt.Errorf("step 4: %w", httpErr(http.MethodPut, 400, "")), errorClassPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAPIMError(tc.err); got != tc.want {
				t.Errorf("classifyAPIMError(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryDelaySequenceAndCap(t *testing.T) {
	p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5}
	want := []time.Duration{
		time.Minute, time.Minute, // n=0 is treated as the first failure
		2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	for n, w := range want {
		if got := p.delay(int32(n)); got != w {
			t.Errorf("delay(%d) = %s, want %s", n, got, w)
		}
	}
	// No overflow however many failures pile up.
	if got := p.delay(1 << 20); got != 30*time.Minute {
		t.Errorf("delay(huge) = %s, want the 30m cap", got)
	}
}

func TestRetryDelayJitterBounds(t *testing.T) {
	low := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, Jitter: 0.2, Random: func() float64 { return 0 }}
	if got := low.delay(1); got != 48*time.Second {
		t.Errorf("delay with random=0 = %s, want 48s (-20%%)", got)
	}
	mid := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, Jitter: 0.2, Random: func() float64 { return 0.5 }}
	if got := mid.delay(2); got != 2*time.Minute {
		t.Errorf("delay with random=0.5 = %s, want exactly 2m", got)
	}
	// The cap applies before jitter, so the longest wait is 30m + 20 %.
	high := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, Jitter: 0.2, Random: func() float64 { return 0.999999 }}
	if got := high.delay(10); got > 36*time.Minute || got < 35*time.Minute {
		t.Errorf("delay at the cap with random~1 = %s, want just under 36m", got)
	}

	// The production policy, with its real random source, stays inside +/-20 %.
	for n := int32(1); n <= 8; n++ {
		base := (&retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute}).delay(n)
		for range 200 {
			got := productionRetryPolicy.delay(n)
			if got < time.Duration(float64(base)*0.8) || got > time.Duration(float64(base)*1.2) {
				t.Fatalf("production delay(%d) = %s, outside +/-20%% of %s", n, got, base)
			}
		}
	}
}

func TestRetryPolicyDefaults(t *testing.T) {
	var nilPolicy *retryPolicy
	got := nilPolicy.effective()
	if got.BaseDelay != time.Minute || got.MaxDelay != 30*time.Minute || got.MaxAttempts != 5 || got.Jitter != 0.2 {
		t.Errorf("nil policy = %+v, want production defaults 1m/30m/5/0.2", got)
	}
	partial := (&retryPolicy{BaseDelay: time.Millisecond}).effective()
	if partial.BaseDelay != time.Millisecond || partial.MaxDelay != 30*time.Minute || partial.MaxAttempts != 5 ||
		partial.Jitter != 0 || partial.Now == nil || partial.Random == nil {
		t.Errorf("partial policy = %+v, want unset fields from production and no jitter", partial)
	}
	if got := apimWriterOptions().MaxConcurrentReconciles; got != 4 {
		t.Errorf("MaxConcurrentReconciles = %d, want 4", got)
	}
}

// fakeClock is a settable clock for the state machine tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// testPolicy is the production shape (1m, 30m cap, 5 attempts) without jitter, on a
// fake clock.
func testPolicy(clock *fakeClock) *retryPolicy {
	return &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now}
}

func testTag(annotations map[string]string) *apimv1.APIMTag {
	return &apimv1.APIMTag{ObjectMeta: metav1.ObjectMeta{
		Name: "tag-a", Namespace: "team-a", Generation: 1, Annotations: annotations,
	}}
}

var (
	errTransient = &apim.Error{Operation: "upsert tag t1", Method: http.MethodPut, StatusCode: 412, Code: "PreconditionFailed"}
	errPermanent = &apim.Error{Operation: "upsert tag t1", Method: http.MethodPut, StatusCode: 400, Code: "ValidationError"}
)

func TestWriteBacksOffThenStalls(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(nil)
	st := &tag.Status.RetryStatus

	wantDelays := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i, wantDelay := range wantDelays {
		w := p.begin(logr.Discard(), "APIMTag", tag)
		if proceed, _ := w.gate(*st, false); !proceed {
			t.Fatalf("attempt %d: gate refused a due write", i+1)
		}
		out := w.failed(st, errTransient)
		if out.Phase != phaseBackoff || out.Class != errorClassTransient {
			t.Fatalf("attempt %d: outcome = %+v, want Backoff/transient", i+1, out)
		}
		if out.Result.RequeueAfter != wantDelay {
			t.Errorf("attempt %d: RequeueAfter = %s, want %s", i+1, out.Result.RequeueAfter, wantDelay)
		}
		if st.ConsecutiveFailures != int32(i+1) {
			t.Errorf("attempt %d: consecutiveFailures = %d", i+1, st.ConsecutiveFailures)
		}
		if want := clock.now().Add(wantDelay).Format(time.RFC3339); st.NextAttemptAt != want {
			t.Errorf("attempt %d: nextAttemptAt = %s, want %s", i+1, st.NextAttemptAt, want)
		}
		if want := fmt.Sprintf("attempt %d/5", i+1); !strings.Contains(out.Message, want) {
			t.Errorf("attempt %d: message %q does not say %q", i+1, out.Message, want)
		}

		// A reconcile half way through the wait must not write, and requeues for the rest.
		clock.advance(wantDelay / 2)
		early := p.begin(logr.Discard(), "APIMTag", tag)
		proceed, res := early.gate(*st, false)
		if proceed {
			t.Fatalf("attempt %d: gate let a write through before nextAttemptAt", i+1)
		}
		if res.RequeueAfter != wantDelay-wantDelay/2 {
			t.Errorf("attempt %d: early RequeueAfter = %s, want %s", i+1, res.RequeueAfter, wantDelay-wantDelay/2)
		}
		clock.advance(wantDelay - wantDelay/2)
	}

	w := p.begin(logr.Discard(), "APIMTag", tag)
	if proceed, _ := w.gate(*st, false); !proceed {
		t.Fatal("attempt 5: gate refused a due write")
	}
	out := w.failed(st, errTransient)
	if out.Phase != phaseStalled {
		t.Fatalf("attempt 5: phase = %s, want Stalled", out.Phase)
	}
	if out.Result != (ctrl.Result{}) {
		t.Errorf("Stalled must not requeue, got %+v", out.Result)
	}
	if st.NextAttemptAt != "" || st.ConsecutiveFailures != 5 {
		t.Errorf("stalled status = %+v, want 5 failures and no nextAttemptAt", *st)
	}

	// From here on nothing writes, however much time passes.
	clock.advance(24 * time.Hour)
	proceed, res := p.begin(logr.Discard(), "APIMTag", tag).gate(*st, false)
	if proceed || res != (ctrl.Result{}) {
		t.Errorf("stalled gate = %v, %+v; want no write and no requeue", proceed, res)
	}
}

func TestPermanentErrorIsInvalidAtOnce(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(nil)
	st := &tag.Status.RetryStatus

	w := p.begin(logr.Discard(), "APIMTag", tag)
	w.gate(*st, false)
	out := w.failed(st, errPermanent)
	if out.Phase != phaseInvalid || out.Class != errorClassPermanent || out.Result != (ctrl.Result{}) {
		t.Fatalf("outcome = %+v, want Invalid/permanent without requeue", out)
	}
	if st.ConsecutiveFailures != 1 || st.NextAttemptAt != "" {
		t.Errorf("status = %+v, want 1 failure and no nextAttemptAt", *st)
	}
	clock.advance(time.Hour)
	if proceed, _ := p.begin(logr.Discard(), "APIMTag", tag).gate(*st, false); proceed {
		t.Error("an Invalid resource must not be written again without a reset")
	}
}

func TestPermanentErrorAfterTransientOnes(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(nil)
	st := &tag.Status.RetryStatus
	for range 2 {
		w := p.begin(logr.Discard(), "APIMTag", tag)
		w.gate(*st, false)
		out := w.failed(st, errTransient)
		clock.advance(out.Result.RequeueAfter)
	}
	w := p.begin(logr.Discard(), "APIMTag", tag)
	w.gate(*st, false)
	if out := w.failed(st, errPermanent); out.Phase != phaseInvalid {
		t.Errorf("phase = %s, want Invalid", out.Phase)
	}
}

func TestSpecChangeResetsTheFailures(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(nil)
	st := &tag.Status.RetryStatus
	st.ConsecutiveFailures = 5 // Stalled

	w := p.begin(logr.Discard(), "APIMTag", tag)
	proceed, res := w.gate(*st, true)
	if !proceed || res != (ctrl.Result{}) {
		t.Fatalf("gate after a spec change = %v, %+v; want a write", proceed, res)
	}
	if st.ConsecutiveFailures != 5 {
		t.Error("gate must not modify the status itself")
	}
	out := w.failed(st, errTransient)
	if st.ConsecutiveFailures != 1 || out.Phase != phaseBackoff {
		t.Errorf("after reset and one failure: failures = %d, phase = %s; want 1, Backoff", st.ConsecutiveFailures, out.Phase)
	}

	// A spec change also cuts a running backoff short.
	proceed, _ = p.begin(logr.Discard(), "APIMTag", tag).gate(*st, true)
	if !proceed {
		t.Error("a spec change must not wait for nextAttemptAt")
	}
}

func TestRetryAnnotationResetsOncePerValue(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(map[string]string{retryAnnotation: "1"})
	st := &tag.Status.RetryStatus
	st.ConsecutiveFailures = 1 // Invalid

	w := p.begin(logr.Discard(), "APIMTag", tag)
	if proceed, _ := w.gate(*st, false); !proceed {
		t.Fatal("a new retry annotation value must allow a write")
	}
	out := w.failed(st, errPermanent)
	if st.LastRetryAnnotation != "1" || st.ConsecutiveFailures != 1 || out.Phase != phaseInvalid {
		t.Fatalf("status = %+v phase %s; want the value remembered, a fresh count and Invalid", *st, out.Phase)
	}

	// The same value again does not retrigger.
	if proceed, _ := p.begin(logr.Discard(), "APIMTag", tag).gate(*st, false); proceed {
		t.Error("the same annotation value must not retry again")
	}
	// Removing it does not either.
	tag.Annotations = nil
	if proceed, _ := p.begin(logr.Discard(), "APIMTag", tag).gate(*st, false); proceed {
		t.Error("removing the annotation must not retry")
	}
	// A new value does.
	tag.Annotations = map[string]string{retryAnnotation: "2"}
	w = p.begin(logr.Discard(), "APIMTag", tag)
	if proceed, _ := w.gate(*st, false); !proceed {
		t.Fatal("a changed annotation value must allow a write")
	}
	w.succeeded(st)
	if *st != (apimv1.RetryStatus{LastRetryAnnotation: "2"}) {
		t.Errorf("status after success = %+v, want only the remembered annotation", *st)
	}
}

func TestRetryAnnotationOnAHealthyResourceIsRemembered(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(map[string]string{retryAnnotation: "x"})
	st := &tag.Status.RetryStatus
	w := p.begin(logr.Discard(), "APIMTag", tag)
	if proceed, _ := w.gate(*st, false); !proceed {
		t.Fatal("gate refused a healthy resource")
	}
	w.succeeded(st)
	if st.LastRetryAnnotation != "x" {
		t.Errorf("lastRetryAnnotation = %q, want x so later failures are not reset by it", st.LastRetryAnnotation)
	}
}

func TestPrepareIsIdempotentAndPersistsTheReset(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(map[string]string{retryAnnotation: "go"})
	st := &tag.Status.RetryStatus
	st.ConsecutiveFailures = 5

	w := p.begin(logr.Discard(), "APIMTag", tag)
	w.gate(*st, false)
	w.prepare(st)
	w.prepare(st)
	if *st != (apimv1.RetryStatus{LastRetryAnnotation: "go"}) {
		t.Fatalf("after prepare = %+v, want counters cleared and the value remembered", *st)
	}
	w.failed(st, errTransient)
	if st.ConsecutiveFailures != 1 {
		t.Errorf("failed after prepare counted %d, want 1", st.ConsecutiveFailures)
	}
}

func TestSuccessClearsTheBackoff(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	tag := testTag(nil)
	st := &tag.Status.RetryStatus
	st.ConsecutiveFailures = 3
	st.NextAttemptAt = clock.now().Format(time.RFC3339)

	w := p.begin(logr.Discard(), "APIMTag", tag)
	if proceed, _ := w.gate(*st, false); !proceed {
		t.Fatal("a write at exactly nextAttemptAt is due")
	}
	w.succeeded(st)
	if *st != (apimv1.RetryStatus{}) {
		t.Errorf("status after success = %+v, want empty", *st)
	}
}

func TestUnreadableNextAttemptAtIsDue(t *testing.T) {
	p := testPolicy(&fakeClock{t: time.Now()})
	st := apimv1.RetryStatus{ConsecutiveFailures: 2, NextAttemptAt: "tomorrow-ish"}
	if proceed, _ := p.begin(logr.Discard(), "APIMTag", testTag(nil)).gate(st, false); !proceed {
		t.Error("an unreadable nextAttemptAt must not block writes for good")
	}
}

// TestNextAttemptAtIsRoundedUp: the status keeps whole seconds, so with a clock in the
// middle of a second the stored time is rounded up and the requeue lands on or after it.
func TestNextAttemptAtIsRoundedUp(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 400*int(time.Millisecond), time.UTC)}
	p := testPolicy(clock)
	tag := testTag(nil)
	st := &tag.Status.RetryStatus
	w := p.begin(logr.Discard(), "APIMTag", tag)
	w.gate(*st, false)
	out := w.failed(st, errTransient)
	if st.NextAttemptAt != "2026-09-25T10:01:01Z" {
		t.Errorf("nextAttemptAt = %s, want 10:01:01 (rounded up)", st.NextAttemptAt)
	}
	if out.Result.RequeueAfter != 60*time.Second+600*time.Millisecond {
		t.Errorf("RequeueAfter = %s, want 1m0.6s", out.Result.RequeueAfter)
	}
	clock.advance(out.Result.RequeueAfter)
	if proceed, _ := p.begin(logr.Discard(), "APIMTag", tag).gate(*st, false); !proceed {
		t.Error("the requeue must land when the write is due")
	}
}

// TestIncidentReplay replays Sep 2026 against the new policy: a write that times out
// every time. It used to run ~800 times in 69 h; now it runs five times in about 15
// minutes and stops.
func TestIncidentReplay(t *testing.T) {
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: start}
	p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Jitter: 0.2,
		Now: clock.now, Random: rand.New(rand.NewPCG(1, 2)).Float64}
	tag := testTag(nil)
	st := &tag.Status.RetryStatus
	timeout := &apim.Error{Operation: "import API", Err: apim.ErrImportWaitTimeout}

	writes := 0
	phase := ""
	for reconcile := 0; reconcile < 10000 && clock.now().Before(start.Add(69*time.Hour)); reconcile++ {
		w := p.begin(logr.Discard(), "APIMAPIDeployment", tag)
		proceed, res := w.gate(*st, false)
		if proceed {
			writes++
			out := w.failed(st, timeout)
			phase, res = out.Phase, out.Result
		}
		if res.RequeueAfter == 0 {
			// Nothing scheduled: the old code's fixed 60 s requeue would have fired here,
			// so keep poking to prove the gate holds.
			res.RequeueAfter = time.Minute
		}
		clock.advance(res.RequeueAfter)
	}
	if writes != 5 || phase != phaseStalled {
		t.Errorf("writes = %d, phase = %s; want 5 writes and Stalled", writes, phase)
	}
}

// logLine is one captured log call.
type logLine struct {
	msg string
	kv  map[string]any
	err bool
}

// captureLogs returns a logger that records every line.
func captureLogs() (logr.Logger, func() []logLine) {
	var mu sync.Mutex
	var lines []logLine
	logger := logr.New(&recordingSink{record: func(l logLine) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, l)
	}})
	return logger, func() []logLine {
		mu.Lock()
		defer mu.Unlock()
		return append([]logLine(nil), lines...)
	}
}

// recordingSink is a minimal logr.LogSink that keeps message and keys.
type recordingSink struct {
	record func(logLine)
	values []any
}

func (s *recordingSink) Init(logr.RuntimeInfo)        {}
func (s *recordingSink) Enabled(int) bool             { return true }
func (s *recordingSink) WithName(string) logr.LogSink { return s }
func (s *recordingSink) WithValues(kv ...any) logr.LogSink {
	return &recordingSink{record: s.record, values: append(append([]any(nil), s.values...), kv...)}
}
func (s *recordingSink) Info(_ int, msg string, kv ...any) { s.emit(msg, false, kv) }
func (s *recordingSink) Error(_ error, msg string, kv ...any) {
	s.emit(msg, true, kv)
}
func (s *recordingSink) emit(msg string, isErr bool, kv []any) {
	all := append(append([]any(nil), s.values...), kv...)
	m := map[string]any{}
	for i := 0; i+1 < len(all); i += 2 {
		m[fmt.Sprint(all[i])] = all[i+1]
	}
	s.record(logLine{msg: msg, kv: m, err: isErr})
}

func TestLogLines(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)}
	p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 2, Now: clock.now}
	tag := testTag(nil)
	st := &tag.Status.RetryStatus
	logger, lines := captureLogs()

	w := p.begin(logger, "APIMTag", tag, "tagID", "t1")
	w.gate(*st, false)
	w.starting()
	out := w.failed(st, errTransient)
	p.begin(logger, "APIMTag", tag, "tagID", "t1").gate(*st, false) // still backing off
	clock.advance(out.Result.RequeueAfter)
	w = p.begin(logger, "APIMTag", tag, "tagID", "t1")
	w.gate(*st, false)
	w.failed(st, errTransient)                                      // stalls (MaxAttempts 2)
	p.begin(logger, "APIMTag", tag, "tagID", "t1").gate(*st, false) // held

	got := lines()
	wantMsgs := []string{msgWriteStarting, msgWriteFailed, msgWriteBackingOff, msgWriteStalled, msgWriteHeld}
	if len(got) != len(wantMsgs) {
		t.Fatalf("logged %d lines, want %d: %+v", len(got), len(wantMsgs), got)
	}
	for i, want := range wantMsgs {
		if got[i].msg != want {
			t.Errorf("line %d = %q, want %q", i, got[i].msg, want)
		}
		for _, key := range []string{"kind", "namespace", "name", "tagID"} {
			if _, ok := got[i].kv[key]; !ok {
				t.Errorf("line %d (%s) lacks %q", i, got[i].msg, key)
			}
		}
	}
	if got[0].kv["attempt"] != "1/2" {
		t.Errorf("starting attempt = %v, want 1/2", got[0].kv["attempt"])
	}
	failed := got[1]
	if failed.kv["class"] != errorClassTransient || failed.kv["attempt"] != "1/2" || failed.kv["nextAttemptAt"] != "2026-09-25T10:01:00Z" {
		t.Errorf("failed line = %+v, want class, attempt 1/2 and nextAttemptAt", failed.kv)
	}

	// The stalled line is what the Datadog monitor matches: exact message, these keys.
	stalled := got[3]
	if stalled.msg != "🛑 APIM write stalled" {
		t.Errorf("stalled message = %q; the Datadog monitor matches it verbatim", stalled.msg)
	}
	want := map[string]any{"kind": "APIMTag", "namespace": "team-a", "name": "tag-a", "attempts": int32(2),
		"lastError": errTransient.Error(), "tagID": "t1"}
	for k, v := range want {
		if stalled.kv[k] != v {
			t.Errorf("stalled %s = %v, want %v", k, stalled.kv[k], v)
		}
	}

	// A rejected write says so, with its own stable message.
	tag2 := testTag(nil)
	logger2, lines2 := captureLogs()
	w = p.begin(logger2, "APIMTag", tag2)
	w.gate(tag2.Status.RetryStatus, false)
	w.failed(&tag2.Status.RetryStatus, errPermanent)
	if l := lines2(); len(l) != 1 || l[0].msg != "💔 APIM write rejected; not retrying" || l[0].kv["class"] != errorClassPermanent {
		t.Errorf("rejected lines = %+v", l)
	}

	// Success is logged with 💚; a reset is logged when it clears real failures.
	logger3, lines3 := captureLogs()
	st3 := apimv1.RetryStatus{ConsecutiveFailures: 2}
	w = p.begin(logger3, "APIMTag", tag2)
	w.gate(st3, true)
	w.succeeded(&st3)
	if l := lines3(); len(l) != 2 || l[0].msg != msgWriteReset || l[1].msg != msgWriteSucceeded {
		t.Errorf("reset+success lines = %+v", l)
	}
}

func TestManagementTokenFuncSeam(t *testing.T) {
	var f managementTokenFunc = func(_ context.Context, clientID, tenantID string) (string, error) {
		return clientID + "/" + tenantID, nil
	}
	if got, err := f.get(context.Background(), "c", "t"); err != nil || got != "c/t" {
		t.Errorf("get() = %q, %v; want the injected token", got, err)
	}
}

// TestHeldStatusPutsTheStoppedPhaseBack covers what a reconcile that gate turned away
// writes when an error path before the gate (missing APIMService, missing identity,
// failed OpenAPI fetch) has overwritten the phase in the meantime.
func TestHeldStatusPutsTheStoppedPhaseBack(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}
	p := testPolicy(clock)
	next := clock.t.Add(time.Minute).Format(time.RFC3339)

	cases := []struct {
		name        string
		st          apimv1.RetryStatus
		phase       string
		specChanged bool
		wantPhase   string
		wantOK      bool
		wantInMsg   string
	}{
		{name: "stalled after an error path", st: apimv1.RetryStatus{ConsecutiveFailures: 5}, phase: phaseError,
			wantPhase: phaseStalled, wantOK: true, wantInMsg: "stalled after 5 failures"},
		{name: "invalid after an error path", st: apimv1.RetryStatus{ConsecutiveFailures: 1}, phase: phaseError,
			wantPhase: phaseInvalid, wantOK: true, wantInMsg: "rejected"},
		{name: "backing off after an error path", st: apimv1.RetryStatus{ConsecutiveFailures: 2, NextAttemptAt: next},
			phase: phaseError, wantPhase: phaseBackoff, wantOK: true, wantInMsg: next},
		{name: "stalled already says so", st: apimv1.RetryStatus{ConsecutiveFailures: 5}, phase: phaseStalled},
		{name: "invalid on the last attempt keeps its precise phase", st: apimv1.RetryStatus{ConsecutiveFailures: 5},
			phase: phaseInvalid},
		{name: "backing off already says so", st: apimv1.RetryStatus{ConsecutiveFailures: 2, NextAttemptAt: next},
			phase: phaseBackoff},
		{name: "a write that goes ahead writes nothing here", st: apimv1.RetryStatus{}, phase: phaseError},
		{name: "a spec change goes ahead", st: apimv1.RetryStatus{ConsecutiveFailures: 5}, phase: phaseError,
			specChanged: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := p.begin(logr.Discard(), "APIMTag", testTag(nil))
			w.gate(tc.st, tc.specChanged)
			phase, msg, ok := w.heldStatus(tc.phase)
			if ok != tc.wantOK || phase != tc.wantPhase {
				t.Fatalf("heldStatus(%q) = %q, %v; want %q, %v", tc.phase, phase, ok, tc.wantPhase, tc.wantOK)
			}
			if !strings.Contains(msg, tc.wantInMsg) {
				t.Errorf("message %q does not contain %q", msg, tc.wantInMsg)
			}
		})
	}
}
