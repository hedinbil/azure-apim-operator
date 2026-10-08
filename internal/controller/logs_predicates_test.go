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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// These tests pin down the two things operators see of the retry handling without
// reading code: the log lines (emoji, message, keys; the stalled one feeds a Datadog
// monitor) and which updates reach Reconcile (the retry annotation must, status-only
// updates must not). The log tests drive the shared apimWrite with the exact keys each
// controller passes; the envtest counterparts in logs_predicates_envtest_test.go run
// the real reconcilers.

var lpStart = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// lpPolicy is the production shape (1 min doubling to a 30 min cap, 5 attempts) without
// jitter, on clock.
func lpPolicy(clock *lpClock) *retryPolicy {
	return &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Now: clock.now}
}

var (
	lpErrTransient = &apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusPreconditionFailed,
		Code: "PreconditionFailed", Message: "the entity changed"}
	lpErrPermanent = &apim.Error{Operation: "import API", Method: http.MethodPut, StatusCode: http.StatusBadRequest,
		Code: "ValidationError", Message: "the document is not valid"}
)

// lpFixture is one kind that writes to APIM, with the log keys its controller passes to
// begin (see each controller's r.retry.begin call).
type lpFixture struct {
	kind string
	obj  client.Object
	st   *apimv1.RetryStatus
	// ids are the extra keys the controller adds after kind, namespace and name.
	ids []any
}

// lpFixtures returns fresh objects of the four kinds, all team-a/res-a at generation 1.
func lpFixtures() []lpFixture {
	meta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: "res-a", Namespace: "team-a", Generation: 1}
	}
	deployment := &apimv1.APIMAPIDeployment{ObjectMeta: meta(), Spec: apimv1.APIMAPIDeploymentSpec{APIID: "api-1"}}
	product := &apimv1.APIMProduct{ObjectMeta: meta(), Spec: apimv1.APIMProductSpec{ProductID: "prod-1"}}
	tag := &apimv1.APIMTag{ObjectMeta: meta(), Spec: apimv1.APIMTagSpec{TagID: "tag-1"}}
	policy := &apimv1.APIMInboundPolicy{ObjectMeta: meta(),
		Spec: apimv1.APIMInboundPolicySpec{APIID: "api-1", OperationID: "op-1"}}
	return []lpFixture{
		{kind: "APIMAPIDeployment", obj: deployment, st: &deployment.Status.RetryStatus, ids: []any{"apiID", "api-1"}},
		{kind: "APIMProduct", obj: product, st: &product.Status.RetryStatus,
			ids: []any{"productID", "prod-1", "operation", "upsert"}},
		{kind: "APIMTag", obj: tag, st: &tag.Status.RetryStatus, ids: []any{"tagID", "tag-1"}},
		{kind: "APIMInboundPolicy", obj: policy, st: &policy.Status.RetryStatus,
			ids: []any{"apiID", "api-1", "operationID", "op-1"}},
	}
}

// begin starts a write for f the way its controller does.
func (f lpFixture) begin(p *retryPolicy, c *lpCapture) *apimWrite {
	return p.begin(c.logger(), f.kind, f.obj, f.ids...)
}

// lpExpectLine checks one line: message, level, the identity keys first and in order
// (kind, namespace, name, then the controller's ids), and the wanted values.
func lpExpectLine(t *testing.T, f lpFixture, l lpLine, msg string, isError bool, want map[string]string) {
	t.Helper()
	if l.msg != msg {
		t.Errorf("message = %q, want %q", l.msg, msg)
		return
	}
	if l.isError != isError {
		t.Errorf("%s: logged as error = %v, want %v", msg, l.isError, isError)
	}
	wantKeys := []string{"kind", "namespace", "name"}
	for i := 0; i+1 < len(f.ids); i += 2 {
		wantKeys = append(wantKeys, f.ids[i].(string))
	}
	if len(l.keys) < len(wantKeys) {
		t.Errorf("%s: keys = %v, want them to start with %v", msg, l.keys, wantKeys)
	} else {
		for i, k := range wantKeys {
			if l.keys[i] != k {
				t.Errorf("%s: key %d = %q, want %q (keys %v)", msg, i, l.keys[i], k, l.keys)
			}
		}
	}
	identity := map[string]string{"kind": f.kind, "namespace": "team-a", "name": "res-a"}
	for i := 0; i+1 < len(f.ids); i += 2 {
		identity[f.ids[i].(string)] = fmt.Sprint(f.ids[i+1])
	}
	for k, v := range identity {
		if got := l.str(k); got != v {
			t.Errorf("%s: %s = %q, want %q", msg, k, got, v)
		}
	}
	for k, v := range want {
		if got := l.str(k); got != v {
			t.Errorf("%s: %s = %q, want %q", msg, k, got, v)
		}
	}
	if l.has("<odd>") {
		t.Errorf("%s: odd number of key/value arguments: %v", msg, l.keys)
	}
}

// lpTake returns the captured lines and clears the capture, failing when the count is
// not n.
func lpTake(t *testing.T, c *lpCapture, n int) []lpLine {
	t.Helper()
	lines := c.all()
	c.reset()
	if len(lines) != n {
		t.Fatalf("logged %d lines %q, want %d", len(lines), lpMsgs(lines), n)
	}
	return lines
}

// TestLPMessageLiterals: the constants retry.go logs with are the agreed strings, emoji
// variation selectors included (▶️ and ⏸️ are two runes each).
func TestLPMessageLiterals(t *testing.T) {
	cases := []struct{ got, want string }{
		{msgWriteStarting, lpMsgStarting},
		{msgWriteSucceeded, lpMsgSucceeded},
		{msgWriteFailed, lpMsgFailed},
		{msgWriteRejected, lpMsgRejected},
		{msgWriteBackingOff, lpMsgBackingOff},
		{msgWriteHeld, lpMsgHeld},
		{msgWriteStalled, lpMsgStalled},
		{msgWriteReset, lpMsgReset},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("message %q, want %q", tc.got, tc.want)
		}
	}
	prefixes := map[string]string{
		lpMsgStarting: "▶️ ", lpMsgBackingOff: "⏸️ ", lpMsgHeld: "⏸️ ",
		lpMsgSucceeded: "\U0001F49A ", lpMsgFailed: "\U0001F494 ", lpMsgRejected: "\U0001F494 ",
		lpMsgStalled: "\U0001F6D1 ",
	}
	for msg, prefix := range prefixes {
		if !strings.HasPrefix(msg, prefix) {
			t.Errorf("%q does not start with %+q", msg, prefix)
		}
	}
	if msgWriteStalled != "🛑 APIM write stalled" {
		t.Errorf("the Datadog monitor matches %q verbatim, got %q", "🛑 APIM write stalled", msgWriteStalled)
	}
	if retryAnnotation != "apim.operator.io/retry" {
		t.Errorf("retry annotation = %q", retryAnnotation)
	}
}

