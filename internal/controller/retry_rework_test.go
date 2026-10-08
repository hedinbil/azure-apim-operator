package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// errDependency is a write whose parent (the product, tag or API it hangs off) is not in
// APIM yet, as the apim package returns it for a dependent write.
var errDependency = &apim.Error{Operation: "assign API to product p1", Method: http.MethodPut, StatusCode: http.StatusNotFound,
	Code: "ResourceNotFound", Err: apim.ErrDependencyNotFound}

// TestDependencyNotFoundNeverStalls: a write that fails only because another resource is
// not in APIM yet keeps backing off past MaxAttempts. From then on its count stays at
// MaxAttempts-1, so the wait does not use up the attempts of a later failure of another
// kind, and it waits exactly MaxDelay, without jitter.
func TestDependencyNotFoundNeverStalls(t *testing.T) {
	clock := &rcClock{t: rcStart}
	p := rcPolicy(clock)
	p.Jitter = 0.2
	p.Random = func() float64 { return 0.999 } // +20 % wherever jitter applies
	tag := rcTag("")
	st := &tag.Status.RetryStatus

	for i := int32(1); i <= p.MaxAttempts+3; i++ {
		w := p.begin(logr.Discard(), "APIMAPIDeployment", tag)
		if proceed, _ := w.gate(*st, false); !proceed {
			t.Fatalf("attempt %d: gate refused a due write", i)
		}
		out := w.failed(st, fmt.Errorf("step: %w", errDependency))
		if out.Phase != phaseBackoff {
			t.Fatalf("attempt %d: phase = %s, want Backoff: a missing dependency never stalls", i, out.Phase)
		}
		if out.Class != errorClassTransient {
			t.Errorf("attempt %d: class = %s, want transient", i, out.Class)
		}
		wantCount := min(i, p.MaxAttempts-1)
		if st.ConsecutiveFailures != wantCount {
			t.Errorf("attempt %d: consecutiveFailures = %d, want %d", i, st.ConsecutiveFailures, wantCount)
		}
		if st.NextAttemptAt == "" {
			t.Fatalf("attempt %d: Backoff without nextAttemptAt", i)
		}
		delay := out.Result.RequeueAfter
		if i >= p.MaxAttempts {
			if delay != p.MaxDelay {
				t.Errorf("attempt %d: delay = %s, want exactly MaxDelay %s", i, delay, p.MaxDelay)
			}
		} else if delay <= 0 || delay > p.MaxDelay+p.MaxDelay/5+time.Second {
			t.Errorf("attempt %d: delay = %s, out of range", i, delay)
		}
		if want := fmt.Sprintf("attempt %d/%d", wantCount, p.MaxAttempts); !strings.Contains(out.Message, want) {
			t.Errorf("attempt %d: message %q does not say %q", i, out.Message, want)
		}
		clock.advance(delay)
	}

	// A failure of another kind after the wait stalls at once: the wait kept one attempt.
	w := p.begin(logr.Discard(), "APIMAPIDeployment", tag)
	if proceed, _ := w.gate(*st, false); !proceed {
		t.Fatal("gate refused a due write")
	}
	if out := w.failed(st, rcTransientErr); out.Phase != phaseStalled || st.ConsecutiveFailures != p.MaxAttempts {
		t.Errorf("after the dependency wait a transient failure = %s with %d failures, want Stalled with %d",
			out.Phase, st.ConsecutiveFailures, p.MaxAttempts)
	}
}