// TestLPWriteLifecycleLogsPerKind runs one resource of each kind through the whole state
// machine and checks every line it logs: start, failure with attempt n/5 and the next
// attempt time, the backoff skip, stalled, held, reset by annotation, rejected, reset by
// spec change, success.
func TestLPWriteLifecycleLogsPerKind(t *testing.T) {
	for _, f := range lpFixtures() {
		t.Run(f.kind, func(t *testing.T) {
			clock := &lpClock{t: lpStart}
			p := lpPolicy(clock)
			c := &lpCapture{}

			// Attempt 1: start, fail transiently.
			w := f.begin(p, c)
			if proceed, res := w.gate(*f.st, false); !proceed || !res.IsZero() {
				t.Fatalf("a fresh resource must write: proceed=%v result=%+v", proceed, res)
			}
			lpTake(t, c, 0)
			w.starting()
			out := w.failed(f.st, lpErrTransient)
			lines := lpTake(t, c, 2)
			lpExpectLine(t, f, lines[0], lpMsgStarting, false, map[string]string{"attempt": "1/5"})
			lpExpectLine(t, f, lines[1], lpMsgFailed, true, map[string]string{
				"class": "transient", "attempt": "1/5", "nextAttemptAt": "2026-09-25T10:01:00Z"})
			if !errors.Is(lines[1].err, lpErrTransient) {
				t.Errorf("failed line error = %v, want the APIM error", lines[1].err)
			}
			if out.Phase != phaseBackoff || out.Result.RequeueAfter != time.Minute {
				t.Errorf("outcome = %+v, want Backoff after 1m", out)
			}

			// A reconcile before nextAttemptAt: one ⏸️ line, nothing written.
			proceed, res := f.begin(p, c).gate(*f.st, false)
			lines = lpTake(t, c, 1)
			lpExpectLine(t, f, lines[0], lpMsgBackingOff, false, map[string]string{
				"attempt": "2/5", "nextAttemptAt": "2026-09-25T10:01:00Z", "remaining": "1m0s"})
			if proceed || res.RequeueAfter != time.Minute {
				t.Errorf("gate = %v, %+v; want skip with RequeueAfter 1m", proceed, res)
			}
			clock.advance(45 * time.Second)
			proceed, res = f.begin(p, c).gate(*f.st, false)
			lines = lpTake(t, c, 1)
			lpExpectLine(t, f, lines[0], lpMsgBackingOff, false, map[string]string{"remaining": "15s"})
			if proceed || res.RequeueAfter != 15*time.Second {
				t.Errorf("gate = %v, %+v; want skip with RequeueAfter 15s", proceed, res)
			}

			// Attempts 2-4: each waits twice as long as the one before.
			wantNext := []string{"", "", "2026-09-25T10:03:00Z", "2026-09-25T10:07:00Z", "2026-09-25T10:15:00Z"}
			for n := 2; n <= 4; n++ {
				next, _ := time.Parse(time.RFC3339, f.st.NextAttemptAt)
				clock.set(next)
				w = f.begin(p, c)
				if proceed, _ := w.gate(*f.st, false); !proceed {
					t.Fatalf("attempt %d: gate at nextAttemptAt must proceed", n)
				}
				w.starting()
				w.failed(f.st, lpErrTransient)
				lines = lpTake(t, c, 2)
				label := fmt.Sprintf("%d/5", n)
				lpExpectLine(t, f, lines[0], lpMsgStarting, false, map[string]string{"attempt": label})
				lpExpectLine(t, f, lines[1], lpMsgFailed, true, map[string]string{
					"class": "transient", "attempt": label, "nextAttemptAt": wantNext[n]})
			}

			// Attempt 5: stalled, with the exact keys the Datadog monitor reads.
			next, _ := time.Parse(time.RFC3339, f.st.NextAttemptAt)
			clock.set(next)
			w = f.begin(p, c)
			w.gate(*f.st, false)
			w.starting()
			out = w.failed(f.st, lpErrTransient)
			lines = lpTake(t, c, 2)
			lpExpectLine(t, f, lines[0], lpMsgStarting, false, map[string]string{"attempt": "5/5"})
			lpExpectLine(t, f, lines[1], lpMsgStalled, true, map[string]string{
				"attempts": "5", "lastError": lpErrTransient.Error()})
			if v, ok := lines[1].kv["attempts"].(int32); !ok || v != 5 {
				t.Errorf("stalled attempts = %#v, want int32(5)", lines[1].kv["attempts"])
			}
			if lines[1].has("nextAttemptAt") {
				t.Errorf("stalled line must not announce a next attempt: %v", lines[1].kv)
			}
			if !errors.Is(lines[1].err, lpErrTransient) {
				t.Errorf("stalled line error = %v, want the APIM error", lines[1].err)
			}
			if out.Phase != phaseStalled || !out.Result.IsZero() {
				t.Errorf("outcome = %+v, want Stalled without requeue", out)
			}

			// Held: later reconciles say why nothing happens, at info level.
			clock.advance(24 * time.Hour)
			proceed, res = f.begin(p, c).gate(*f.st, false)
			lines = lpTake(t, c, 1)
			lpExpectLine(t, f, lines[0], lpMsgHeld, false, map[string]string{
				"attempts": "5", "retryAnnotation": "apim.operator.io/retry"})
			if proceed || !res.IsZero() {
				t.Errorf("gate = %v, %+v; a stalled resource must not write or requeue", proceed, res)
			}

			// The retry annotation clears the failures; a permanent error is rejected.
			f.obj.SetAnnotations(map[string]string{retryAnnotation: "r1"})
			w = f.begin(p, c)
			if proceed, _ := w.gate(*f.st, false); !proceed {
				t.Fatal("a new retry annotation value must let the write through")
			}
			w.starting()
			out = w.failed(f.st, lpErrPermanent)
			lines = lpTake(t, c, 3)
			lpExpectLine(t, f, lines[0], lpMsgReset, false, map[string]string{
				"reason": "apim.operator.io/retry annotation set", "previousFailures": "5"})
			lpExpectLine(t, f, lines[1], lpMsgStarting, false, map[string]string{"attempt": "1/5"})
			lpExpectLine(t, f, lines[2], lpMsgRejected, true, map[string]string{"class": "permanent", "attempt": "1/5"})
			if lines[2].has("nextAttemptAt") {
				t.Errorf("rejected line must not announce a next attempt: %v", lines[2].kv)
			}
			if !errors.Is(lines[2].err, lpErrPermanent) {
				t.Errorf("rejected line error = %v, want the APIM error", lines[2].err)
			}
			if out.Phase != phaseInvalid || !out.Result.IsZero() {
				t.Errorf("outcome = %+v, want Invalid without requeue", out)
			}

			// Invalid with the same annotation value: held, no reset line.
			f.begin(p, c).gate(*f.st, false)
			lines = lpTake(t, c, 1)
			lpExpectLine(t, f, lines[0], lpMsgHeld, false, map[string]string{"attempts": "1"})

			// A spec change clears the failures and the write succeeds.
			w = f.begin(p, c)
			if proceed, _ := w.gate(*f.st, true); !proceed {
				t.Fatal("a spec change must let the write through")
			}
			w.starting()
			w.succeeded(f.st)
			lines = lpTake(t, c, 3)
			lpExpectLine(t, f, lines[0], lpMsgReset, false, map[string]string{"reason": "spec changed", "previousFailures": "1"})
			lpExpectLine(t, f, lines[1], lpMsgStarting, false, map[string]string{"attempt": "1/5"})
			lpExpectLine(t, f, lines[2], lpMsgSucceeded, false, map[string]string{"attempt": "1/5"})

			// Healthy and unchanged: the gate is silent.
			if proceed, _ := f.begin(p, c).gate(*f.st, false); !proceed {
				t.Error("a healthy resource must be allowed to write")
			}
			lpTake(t, c, 0)
		})
	}
}