// TestOtherTransientErrorsStillStall: the dependency exemption is narrow. Every other
// transient error stalls at exactly MaxAttempts, including a 404 that is not about a
// dependency and a dependency 404 followed by other failures.
func TestOtherTransientErrorsStillStall(t *testing.T) {
	for name, errs := range map[string][]error{
		"412 PreconditionFailed":          {rcTransientErr},
		"timed-out import":                {&apim.Error{Operation: "import API", Method: http.MethodGet, Err: apim.ErrImportWaitTimeout}},
		"404 on a read":                   {rcAPIMErr(http.MethodGet, http.StatusNotFound, "ResourceNotFound", "")},
		"import outdated":                 {errPendingImportOutdated},
		"import gone":                     {errPendingImportGone},
		"import not recorded":             {errPendingImportNotRecorded},
		"dependency, then transient ones": {errDependency, errDependency, errDependency, errDependency, errDependency, errDependency, rcTransientErr},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &rcClock{t: rcStart}
			p := rcPolicy(clock)
			tag := rcTag("")
			st := &tag.Status.RetryStatus
			var out writeOutcome
			for i := 0; ; i++ {
				err := errs[min(i, len(errs)-1)]
				w := p.begin(logr.Discard(), "APIMTag", tag)
				if proceed, _ := w.gate(*st, false); !proceed {
					t.Fatalf("attempt %d: gate refused a due write", i+1)
				}
				out = w.failed(st, err)
				if out.Phase != phaseBackoff {
					break
				}
				if i > 20 {
					t.Fatal("never stalled")
				}
				clock.advance(out.Result.RequeueAfter)
			}
			if out.Phase != phaseStalled {
				t.Fatalf("phase = %s, want Stalled", out.Phase)
			}
			if st.ConsecutiveFailures != p.MaxAttempts {
				t.Errorf("stalled after %d failures, want %d", st.ConsecutiveFailures, p.MaxAttempts)
			}
			if st.NextAttemptAt != "" || out.Result.RequeueAfter != 0 {
				t.Errorf("Stalled with nextAttemptAt %q, requeue %s", st.NextAttemptAt, out.Result.RequeueAfter)
			}
		})
	}
}