// TestLPStartingCarriesStepKeys: the deployment adds the step (and size, products, tags)
// to each start line after the attempt.
func TestLPStartingCarriesStepKeys(t *testing.T) {
	f := lpFixtures()[0]
	c := &lpCapture{}
	w := f.begin(lpPolicy(&lpClock{t: lpStart}), c)
	w.gate(*f.st, false)
	w.starting("step", "import API", "bytes", 1750000)
	w.starting("step", "assign products", "productIDs", []string{"p1", "p2"})
	lines := lpTake(t, c, 2)
	lpExpectLine(t, f, lines[0], lpMsgStarting, false, map[string]string{"attempt": "1/5", "step": "import API", "bytes": "1750000"})
	lpExpectLine(t, f, lines[1], lpMsgStarting, false, map[string]string{"step": "assign products", "productIDs": "[p1 p2]"})
	if got := lines[0].keys[len(lines[0].keys)-3:]; strings.Join(got, ",") != "attempt,step,bytes" {
		t.Errorf("trailing keys = %v, want attempt, step, bytes", got)
	}
}

// TestLPSuccessAfterFailuresLogsTheAttempt: a write that succeeds on attempt 3 says so.
func TestLPSuccessAfterFailuresLogsTheAttempt(t *testing.T) {
	for _, f := range lpFixtures() {
		t.Run(f.kind, func(t *testing.T) {
			clock := &lpClock{t: lpStart}
			p := lpPolicy(clock)
			c := &lpCapture{}
			for i := 0; i < 2; i++ {
				w := f.begin(p, c)
				w.gate(*f.st, false)
				w.failed(f.st, lpErrTransient)
				next, _ := time.Parse(time.RFC3339, f.st.NextAttemptAt)
				clock.set(next)
			}
			c.reset()
			w := f.begin(p, c)
			w.gate(*f.st, false)
			w.succeeded(f.st)
			lines := lpTake(t, c, 1)
			lpExpectLine(t, f, lines[0], lpMsgSucceeded, false, map[string]string{"attempt": "3/5"})
			if f.st.ConsecutiveFailures != 0 || f.st.NextAttemptAt != "" {
				t.Errorf("status after success = %+v, want cleared", *f.st)
			}
		})
	}
}

// TestLPFailureLineClassPerError: each kind of error lands on the right line: rejected
// (Invalid) for what APIM will never accept, failed (Backoff) for everything else.
func TestLPFailureLineClassPerError(t *testing.T) {
	apimErr := func(method string, status int, code string) error {
		return &apim.Error{Operation: "write", Method: method, StatusCode: status, Code: code, Message: "m"}
	}
	cases := []struct {
		name      string
		err       error
		wantMsg   string
		wantClass string
	}{
		{"400 on PUT", apimErr(http.MethodPut, 400, "ValidationError"), lpMsgRejected, "permanent"},
		{"401 on PUT", apimErr(http.MethodPut, 401, "AuthenticationFailed"), lpMsgRejected, "permanent"},
		{"403 on PATCH", apimErr(http.MethodPatch, 403, "LinkedAuthorizationFailed"), lpMsgRejected, "permanent"},
		{"404 on PUT", apimErr(http.MethodPut, 404, "ResourceNotFound"), lpMsgRejected, "permanent"},
		{"404 on DELETE", apimErr(http.MethodDelete, 404, "ResourceNotFound"), lpMsgRejected, "permanent"},
		{"404 on GET", apimErr(http.MethodGet, 404, "ResourceNotFound"), lpMsgFailed, "transient"},
		{"409", apimErr(http.MethodPut, 409, "Conflict"), lpMsgFailed, "transient"},
		{"412", apimErr(http.MethodPut, 412, "PreconditionFailed"), lpMsgFailed, "transient"},
		{"422 management API timed out", apimErr(http.MethodPut, 422, "ManagementApiRequestFailed"), lpMsgFailed, "transient"},
		{"429", apimErr(http.MethodPut, 429, "TooManyRequests"), lpMsgFailed, "transient"},
		{"500", apimErr(http.MethodPut, 500, "InternalServerError"), lpMsgFailed, "transient"},
		{"503", apimErr(http.MethodPut, 503, ""), lpMsgFailed, "transient"},
		{"400 with a transient Azure code", apimErr(http.MethodPut, 400, "PreconditionFailed"), lpMsgFailed, "transient"},
		{"400 with a transient detail code", &apim.Error{Operation: "w", Method: http.MethodPut, StatusCode: 400,
			Code: "ValidationError", DetailCode: "Timeout"}, lpMsgFailed, "transient"},
		{"async DeadOperationMonitor", &apim.Error{Operation: "import API", Code: "InternalServerError",
			Message: "DeadOperationMonitor", Err: apim.ErrAsyncOperationFailed}, lpMsgFailed, "transient"},
		{"import wait timeout", &apim.Error{Operation: "import API", Err: apim.ErrImportWaitTimeout}, lpMsgFailed, "transient"},
		{"bare wait timeout", fmt.Errorf("waiting: %w", apim.ErrImportWaitTimeout), lpMsgFailed, "transient"},
		{"context deadline", context.DeadlineExceeded, lpMsgFailed, "transient"},
		{"transport error", &url.Error{Op: "Put", URL: "https://management.azure.com", Err: errors.New("connection reset")},
			lpMsgFailed, "transient"},
		{"unknown error", errors.New("something odd"), lpMsgFailed, "transient"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := lpFixtures()[2]
			c := &lpCapture{}
			w := f.begin(lpPolicy(&lpClock{t: lpStart}), c)
			w.gate(*f.st, false)
			out := w.failed(f.st, tc.err)
			lines := lpTake(t, c, 1)
			want := map[string]string{"class": tc.wantClass, "attempt": "1/5"}
			if tc.wantMsg == lpMsgFailed {
				want["nextAttemptAt"] = "2026-09-25T10:01:00Z"
			}
			lpExpectLine(t, f, lines[0], tc.wantMsg, true, want)
			if lines[0].err != tc.err {
				t.Errorf("logged error = %v, want %v", lines[0].err, tc.err)
			}
			if string(out.Class) != tc.wantClass {
				t.Errorf("outcome class = %s, want %s", out.Class, tc.wantClass)
			}
		})
	}
}