// TestClassifyReworkedCases: what the rework changed in the classification.
func TestClassifyReworkedCases(t *testing.T) {
	asyncFailed := func(code, detail string) error {
		return &apim.Error{Operation: "import API", Method: http.MethodGet, Code: code, DetailCode: detail,
			Message: "operation status Failed", Err: apim.ErrAsyncOperationFailed}
	}
	for _, tc := range []struct {
		name string
		err  error
		want apimErrorClass
	}{
		{"a document the operator will not send", fmt.Errorf("import: %w", apim.ErrUnsupportedDocument), errorClassPermanent},
		{"async ValidationError", asyncFailed("ValidationError", ""), errorClassPermanent},
		{"async InvalidRequestContent", asyncFailed("InvalidRequestContent", ""), errorClassPermanent},
		{"async BadRequest", asyncFailed("BadRequest", ""), errorClassPermanent},
		{"async detail ValidationError", asyncFailed("Failed", "ValidationError"), errorClassPermanent},
		{"async InternalServerError", asyncFailed("InternalServerError", "DeadOperationMonitor"), errorClassTransient},
		{"async Timeout", asyncFailed("Timeout", ""), errorClassTransient},
		{"async without a code", asyncFailed("", ""), errorClassTransient},
		{"async with a transient code beats a permanent detail", asyncFailed("Timeout", "ValidationError"), errorClassTransient},
		// A permanent async code only counts for an operation result, which has no status.
		{"ValidationError code on a 409", &apim.Error{Method: http.MethodPut, StatusCode: http.StatusConflict,
			Code: "ValidationError", Err: apim.ErrAsyncOperationFailed}, errorClassTransient},
		{"ValidationError without ErrAsyncOperationFailed", &apim.Error{Method: http.MethodGet, Code: "ValidationError"}, errorClassTransient},
		{"401 ExpiredAuthenticationToken", &apim.Error{Method: http.MethodPut, StatusCode: http.StatusUnauthorized,
			Code: "ExpiredAuthenticationToken"}, errorClassTransient},
		{"403 AuthorizationFailed", &apim.Error{Method: http.MethodPut, StatusCode: http.StatusForbidden,
			Code: "AuthorizationFailed"}, errorClassTransient},
		{"401 InvalidAuthenticationToken", &apim.Error{Method: http.MethodPut, StatusCode: http.StatusUnauthorized,
			Code: "InvalidAuthenticationToken"}, errorClassPermanent},
		{"403 without a code", &apim.Error{Method: http.MethodPut, StatusCode: http.StatusForbidden}, errorClassPermanent},
		{"write outcome unknown", fmt.Errorf("%w: %w", apim.ErrWriteOutcomeUnknown, errors.New("connection reset")), errorClassTransient},
		{"no operation URL", &apim.Error{Method: http.MethodPut, StatusCode: http.StatusAccepted, Err: apim.ErrNoOperationURL}, errorClassTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAPIMError(tc.err); got != tc.want {
				t.Errorf("classifyAPIMError(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestFailedNotBeforeKeepsTheFloor: a write whose outcome is unknown waits at least the
// floor, whatever the backoff would be, and still stalls at MaxAttempts.
func TestFailedNotBeforeKeepsTheFloor(t *testing.T) {
	clock := &rcClock{t: rcStart}
	p := rcPolicy(clock)
	tag := rcTag("")
	st := &tag.Status.RetryStatus
	unknown := fmt.Errorf("%w: %w", apim.ErrWriteOutcomeUnknown, errors.New("EOF"))

	want := []time.Duration{unknownWriteRetryFloor, unknownWriteRetryFloor, unknownWriteRetryFloor, unknownWriteRetryFloor}
	for i, wantDelay := range want {
		w := p.begin(logr.Discard(), "APIMAPIDeployment", tag)
		if proceed, _ := w.gate(*st, false); !proceed {
			t.Fatalf("attempt %d: gate refused a due write", i+1)
		}
		out := w.failedNotBefore(st, unknown, unknownWriteRetryFloor)
		if out.Phase != phaseBackoff || out.Result.RequeueAfter != wantDelay {
			t.Fatalf("attempt %d: %s after %s, want Backoff after %s", i+1, out.Phase, out.Result.RequeueAfter, wantDelay)
		}
		if next := clock.now().Add(wantDelay).Format(time.RFC3339); st.NextAttemptAt != next {
			t.Errorf("attempt %d: nextAttemptAt = %s, want %s", i+1, st.NextAttemptAt, next)
		}
		clock.advance(wantDelay)
	}
	if unknownWriteRetryFloor != 30*time.Minute {
		t.Errorf("unknownWriteRetryFloor = %s, want 30m", unknownWriteRetryFloor)
	}
	out := p.begin(logr.Discard(), "APIMAPIDeployment", tag).failedNotBefore(st, unknown, unknownWriteRetryFloor)
	if out.Phase != phaseStalled {
		t.Errorf("fifth failure = %s, want Stalled", out.Phase)
	}

	// A floor below the backoff changes nothing.
	clock2 := &rcClock{t: rcStart}
	p2 := rcPolicy(clock2)
	st2 := &rcTag("").Status.RetryStatus
	if out := p2.begin(logr.Discard(), "APIMTag", rcTag("")).failedNotBefore(st2, rcTransientErr, time.Second); out.Result.RequeueAfter != time.Minute {
		t.Errorf("floor 1s: RequeueAfter = %s, want the 1m backoff", out.Result.RequeueAfter)
	}
}

// TestAPIMWriterOptionsBoundTheReconcile: every controller that writes to APIM gets the
// reconcile timeout, so a worker can never hang on one resource.
func TestAPIMWriterOptionsBoundTheReconcile(t *testing.T) {
	opts := apimWriterOptions()
	if opts.ReconciliationTimeout != apimReconcileTimeout {
		t.Errorf("ReconciliationTimeout = %s, want %s", opts.ReconciliationTimeout, apimReconcileTimeout)
	}
	if apimReconcileTimeout != 10*time.Minute {
		t.Errorf("apimReconcileTimeout = %s, want 10m", apimReconcileTimeout)
	}
	if opts.MaxConcurrentReconciles != maxConcurrentAPIMWrites {
		t.Errorf("MaxConcurrentReconciles = %d, want %d", opts.MaxConcurrentReconciles, maxConcurrentAPIMWrites)
	}
}

// TestOutcomeContextOutlivesTheReconcile: the outcome of a write is recorded even when the
// reconcile's context is already cancelled or past its deadline, but never unbounded.
func TestOutcomeContextOutlivesTheReconcile(t *testing.T) {
	type ctxKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "kept"))
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	for name, ctx := range map[string]context.Context{"cancelled": parent, "deadline exceeded": expired} {
		t.Run(name, func(t *testing.T) {
			if ctx.Err() == nil {
				t.Fatal("the parent should be done")
			}
			out, done := outcomeContext(ctx)
			defer done()
			if err := out.Err(); err != nil {
				t.Fatalf("outcome context is done at once: %v", err)
			}
			deadline, ok := out.Deadline()
			if !ok {
				t.Fatal("outcome context has no deadline")
			}
			if left := time.Until(deadline); left <= 0 || left > outcomeWriteTimeout {
				t.Errorf("deadline in %s, want within (0, %s]", left, outcomeWriteTimeout)
			}
		})
	}
	out, done := outcomeContext(parent)
	defer done()
	if out.Value(ctxKey{}) != "kept" {
		t.Error("the outcome context must keep the reconcile's values (logger, trace)")
	}
	done()
	if !errors.Is(out.Err(), context.Canceled) {
		t.Errorf("after its own cancel the outcome context is %v, want canceled", out.Err())
	}
}

// TestReplicaSetReadinessSignal: a ReplicaSet signals its APIs when its first pod becomes
// ready and again when all the pods it wants are ready, never when it is scaled to zero.
func TestReplicaSetReadinessSignal(t *testing.T) {
	rs := func(spec *int32, ready int32) *appsv1.ReplicaSet {
		r := &appsv1.ReplicaSet{}
		r.Spec.Replicas = spec
		r.Status.ReadyReplicas = ready
		return r
	}
	n := func(v int32) *int32 { return &v }

	for _, tc := range []struct {
		name     string
		old, new *appsv1.ReplicaSet
		want     bool
	}{
		{"first pod of three ready (0 -> 1)", rs(n(3), 0), rs(n(3), 1), true},
		{"one more pod, not yet all (1 -> 2 of 3)", rs(n(3), 1), rs(n(3), 2), false},
		{"last pod ready (2 -> 3 of 3)", rs(n(3), 2), rs(n(3), 3), true},
		{"all ready, nothing changed (3 -> 3)", rs(n(3), 3), rs(n(3), 3), false},
		{"a pod lost readiness (3 -> 2)", rs(n(3), 3), rs(n(3), 2), false},
		{"all pods lost readiness (3 -> 0)", rs(n(3), 3), rs(n(3), 0), false},
		{"scaled to zero, the old revision of a rollout", rs(n(1), 0), rs(n(0), 1), false},
		{"scaled to zero and draining", rs(n(0), 2), rs(n(0), 1), false},
		{"nil replicas means one: 0 -> 1", rs(nil, 0), rs(nil, 1), true},
		{"nil replicas means one: 1 -> 1", rs(nil, 1), rs(nil, 1), false},
		{"single replica 0 -> 1", rs(n(1), 0), rs(n(1), 1), true},
		{"jump straight to all ready (0 -> 3 of 3)", rs(n(3), 0), rs(n(3), 3), true},
		{"scaled up while all were ready (3 of 3 -> 3 of 5)", rs(n(3), 3), rs(n(5), 3), false},
		{"scale-up completes (4 -> 5 of 5)", rs(n(5), 4), rs(n(5), 5), true},
		{"scaled down below ready (3 of 3 -> 3 of 2)", rs(n(3), 3), rs(n(2), 3), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replicaSetReadinessSignal(tc.old, tc.new); got != tc.want {
				t.Errorf("replicaSetReadinessSignal = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestReplicaSetsStillRollingOut: a matched ReplicaSet of an older revision of the same
// Deployment that still has ready pods means a rolling update is under way.
func TestReplicaSetsStillRollingOut(t *testing.T) {
	owner := func(uid string) []metav1.OwnerReference {
		yes := true
		return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "app-" + uid, UID: types.UID(uid), Controller: &yes}}
	}
	rs := func(name, revision string, owners []metav1.OwnerReference, ready int32) appsv1.ReplicaSet {
		r := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: owners}}
		if revision != "" {
			r.Annotations = map[string]string{replicaSetRevisionAnnotation: revision}
		}
		r.Status.ReadyReplicas = ready
		return r
	}
	notController := []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "x", UID: "x"}}

	for _, tc := range []struct {
		name string
		rss  []appsv1.ReplicaSet
		want []string
	}{
		{"nothing matched", nil, nil},
		{"one ReplicaSet", []appsv1.ReplicaSet{rs("new", "2", owner("a"), 3)}, nil},
		{"old revision still ready", []appsv1.ReplicaSet{rs("old", "1", owner("a"), 2), rs("new", "2", owner("a"), 1)}, []string{"old"}},
		{"old revision drained", []appsv1.ReplicaSet{rs("old", "1", owner("a"), 0), rs("new", "2", owner("a"), 3)}, nil},
		{"two old revisions, one drained", []appsv1.ReplicaSet{
			rs("v1", "1", owner("a"), 0), rs("v2", "2", owner("a"), 1), rs("v3", "3", owner("a"), 1)}, []string{"v2"}},
		{"revisions compare as numbers", []appsv1.ReplicaSet{rs("v9", "9", owner("a"), 1), rs("v10", "10", owner("a"), 1)}, []string{"v9"}},
		{"newest first in the list", []appsv1.ReplicaSet{rs("new", "5", owner("a"), 1), rs("old", "4", owner("a"), 1)}, []string{"old"}},
		{"different Deployments are independent", []appsv1.ReplicaSet{rs("a1", "1", owner("a"), 1), rs("b2", "2", owner("b"), 1)}, nil},
		{"no owner is never counted", []appsv1.ReplicaSet{rs("bare", "1", nil, 1), rs("new", "2", owner("a"), 1)}, nil},
		{"an owner that is not the controller is never counted", []appsv1.ReplicaSet{rs("x1", "1", notController, 1), rs("new", "2", owner("a"), 1)}, nil},
		{"no revision is never counted", []appsv1.ReplicaSet{rs("norev", "", owner("a"), 1), rs("new", "2", owner("a"), 1)}, nil},
		{"an unparseable revision is never counted", []appsv1.ReplicaSet{rs("bad", "one", owner("a"), 1), rs("new", "2", owner("a"), 1)}, nil},
		{"the same revision twice is not a rollout", []appsv1.ReplicaSet{rs("x", "3", owner("a"), 1), rs("y", "3", owner("a"), 1)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := replicaSetsStillRollingOut(tc.rss)
			if !equalStrings(got, tc.want) {
				t.Errorf("replicaSetsStillRollingOut = %v, want %v", got, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCheckPendingImportUnreadableStartTime: a pendingImport whose startedAt cannot be read
// has no age to wait out, so a still running import counts as expired. A finished one is
// still finished: the operation is read first.
func TestCheckPendingImportUnreadableStartTime(t *testing.T) {
	t.Cleanup(apim.UseEndpoint("https://management.azure.com", http.DefaultClient))
	pending := &apimv1.APIMPendingImport{
		OperationURL: "https://management.azure.com/subscriptions/s/resourceGroups/r/providers/Microsoft.ApiManagement/service/a/operationresults/op",
		DesiredHash:  "h", StartedAt: "yesterday",
	}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	read := func(status apim.OperationStatus, err error) func(context.Context, string) (apim.OperationState, error) {
		return func(context.Context, string) (apim.OperationState, error) {
			return apim.OperationState{Status: status}, err
		}
	}

	running := checkPendingImport(context.Background(), pending, "h", now, read(apim.OperationRunning, nil))
	if running.step != pendingImportFailed || !errors.Is(running.err, apim.ErrImportWaitTimeout) {
		t.Errorf("running = %+v, want Failed with ErrImportWaitTimeout", running)
	}
	var apimErr *apim.Error
	if !errors.As(running.err, &apimErr) {
		t.Errorf("err = %T, want *apim.Error", running.err)
	}
	unreadable := checkPendingImport(context.Background(), pending, "h", now, read("", errors.New("409")))
	if unreadable.step != pendingImportFailed || !errors.Is(unreadable.err, apim.ErrImportWaitTimeout) {
		t.Errorf("unreadable = %+v, want Failed with ErrImportWaitTimeout", unreadable)
	}
	if done := checkPendingImport(context.Background(), pending, "h", now, read(apim.OperationSucceeded, nil)); done.step != pendingImportDone {
		t.Errorf("succeeded = %+v, want Done", done)
	}
}