// TestLPPermanentAfterTransientIsRejectedNotStalled: a permanent error on the last
// attempt is still reported as rejected, with that attempt's number.
func TestLPPermanentAfterTransientIsRejectedNotStalled(t *testing.T) {
	f := lpFixtures()[1]
	clock := &lpClock{t: lpStart}
	p := lpPolicy(clock)
	c := &lpCapture{}
	for i := 0; i < 4; i++ {
		w := f.begin(p, c)
		w.gate(*f.st, false)
		w.failed(f.st, lpErrTransient)
		next, _ := time.Parse(time.RFC3339, f.st.NextAttemptAt)
		clock.set(next)
	}
	c.reset()
	w := f.begin(p, c)
	w.gate(*f.st, false)
	w.failed(f.st, lpErrPermanent)
	lines := lpTake(t, c, 1)
	lpExpectLine(t, f, lines[0], lpMsgRejected, true, map[string]string{"class": "permanent", "attempt": "5/5"})
}

// TestLPStalledLineIsStable: the stalled line reads the same whatever the kind, the
// error or the attempt limit; only attempts and lastError vary.
func TestLPStalledLineIsStable(t *testing.T) {
	errs := []error{
		lpErrTransient,
		&apim.Error{Operation: "import API", Err: apim.ErrImportWaitTimeout},
		&apim.Error{Operation: "assign API to product p1", Method: http.MethodPut, StatusCode: 422,
			Code: "ManagementApiRequestFailed", Message: "Management API timed out"},
		errors.New("dial tcp: lookup management.azure.com: no such host"),
	}
	for _, maxAttempts := range []int32{1, 2, 5} {
		for i, err := range errs {
			for _, f := range lpFixtures() {
				t.Run(fmt.Sprintf("%s/max%d/err%d", f.kind, maxAttempts, i), func(t *testing.T) {
					clock := &lpClock{t: lpStart}
					p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: maxAttempts, Now: clock.now}
					c := &lpCapture{}
					for n := int32(1); n <= maxAttempts; n++ {
						w := f.begin(p, c)
						if proceed, _ := w.gate(*f.st, false); !proceed {
							t.Fatalf("attempt %d was not let through", n)
						}
						w.failed(f.st, err)
						if f.st.NextAttemptAt != "" {
							next, _ := time.Parse(time.RFC3339, f.st.NextAttemptAt)
							clock.set(next)
						}
					}
					stalled := lpWithMsg(c.all(), "🛑 APIM write stalled")
					if len(stalled) != 1 {
						t.Fatalf("stalled lines = %d (%q), want exactly 1", len(stalled), lpMsgs(c.all()))
					}
					lpExpectLine(t, f, stalled[0], lpMsgStalled, true, map[string]string{
						"attempts": fmt.Sprint(maxAttempts), "lastError": err.Error()})
					if failed := lpWithMsg(c.all(), lpMsgFailed); len(failed) != int(maxAttempts)-1 {
						t.Errorf("failed lines = %d, want %d", len(failed), maxAttempts-1)
					}
				})
			}
		}
	}
}

// TestLPFailedLineNextAttemptWithJitter: with jitter the logged nextAttemptAt is the one
// written to the status, rounded up to the second, and RequeueAfter lands on it.
func TestLPFailedLineNextAttemptWithJitter(t *testing.T) {
	cases := []struct {
		name     string
		random   float64
		failures int
		wantNext string
	}{
		{"low edge, first failure", 0, 1, "2026-09-25T10:00:48Z"},         // 60 s - 20 %
		{"middle, first failure", 0.5, 1, "2026-09-25T10:01:00Z"},         // 60 s
		{"high edge, first failure", 0.999999, 1, "2026-09-25T10:01:12Z"}, // just under 72 s, rounded up
		{"low edge, fourth failure", 0, 4, "2026-09-25T10:06:24Z"},        // 480 s - 20 %
		{"quarter, fourth failure", 0.25, 4, "2026-09-25T10:07:12Z"},      // 480 s - 10 %
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := lpFixtures()[3]
			clock := &lpClock{t: lpStart}
			p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Jitter: 0.2,
				Now: clock.now, Random: func() float64 { return tc.random }}
			if tc.failures > 1 {
				// Earlier failures with their backoff already over.
				f.st.ConsecutiveFailures = int32(tc.failures - 1)
				f.st.NextAttemptAt = lpStart.Add(-time.Second).Format(time.RFC3339)
			}
			c := &lpCapture{}
			w := f.begin(p, c)
			w.gate(*f.st, false)
			out := w.failed(f.st, lpErrTransient)
			lines := lpTake(t, c, 1)
			lpExpectLine(t, f, lines[0], lpMsgFailed, true, map[string]string{
				"attempt": fmt.Sprintf("%d/5", tc.failures), "nextAttemptAt": tc.wantNext})
			if f.st.NextAttemptAt != tc.wantNext {
				t.Errorf("status nextAttemptAt = %s, want %s", f.st.NextAttemptAt, tc.wantNext)
			}
			next, _ := time.Parse(time.RFC3339, tc.wantNext)
			if got := lpStart.Add(out.Result.RequeueAfter); !got.Equal(next) {
				t.Errorf("requeue lands at %s, want %s", got.Format(time.RFC3339Nano), tc.wantNext)
			}
			if !strings.Contains(out.Message, "next attempt at "+tc.wantNext) {
				t.Errorf("status message %q does not name the next attempt", out.Message)
			}
		})
	}
}

// TestLPBackingOffSkipLine: the skip line's remaining time and the requeue match, right
// up to nextAttemptAt; at and after it the gate is silent and lets the write through.
func TestLPBackingOffSkipLine(t *testing.T) {
	cases := []struct {
		name          string
		now           time.Time
		wantSkip      bool
		wantRemaining string
		wantRequeue   time.Duration
	}{
		{"just failed", lpStart, true, "8m0s", 8 * time.Minute},
		{"half way", lpStart.Add(4 * time.Minute), true, "4m0s", 4 * time.Minute},
		{"one second left", lpStart.Add(8*time.Minute - time.Second), true, "1s", time.Second},
		{"sub-second left", lpStart.Add(8*time.Minute - 300*time.Millisecond), true, "0s", 300 * time.Millisecond},
		{"exactly due", lpStart.Add(8 * time.Minute), false, "", 0},
		{"overdue", lpStart.Add(time.Hour), false, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := lpFixtures()[0]
			*f.st = apimv1.RetryStatus{ConsecutiveFailures: 3, NextAttemptAt: "2026-09-25T10:08:00Z"}
			clock := &lpClock{t: tc.now}
			c := &lpCapture{}
			proceed, res := f.begin(lpPolicy(clock), c).gate(*f.st, false)
			if !tc.wantSkip {
				lpTake(t, c, 0)
				if !proceed || !res.IsZero() {
					t.Errorf("gate = %v, %+v; want proceed", proceed, res)
				}
				return
			}
			lines := lpTake(t, c, 1)
			lpExpectLine(t, f, lines[0], lpMsgBackingOff, false, map[string]string{
				"attempt": "4/5", "nextAttemptAt": "2026-09-25T10:08:00Z", "remaining": tc.wantRemaining})
			if proceed || res.RequeueAfter != tc.wantRequeue {
				t.Errorf("gate = %v, %+v; want skip with RequeueAfter %s", proceed, res, tc.wantRequeue)
			}
		})
	}
}

// TestLPResetLine: the 🔁 line appears only when there was something to clear, and names
// why; spec change wins over the annotation when both happen at once only in reason
// text, never in behaviour.
func TestLPResetLine(t *testing.T) {
	cases := []struct {
		name        string
		st          apimv1.RetryStatus
		annotation  string
		specChanged bool
		wantLine    bool
		wantReason  string
	}{
		{"spec change on a healthy resource", apimv1.RetryStatus{}, "", true, false, ""},
		{"annotation on a healthy resource", apimv1.RetryStatus{}, "a", false, false, ""},
		{"spec change while backing off", apimv1.RetryStatus{ConsecutiveFailures: 2, NextAttemptAt: "2026-09-25T10:02:00Z"},
			"", true, true, "spec changed"},
		{"annotation while stalled", apimv1.RetryStatus{ConsecutiveFailures: 5}, "a", false, true,
			"apim.operator.io/retry annotation set"},
		{"annotation and spec change together", apimv1.RetryStatus{ConsecutiveFailures: 5}, "a", true, true,
			"apim.operator.io/retry annotation set"},
		{"same annotation value again", apimv1.RetryStatus{ConsecutiveFailures: 5, LastRetryAnnotation: "a"}, "a", false, false, ""},
		{"annotation removed", apimv1.RetryStatus{ConsecutiveFailures: 5, LastRetryAnnotation: "a"}, "", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := lpFixtures()[2]
			*f.st = tc.st
			if tc.annotation != "" {
				f.obj.SetAnnotations(map[string]string{retryAnnotation: tc.annotation})
			}
			c := &lpCapture{}
			f.begin(lpPolicy(&lpClock{t: lpStart}), c).gate(*f.st, tc.specChanged)
			resets := lpWithMsg(c.all(), lpMsgReset)
			if !tc.wantLine {
				if len(resets) != 0 {
					t.Errorf("unexpected reset line: %+v", resets[0].kv)
				}
				return
			}
			if len(resets) != 1 {
				t.Fatalf("lines = %q, want one reset line", lpMsgs(c.all()))
			}
			lpExpectLine(t, f, resets[0], lpMsgReset, false, map[string]string{
				"reason": tc.wantReason, "previousFailures": fmt.Sprint(tc.st.ConsecutiveFailures)})
		})
	}
}

// TestLPMessagesAreConstant: every message logged by the write handling is one of the
// agreed literals, with no formatting verbs and no resource data baked in, so log
// searches and monitors can match on the message alone.
func TestLPMessagesAreConstant(t *testing.T) {
	allowed := map[string]bool{lpMsgStarting: true, lpMsgSucceeded: true, lpMsgFailed: true, lpMsgRejected: true,
		lpMsgBackingOff: true, lpMsgHeld: true, lpMsgStalled: true, lpMsgReset: true}
	for _, f := range lpFixtures() {
		clock := &lpClock{t: lpStart}
		p := lpPolicy(clock)
		c := &lpCapture{}
		for i := 0; i < 8; i++ {
			w := f.begin(p, c)
			if proceed, _ := w.gate(*f.st, false); proceed {
				w.starting()
				w.failed(f.st, lpErrTransient)
			}
			clock.advance(31 * time.Minute)
		}
		f.obj.SetAnnotations(map[string]string{retryAnnotation: "x"})
		w := f.begin(p, c)
		w.gate(*f.st, false)
		w.succeeded(f.st)
		for _, l := range c.all() {
			if !allowed[l.msg] {
				t.Errorf("%s logged an unexpected message %q", f.kind, l.msg)
			}
			if strings.ContainsAny(l.msg, "%") || strings.Contains(l.msg, "res-a") || strings.Contains(l.msg, "team-a") {
				t.Errorf("%s: message %q carries data that belongs in keys", f.kind, l.msg)
			}
		}
	}
}

// TestLPStalledLineAsDatadogSeesIt renders the stalled and failed lines through zap
// configured as cmd/main.go configures it (JSON, Development false) and checks the
// fields a Datadog log monitor on "🛑 APIM write stalled" would read.
func TestLPStalledLineAsDatadogSeesIt(t *testing.T) {
	for _, f := range lpFixtures() {
		t.Run(f.kind, func(t *testing.T) {
			out := &lpWriter{}
			logger := zap.New(zap.UseFlagOptions(&zap.Options{
				Development: false, StacktraceLevel: zapcore.DPanicLevel, DestWriter: out,
			}))
			clock := &lpClock{t: lpStart}
			p := lpPolicy(clock)
			for n := 1; n <= 5; n++ {
				w := p.begin(logger, f.kind, f.obj, f.ids...)
				w.gate(*f.st, false)
				w.starting()
				w.failed(f.st, lpErrTransient)
				if f.st.NextAttemptAt != "" {
					next, _ := time.Parse(time.RFC3339, f.st.NextAttemptAt)
					clock.set(next)
				}
			}
			raws := strings.Split(strings.TrimSpace(out.String()), "\n")
			records := make([]map[string]any, 0, len(raws))
			for _, raw := range raws {
				var rec map[string]any
				if err := json.Unmarshal([]byte(raw), &rec); err != nil {
					t.Fatalf("not a JSON log line: %q: %v", raw, err)
				}
				records = append(records, rec)
			}
			var stalled, failed []map[string]any
			for _, rec := range records {
				switch rec["msg"] {
				case "🛑 APIM write stalled":
					stalled = append(stalled, rec)
				case lpMsgFailed:
					failed = append(failed, rec)
				}
			}
			if len(stalled) != 1 || len(failed) != 4 {
				t.Fatalf("stalled=%d failed=%d lines, want 1 and 4: %v", len(stalled), len(failed), records)
			}
			s := stalled[0]
			want := map[string]any{"level": "error", "kind": f.kind, "namespace": "team-a", "name": "res-a",
				"attempts": float64(5), "lastError": lpErrTransient.Error(), "error": lpErrTransient.Error()}
			for i := 0; i+1 < len(f.ids); i += 2 {
				want[f.ids[i].(string)] = f.ids[i+1]
			}
			for k, v := range want {
				if s[k] != v {
					t.Errorf("stalled %s = %#v, want %#v", k, s[k], v)
				}
			}
			if _, ok := s["stacktrace"]; ok {
				t.Error("the stalled line must not carry a stack trace at the production stacktrace level")
			}
			fl := failed[3]
			if fl["attempt"] != "4/5" || fl["class"] != "transient" || fl["nextAttemptAt"] != "2026-09-25T10:15:00Z" {
				t.Errorf("last failed line = %v", fl)
			}
		})
	}
}

// --- Predicates -----------------------------------------------------------------------

// lpPredicateKind is one kind with its update predicate exactly as SetupWithManager
// installs it.
type lpPredicateKind struct {
	kind      string
	predicate predicate.Predicate
	// newObj returns a fresh object at generation 3 with a Stalled status.
	newObj func() client.Object
	// changeSpec changes a spec field the controller writes to APIM and bumps the
	// generation, as the API server would.
	changeSpec func(client.Object)
	// changeOtherSpec changes a spec field that does not reach APIM (deletionPolicy or
	// revision) and bumps the generation.
	changeOtherSpec func(client.Object)
	// changeStatus changes only the status.
	changeStatus func(client.Object)
}

func lpPredicateKinds() []lpPredicateKind {
	meta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: "res-a", Namespace: "team-a", Generation: 3, ResourceVersion: "100"}
	}
	stalled := apimv1.RetryStatus{ConsecutiveFailures: 5}
	return []lpPredicateKind{
		{
			kind:      "APIMAPIDeployment",
			predicate: apimAPIDeploymentPredicate(),
			newObj: func() client.Object {
				return &apimv1.APIMAPIDeployment{ObjectMeta: meta(),
					Spec:   apimv1.APIMAPIDeploymentSpec{APIID: "api-1", ServiceURL: "https://a.example.net"},
					Status: apimv1.APIMAPIDeploymentStatus{Phase: phaseStalled, RetryStatus: stalled}}
			},
			changeSpec: func(o client.Object) {
				d := o.(*apimv1.APIMAPIDeployment)
				d.Spec.ServiceURL = "https://b.example.net"
				d.Generation++
			},
			changeOtherSpec: func(o client.Object) {
				d := o.(*apimv1.APIMAPIDeployment)
				d.Spec.Revision = "2"
				d.Generation++
			},
			changeStatus: func(o client.Object) {
				d := o.(*apimv1.APIMAPIDeployment)
				d.Status.Phase = phaseBackoff
				d.Status.LastAttemptAt = "2026-09-25T10:00:05Z"
			},
		},
		{
			kind:      "APIMProduct",
			predicate: predicate.And(logRetainedOnDelete(), specOrDeletionChanged()),
			newObj: func() client.Object {
				return &apimv1.APIMProduct{ObjectMeta: meta(), Spec: apimv1.APIMProductSpec{ProductID: "prod-1", DisplayName: "A"},
					Status: apimv1.APIMProductStatus{Phase: phaseStalled, ObservedGeneration: 3, RetryStatus: stalled}}
			},
			changeSpec: func(o client.Object) {
				p := o.(*apimv1.APIMProduct)
				p.Spec.DisplayName = "B"
				p.Generation++
			},
			changeOtherSpec: func(o client.Object) {
				p := o.(*apimv1.APIMProduct)
				p.Spec.DeletionPolicy = apimv1.DeletionPolicyDelete
				p.Generation++
			},
			changeStatus: func(o client.Object) {
				p := o.(*apimv1.APIMProduct)
				p.Status.Phase = phaseBackoff
				p.Status.Message = "x"
			},
		},
		{
			kind:      "APIMTag",
			predicate: specOrDeletionChanged(),
			newObj: func() client.Object {
				return &apimv1.APIMTag{ObjectMeta: meta(), Spec: apimv1.APIMTagSpec{TagID: "tag-1", DisplayName: "A"},
					Status: apimv1.APIMTagStatus{Phase: phaseStalled, ObservedGeneration: 3, RetryStatus: stalled}}
			},
			changeSpec: func(o client.Object) {
				tg := o.(*apimv1.APIMTag)
				tg.Spec.DisplayName = "B"
				tg.Generation++
			},
			changeOtherSpec: func(o client.Object) {
				tg := o.(*apimv1.APIMTag)
				tg.Spec.DeletionPolicy = apimv1.DeletionPolicyDelete
				tg.Generation++
			},
			changeStatus: func(o client.Object) {
				tg := o.(*apimv1.APIMTag)
				tg.Status.Phase = phaseBackoff
				tg.Status.Message = "x"
			},
		},
		{
			kind: "APIMInboundPolicy",
			// SetupWithManager builds this predicate.Funcs inline around
			// apimInboundPolicyUpdateFilter; this is the same shape.
			predicate: predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return true },
				UpdateFunc:  apimInboundPolicyUpdateFilter(),
				DeleteFunc:  func(event.DeleteEvent) bool { return false },
				GenericFunc: func(event.GenericEvent) bool { return false },
			},
			newObj: func() client.Object {
				return &apimv1.APIMInboundPolicy{ObjectMeta: meta(),
					Spec:   apimv1.APIMInboundPolicySpec{APIMService: "svc", APIID: "api-1", PolicyContent: "<a/>"},
					Status: apimv1.APIMInboundPolicyStatus{Phase: phaseStalled, ObservedGeneration: 3, RetryStatus: stalled}}
			},
			changeSpec: func(o client.Object) {
				p := o.(*apimv1.APIMInboundPolicy)
				p.Spec.PolicyContent = "<b/>"
				p.Generation++
			},
			changeOtherSpec: func(o client.Object) {
				p := o.(*apimv1.APIMInboundPolicy)
				p.Spec.DeletionPolicy = apimv1.DeletionPolicyDelete
				p.Generation++
			},
			changeStatus: func(o client.Object) {
				p := o.(*apimv1.APIMInboundPolicy)
				p.Status.Phase = phaseBackoff
				p.Status.Message = "x"
			},
		},
	}
}

// lpAnnotate returns a mutation that sets (value != nil) or removes (nil) annotation key.
func lpAnnotate(key string, value *string) func(client.Object) {
	return func(o client.Object) {
		a := map[string]string{}
		for k, v := range o.GetAnnotations() {
			a[k] = v
		}
		if value == nil {
			delete(a, key)
		} else {
			a[key] = *value
		}
		o.SetAnnotations(a)
	}
}

func lpStr(s string) *string { return &s }

// lpUpdateCase is one update event: base prepares the old object, change turns a copy of
// it into the new one. want is per kind; kinds left out use wantDefault.
type lpUpdateCase struct {
	name        string
	base        []func(client.Object)
	change      func(k lpPredicateKind) []func(client.Object)
	wantDefault bool
	want        map[string]bool
}

func lpUpdateCases() []lpUpdateCase {
	retry := func(v string) func(client.Object) { return lpAnnotate(retryAnnotation, lpStr(v)) }
	noRetry := lpAnnotate(retryAnnotation, nil)
	other := func(v string) func(client.Object) { return lpAnnotate("example.com/note", lpStr(v)) }
	fixed := func(m ...func(client.Object)) func(lpPredicateKind) []func(client.Object) {
		return func(lpPredicateKind) []func(client.Object) { return m }
	}
	bumpRV := func(o client.Object) { o.SetResourceVersion("101") }
	return []lpUpdateCase{
		// The retry annotation, a metadata-only change, must reach Reconcile.
		{name: "retry annotation added", change: fixed(retry("1"), bumpRV), wantDefault: true},
		{name: "retry annotation added next to others", base: []func(client.Object){other("x")},
			change: fixed(retry("1"), bumpRV), wantDefault: true},
		{name: "retry annotation value changed", base: []func(client.Object){retry("1")},
			change: fixed(retry("2"), bumpRV), wantDefault: true},
		{name: "retry annotation value changed to a timestamp", base: []func(client.Object){retry("1759312800")},
			change: fixed(retry("1759312801"), bumpRV), wantDefault: true},
		{name: "retry annotation removed", base: []func(client.Object){retry("1")},
			change: fixed(noRetry, bumpRV), wantDefault: true},
		{name: "retry annotation changed with an unrelated one", base: []func(client.Object){retry("1"), other("x")},
			change: fixed(retry("2"), other("y"), bumpRV), wantDefault: true},
		{name: "retry annotation changed with a status change",
			base: []func(client.Object){retry("1")},
			change: func(k lpPredicateKind) []func(client.Object) {
				return []func(client.Object){retry("2"), k.changeStatus, bumpRV}
			},
			wantDefault: true},
		{name: "retry annotation changed while the status catches up",
			base: []func(client.Object){retry("1")},
			change: fixed(retry("2"), func(o client.Object) {
				lpRetryStatusOf(o).LastRetryAnnotation = "1"
			}, bumpRV), wantDefault: true},

		// Nothing to act on.
		{name: "retry annotation unchanged", base: []func(client.Object){retry("1")},
			change: fixed(retry("1"), bumpRV), wantDefault: false},
		// An empty value is not a retry request; gate ignores it too.
		{name: "retry annotation added with an empty value", change: fixed(retry(""), bumpRV), wantDefault: false},
		{name: "unrelated annotation added", change: fixed(other("x"), bumpRV), wantDefault: false},
		{name: "unrelated annotation changed", base: []func(client.Object){other("x")},
			change: fixed(other("y"), bumpRV), wantDefault: false},
		{name: "unrelated annotation removed", base: []func(client.Object){other("x")},
			change: fixed(lpAnnotate("example.com/note", nil), bumpRV), wantDefault: false},
		{name: "unrelated annotation changed next to an unchanged retry annotation",
			base:   []func(client.Object){retry("1"), other("x")},
			change: fixed(other("y"), bumpRV), wantDefault: false},
		{name: "kubectl last-applied-configuration changed",
			change:      fixed(lpAnnotate("kubectl.kubernetes.io/last-applied-configuration", lpStr(`{"spec":{}}`)), bumpRV),
			wantDefault: false},
		{name: "look-alike annotation key with a suffix",
			change: fixed(lpAnnotate("apim.operator.io/retry-at", lpStr("1")), bumpRV), wantDefault: false},
		{name: "look-alike annotation key in another domain",
			change: fixed(lpAnnotate("example.com/retry", lpStr("1")), bumpRV), wantDefault: false},
		{name: "retry annotation key in different case",
			change: fixed(lpAnnotate("apim.operator.io/Retry", lpStr("1")), bumpRV), wantDefault: false},
		{name: "label added", change: fixed(func(o client.Object) {
			o.SetLabels(map[string]string{"team": "a"})
		}, bumpRV), wantDefault: false},
		{name: "finalizer added", change: fixed(func(o client.Object) {
			o.SetFinalizers([]string{"example.com/f"})
		}, bumpRV), wantDefault: false},
		{name: "resourceVersion and managedFields only", change: fixed(bumpRV, func(o client.Object) {
			o.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate}})
		}), wantDefault: false},

		// Status-only updates, including the ones the retry handling itself writes.
		{name: "status phase and message", change: func(k lpPredicateKind) []func(client.Object) {
			return []func(client.Object){k.changeStatus, bumpRV}
		}, wantDefault: false},
		{name: "status backoff fields", change: fixed(func(o client.Object) {
			st := lpRetryStatusOf(o)
			st.ConsecutiveFailures = 2
			st.NextAttemptAt = "2026-09-25T10:02:00Z"
		}, bumpRV), wantDefault: false},
		{name: "status failures cleared", change: fixed(func(o client.Object) {
			*lpRetryStatusOf(o) = apimv1.RetryStatus{}
		}, bumpRV), wantDefault: false},
		{name: "status lastRetryAnnotation recorded", base: []func(client.Object){retry("1")},
			change:      fixed(func(o client.Object) { lpRetryStatusOf(o).LastRetryAnnotation = "1" }, bumpRV),
			wantDefault: false},

		// Spec changes bump the generation and must reconcile.
		{name: "spec change", change: func(k lpPredicateKind) []func(client.Object) {
			return []func(client.Object){k.changeSpec, bumpRV}
		}, wantDefault: true},
		{name: "spec change with an unchanged retry annotation", base: []func(client.Object){retry("1")},
			change: func(k lpPredicateKind) []func(client.Object) {
				return []func(client.Object){k.changeSpec, bumpRV}
			}, wantDefault: true},
		{name: "spec change with a status change", change: func(k lpPredicateKind) []func(client.Object) {
			return []func(client.Object){k.changeSpec, k.changeStatus, bumpRV}
		}, wantDefault: true},
		// Without it status.observedGeneration falls behind, and the next reconcile for
		// another reason would read that as a spec change and reset a Stalled resource.
		{name: "generation bump from a spec field that does not reach APIM",
			change: func(k lpPredicateKind) []func(client.Object) {
				return []func(client.Object){k.changeOtherSpec, bumpRV}
			}, wantDefault: true},

		// The deployment's own signal annotations still work, and only for it.
		{name: "replicaset signal annotation changed",
			base:   []func(client.Object){lpAnnotate(apimDeploymentSignalAnnotation, lpStr("t1"))},
			change: fixed(lpAnnotate(apimDeploymentSignalAnnotation, lpStr("t2")), bumpRV), wantDefault: false,
			want: map[string]bool{"APIMAPIDeployment": true}},
	}
}

// lpRetryStatusOf returns the embedded retry status of any of the four kinds.
func lpRetryStatusOf(o client.Object) *apimv1.RetryStatus {
	switch v := o.(type) {
	case *apimv1.APIMAPIDeployment:
		return &v.Status.RetryStatus
	case *apimv1.APIMProduct:
		return &v.Status.RetryStatus
	case *apimv1.APIMTag:
		return &v.Status.RetryStatus
	case *apimv1.APIMInboundPolicy:
		return &v.Status.RetryStatus
	}
	panic(fmt.Sprintf("no retry status on %T", o))
}

// TestLPUpdatePredicatesAllKinds checks every update case against the predicate of every
// kind that writes to APIM.
func TestLPUpdatePredicatesAllKinds(t *testing.T) {
	for _, k := range lpPredicateKinds() {
		for _, tc := range lpUpdateCases() {
			t.Run(k.kind+"/"+tc.name, func(t *testing.T) {
				old := k.newObj()
				for _, m := range tc.base {
					m(old)
				}
				updated := old.DeepCopyObject().(client.Object)
				for _, m := range tc.change(k) {
					m(updated)
				}
				want := tc.wantDefault
				if w, ok := tc.want[k.kind]; ok {
					want = w
				}
				got := k.predicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated})
				if got != want {
					t.Errorf("Update() = %v, want %v", got, want)
				}
			})
		}
	}
}

// TestLPPredicatesCreateAndNil: every kind reconciles a new object, and an update event
// missing either object is dropped rather than panicking.
func TestLPPredicatesCreateAndNil(t *testing.T) {
	for _, k := range lpPredicateKinds() {
		t.Run(k.kind, func(t *testing.T) {
			obj := k.newObj()
			if !k.predicate.Create(event.CreateEvent{Object: obj}) {
				t.Error("create must reconcile")
			}
			withRetry := k.newObj()
			withRetry.SetAnnotations(map[string]string{retryAnnotation: "1"})
			if !k.predicate.Create(event.CreateEvent{Object: withRetry}) {
				t.Error("create with the retry annotation must reconcile")
			}
			if k.predicate.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: withRetry}) {
				t.Error("an update without the old object must not reconcile")
			}
			if k.predicate.Update(event.UpdateEvent{ObjectOld: obj, ObjectNew: nil}) {
				t.Error("an update without the new object must not reconcile")
			}
		})
	}
}

// TestLPRetryAnnotationChanged covers the shared helper directly.
func TestLPRetryAnnotationChanged(t *testing.T) {
	obj := func(a map[string]string) client.Object {
		return &apimv1.APIMTag{ObjectMeta: metav1.ObjectMeta{Name: "t", Annotations: a}}
	}
	cases := []struct {
		name     string
		old, new client.Object
		want     bool
	}{
		{"nil annotations on both", obj(nil), obj(nil), false},
		{"nil to empty map", obj(nil), obj(map[string]string{}), false},
		{"added", obj(nil), obj(map[string]string{retryAnnotation: "1"}), true},
		{"changed", obj(map[string]string{retryAnnotation: "1"}), obj(map[string]string{retryAnnotation: "2"}), true},
		{"removed", obj(map[string]string{retryAnnotation: "1"}), obj(nil), true},
		{"set to empty", obj(map[string]string{retryAnnotation: "1"}), obj(map[string]string{retryAnnotation: ""}), true},
		{"unchanged", obj(map[string]string{retryAnnotation: "1"}), obj(map[string]string{retryAnnotation: "1"}), false},
		{"other key", obj(nil), obj(map[string]string{"x": "1"}), false},
		{"old nil", nil, obj(map[string]string{retryAnnotation: "1"}), false},
		{"new nil", obj(map[string]string{retryAnnotation: "1"}), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := event.UpdateEvent{}
			if tc.old != nil {
				e.ObjectOld = tc.old
			}
			if tc.new != nil {
				e.ObjectNew = tc.new
			}
			if got := retryAnnotationChanged(e); got != tc.want {
				t.Errorf("retryAnnotationChanged() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLPDeletionStartReconcilesKindsWithFinalizers: product (which may hold a finalizer
// and delete from APIM) and tag still see the start of a deletion through the shared
// predicate, with the retry annotation changes added to it.
func TestLPDeletionStartReconcilesKindsWithFinalizers(t *testing.T) {
	for _, k := range lpPredicateKinds() {
		if k.kind != "APIMProduct" && k.kind != "APIMTag" {
			continue
		}
		t.Run(k.kind, func(t *testing.T) {
			old := k.newObj()
			deleting := old.DeepCopyObject().(client.Object)
			now := metav1.NewTime(lpStart)
			deleting.SetDeletionTimestamp(&now)
			if !k.predicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: deleting}) {
				t.Error("the start of a deletion must reconcile")
			}
			again := deleting.DeepCopyObject().(client.Object)
			again.SetResourceVersion("102")
			if k.predicate.Update(event.UpdateEvent{ObjectOld: deleting, ObjectNew: again}) {
				t.Error("a no-op update of a deleting object must not reconcile")
			}
		})
	}
}
