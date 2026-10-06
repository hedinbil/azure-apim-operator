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
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// This file bounds what a failing APIM write can cost. Every controller that writes to
// APIM (APIMAPIDeployment, APIMProduct, APIMTag, APIMInboundPolicy) goes through the same
// few calls: gate before writing, then succeeded or failed afterwards. Between them they
// back off exponentially, stop after five transient failures in a row (Stalled), stop at
// once on a request APIM will never accept (Invalid), and start over on a spec change or
// a new value of the retry annotation.
//
// It exists because of the 25-28 Sep 2026 incident, when one deployment re-imported a
// 1.75 MB OpenAPI document about 800 times in 69 hours: every error path requeued after a
// fixed 60 s with no cap, and the overlapping imports kept APIM too busy to finish any.

// retryAnnotation clears a Stalled or Invalid resource's failures and retries at once
// when it is added or set to a new value, e.g.
//
//	kubectl annotate apimtag my-tag apim.operator.io/retry="$(date +%s)" --overwrite
const retryAnnotation = "apim.operator.io/retry"

// Phases of a resource whose APIM write failed, alongside each kind's own phases.
const (
	phaseBackoff = "Backoff" // A write failed; the next one waits until status.nextAttemptAt.
	phaseStalled = "Stalled" // Too many transient failures in a row; no more writes until reset.
	phaseInvalid = "Invalid" // APIM rejected the request; no more writes until reset.
)

// Log messages for APIM writes. msgWriteStalled is matched verbatim by a Datadog log
// monitor: change it and the monitor goes silent.
const (
	msgWriteStarting   = "▶️ APIM write starting"
	msgWriteSucceeded  = "💚 APIM write succeeded"
	msgWriteFailed     = "💔 APIM write failed; backing off"
	msgWriteRejected   = "💔 APIM write rejected; not retrying"
	msgWriteBackingOff = "⏸️ APIM write still backing off; skipping"
	msgWriteHeld       = "⏸️ APIM write stopped; waiting for a spec change or the retry annotation"
	msgWriteStalled    = "🛑 APIM write stalled"
	msgWriteReset      = "🔁 APIM write failures cleared"
)

// maxConcurrentAPIMWrites is how many resources of one kind a controller reconciles at
// once. The workqueue never hands the same object to two workers, so this only lets
// unrelated resources proceed while one waits on a slow import.
const maxConcurrentAPIMWrites = 4

// apimWriterOptions are the controller options of every controller that writes to APIM.
func apimWriterOptions() controller.Options {
	return controller.Options{MaxConcurrentReconciles: maxConcurrentAPIMWrites}
}

// latestReader is where a controller that writes to APIM reads its own resource at the
// start of a reconcile: straight from the API server through apiReader, which
// SetupWithManager sets to the manager's uncached reader, or through c when apiReader is
// nil (specs that call Reconcile directly with an uncached client).
//
// The informer cache can still hold the status from before the failure the previous
// reconcile of the same resource just patched. When an event marked the resource dirty
// during that reconcile (a ReplicaSet signal while a deployment waited out a slow
// import), the workqueue hands it straight back, and a gate reading the cache would see
// no failure and no nextAttemptAt and write again at once: the overlapping imports
// behind the Sep 2026 incident. One uncached GET per reconcile rules that out.
func latestReader(apiReader client.Reader, c client.Client) client.Reader {
	if apiReader != nil {
		return apiReader
	}
	return c
}

// apimErrorClass says whether retrying a failed APIM write can help.
type apimErrorClass string

const (
	// errorClassTransient may succeed later: throttling, conflicts, timeouts, APIM overload.
	errorClassTransient apimErrorClass = "transient"
	// errorClassPermanent will fail the same way until the spec or the credentials change.
	errorClassPermanent apimErrorClass = "permanent"
)

// transientAzureCodes are Azure error codes that mean "busy or racing", whatever HTTP
// status they arrive with; all of them were seen during the Sep 2026 import loop.
var transientAzureCodes = map[string]bool{
	"InternalServerError":        true,
	"PreconditionFailed":         true,
	"Timeout":                    true,
	"ManagementApiRequestFailed": true,
	"Conflict":                   true,
}

// classifyAPIMError sorts a failed write. Permanent is 400, 401, 403, or 404 on a write,
// unless the Azure code marks it transient or the 404 is on a write that depends on
// another resource (apim.ErrDependencyNotFound: a product or tag assignment, a policy, a
// patch of the imported API); everything else, including errors this function does not
// recognise, is transient. A wrong guess towards transient costs at most five bounded
// attempts before Stalled; a wrong guess towards permanent would stop a write that would
// have succeeded.
func classifyAPIMError(err error) apimErrorClass {
	if err == nil {
		return errorClassTransient
	}
	if errors.Is(err, apim.ErrImportWaitTimeout) || errors.Is(err, apim.ErrDependencyNotFound) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errorClassTransient
	}
	var apimErr *apim.Error
	if errors.As(err, &apimErr) {
		if transientAzureCodes[apimErr.Code] || transientAzureCodes[apimErr.DetailCode] {
			return errorClassTransient
		}
		switch apimErr.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			return errorClassPermanent
		case http.StatusNotFound:
			// A read that finds nothing may just be early; a write to a path that does not
			// exist (the APIM service is not there) will not fix itself. A write whose
			// parent another resource creates was caught above as ErrDependencyNotFound.
			if apimErr.Method != http.MethodGet {
				return errorClassPermanent
			}
		}
		return errorClassTransient
	}
	// Transport failures (DNS, TLS, connection reset, *url.Error) and anything else.
	return errorClassTransient
}

// retryPolicy is how failed APIM writes are retried. Reconcilers carry a *retryPolicy;
// nil means productionRetryPolicy. Tests set short delays and a fixed clock.
type retryPolicy struct {
	// BaseDelay is the wait after the first failure; it doubles with every further one.
	BaseDelay time.Duration
	// MaxDelay caps the doubled wait before jitter.
	MaxDelay time.Duration
	// MaxAttempts is how many transient failures in a row make a resource Stalled.
	MaxAttempts int32
	// Jitter spreads each wait by up to this fraction either way (0.2 is +/-20 %).
	// Zero means no jitter.
	Jitter float64
	// Now is the clock.
	Now func() time.Time
	// Random returns a number in [0, 1) and drives the jitter.
	Random func() float64
}

// productionRetryPolicy is 1, 2, 4, 8 minutes (+/-20 %) between attempts, then Stalled
// after the fifth failure: about a quarter of an hour of trying before a human is asked.
var productionRetryPolicy = retryPolicy{
	BaseDelay:   time.Minute,
	MaxDelay:    30 * time.Minute,
	MaxAttempts: 5,
	Jitter:      0.2,
	Now:         time.Now,
	Random:      rand.Float64,
}

// effective fills what a policy leaves unset from productionRetryPolicy, Jitter aside
// (zero means none), so a nil or partial policy always works.
func (p *retryPolicy) effective() *retryPolicy {
	if p == nil {
		return &productionRetryPolicy
	}
	out := *p
	if out.BaseDelay <= 0 {
		out.BaseDelay = productionRetryPolicy.BaseDelay
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = productionRetryPolicy.MaxDelay
	}
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = productionRetryPolicy.MaxAttempts
	}
	if out.Now == nil {
		out.Now = productionRetryPolicy.Now
	}
	if out.Random == nil {
		out.Random = productionRetryPolicy.Random
	}
	return &out
}

// delay is the wait after the n-th consecutive failure: min(BaseDelay * 2^(n-1),
// MaxDelay), then spread by Jitter.
func (p *retryPolicy) delay(n int32) time.Duration {
	p = p.effective()
	d := p.BaseDelay
	for i := int32(1); i < n && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if p.Jitter > 0 {
		d = time.Duration(float64(d) * (1 + p.Jitter*(2*p.Random()-1)))
	}
	return d
}

// apimWrite is one reconcile's attempt to write a resource to APIM. Make one with begin,
// ask gate whether to write, then report the outcome with succeeded or failed. It keeps
// what gate decided, so the reset it implies lands in the same status patch as the
// outcome.
type apimWrite struct {
	policy *retryPolicy
	log    logr.Logger
	// keysAndValues identify the resource in every log line: kind, namespace, name and
	// whatever the controller added (apiID, productID, tagID).
	keysAndValues []any
	// retryValue is the retry annotation's value when the reconcile started.
	retryValue string
	// reset is set by gate when the failures must be cleared before this write counts.
	reset bool
	// attempt is the number this write will have among consecutive attempts.
	attempt int32
	// heldPhase and heldMessage are what the status should say when gate turned the write
	// away: Backoff, Stalled or Invalid and why. Empty when gate let it through.
	heldPhase   string
	heldMessage string
}

// begin starts tracking one APIM write for obj. kind is the resource kind as it should
// appear in logs (e.g. "APIMTag"); keysAndValues are extra log keys such as "tagID", id.
func (p *retryPolicy) begin(log logr.Logger, kind string, obj client.Object, keysAndValues ...any) *apimWrite {
	kv := append([]any{"kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName()}, keysAndValues...)
	return &apimWrite{
		policy:        p.effective(),
		log:           log,
		keysAndValues: kv,
		retryValue:    obj.GetAnnotations()[retryAnnotation],
	}
}

// gate decides whether this reconcile may write to APIM. specChanged is the caller's
// own test: metadata.generation != status.observedGeneration, or for a deployment the
// desired hash differing from status.desiredHash.
//
// It returns proceed=true when the write should happen now. Otherwise the caller
// returns result as it is, without writing to APIM or changing the status: RequeueAfter
// the remaining backoff, or no requeue at all for a Stalled or Invalid resource. gate
// never modifies st; a reset it decides on is applied by prepare, succeeded or failed.
func (w *apimWrite) gate(st apimv1.RetryStatus, specChanged bool) (proceed bool, result ctrl.Result) {
	retryRequested := w.retryValue != "" && w.retryValue != st.LastRetryAnnotation
	if specChanged || retryRequested {
		w.reset = true
		w.attempt = 1
		if st.ConsecutiveFailures > 0 || st.NextAttemptAt != "" {
			reason := "spec changed"
			if retryRequested {
				reason = retryAnnotation + " annotation set"
			}
			w.log.Info(msgWriteReset, w.with("reason", reason, "previousFailures", st.ConsecutiveFailures)...)
		}
		return true, ctrl.Result{}
	}

	w.attempt = st.ConsecutiveFailures + 1
	if st.ConsecutiveFailures > 0 && st.NextAttemptAt == "" {
		// Stalled or Invalid: failed() leaves nextAttemptAt empty only for those. The status
		// does not say which once an error path (e.g. a missing APIMService) has overwritten
		// the phase, so the count decides: a permanent error that happens to arrive on the
		// last attempt then reads Stalled, which is just as stopped.
		w.log.Info(msgWriteHeld, w.with("attempts", st.ConsecutiveFailures, "retryAnnotation", retryAnnotation)...)
		if st.ConsecutiveFailures >= w.policy.MaxAttempts {
			w.heldPhase, w.heldMessage = phaseStalled, stalledMessage(st.ConsecutiveFailures)
		} else {
			w.heldPhase, w.heldMessage = phaseInvalid, invalidMessage
		}
		return false, ctrl.Result{}
	}
	if st.NextAttemptAt != "" {
		next, err := time.Parse(time.RFC3339, st.NextAttemptAt)
		if err == nil {
			if wait := next.Sub(w.policy.Now()); wait > 0 {
				w.log.Info(msgWriteBackingOff, w.with("attempt", w.attemptLabel(), "nextAttemptAt", st.NextAttemptAt,
					"remaining", wait.Round(time.Second).String())...)
				w.heldPhase = phaseBackoff
				w.heldMessage = fmt.Sprintf("APIM write failed %d times in a row; next attempt at %s",
					st.ConsecutiveFailures, st.NextAttemptAt)
				return false, ctrl.Result{RequeueAfter: wait}
			}
		}
		// An unreadable time is treated as due rather than as a reason to stop for good.
	}
	return true, ctrl.Result{}
}

// heldStatus is the phase and message a resource gate turned away should show, for the
// caller to patch into its status when currentPhase says something else. That happens
// when a path before the gate (missing APIMService, missing identity, failed OpenAPI
// fetch) wrote phase Error and its cause has since gone: without this the resource
// would keep reporting the stale error while it is in fact Stalled or Invalid and
// waiting for the retry annotation. ok is false when gate let the write through or the
// phase already says the resource is stopped or backing off.
func (w *apimWrite) heldStatus(currentPhase string) (phase, message string, ok bool) {
	switch {
	case w.heldPhase == "":
		return "", "", false
	case w.heldPhase == currentPhase:
		return "", "", false
	case w.heldPhase != phaseBackoff && (currentPhase == phaseStalled || currentPhase == phaseInvalid):
		// Stopped either way; the phase failed() wrote is the precise one.
		return "", "", false
	}
	return w.heldPhase, w.heldMessage, true
}

// prepare applies the reset gate decided on and remembers the retry annotation value as
// handled. succeeded and failed call it themselves; a controller calls it directly only
// when it persists the status before the write's outcome is known (the deployment's
// Importing patch), so a crash in between cannot leave stale failures behind. Calling it
// more than once is harmless.
func (w *apimWrite) prepare(st *apimv1.RetryStatus) {
	if w.reset {
		st.ConsecutiveFailures = 0
		st.NextAttemptAt = ""
	}
	if w.retryValue != "" {
		st.LastRetryAnnotation = w.retryValue
	}
}

// starting logs that the write begins, with the attempt number and any extra keys.
func (w *apimWrite) starting(keysAndValues ...any) {
	w.log.Info(msgWriteStarting, w.with(append([]any{"attempt", w.attemptLabel()}, keysAndValues...)...)...)
}

// succeeded clears the failures and logs the success. The caller sets its own success
// phase and, for product, tag and policy, status.observedGeneration.
func (w *apimWrite) succeeded(st *apimv1.RetryStatus) {
	w.prepare(st)
	st.ConsecutiveFailures = 0
	st.NextAttemptAt = ""
	w.log.Info(msgWriteSucceeded, w.with("attempt", w.attemptLabel())...)
}

// writeOutcome is what a failed write means for the resource.
type writeOutcome struct {
	// Phase is phaseBackoff, phaseStalled or phaseInvalid.
	Phase string
	// Result is what Reconcile returns, with a nil error: RequeueAfter the backoff, or
	// nothing for Stalled and Invalid, which wait for a spec change or the annotation.
	Result ctrl.Result
	// Class is how the error was classified.
	Class apimErrorClass
	// Message is a one-line summary for status.message, e.g. "APIM write failed
	// (transient, attempt 2/5); next attempt at 2026-09-28T10:04:00Z".
	Message string
}

// statusMessage is status.message for a failed write on a kind without a lastError
// field (product, tag, policy): which step failed, the error APIM returned, and what
// happens next. The deployment keeps the error in status.lastError and writes only
// step + ": " + Message.
func (o writeOutcome) statusMessage(step string, err error) string {
	return fmt.Sprintf("%s: %v; %s", step, err, o.Message)
}

// failed records err against st and logs it: 💔 with the next attempt time while backing
// off, 💔 rejected for a permanent error, 🛑 stalled when the attempts are used up. The
// caller copies Phase and Message into its status (and err into lastError where the kind
// has one), patches the status, and returns Result with a nil error. Returning the error
// instead would put controller-runtime's own rate limiter back in charge. A failing
// status patch is logged, not returned, for the same reason: the failure count is lost
// for that attempt, but the next one still waits out the backoff.
func (w *apimWrite) failed(st *apimv1.RetryStatus, err error) writeOutcome {
	w.prepare(st)
	st.ConsecutiveFailures++
	n := st.ConsecutiveFailures
	w.attempt = n
	class := classifyAPIMError(err)

	if class == errorClassPermanent {
		st.NextAttemptAt = ""
		w.log.Error(err, msgWriteRejected, w.with("class", class, "attempt", w.attemptLabel())...)
		return writeOutcome{
			Phase:   phaseInvalid,
			Class:   class,
			Message: invalidMessage,
		}
	}

	if n >= w.policy.MaxAttempts {
		st.NextAttemptAt = ""
		w.log.Error(err, msgWriteStalled, w.with("attempts", n, "lastError", err.Error())...)
		return writeOutcome{
			Phase:   phaseStalled,
			Class:   class,
			Message: stalledMessage(n),
		}
	}

	now := w.policy.Now()
	// Rounded up to the second RFC3339 keeps, so the requeue never lands before the time
	// written to the status and is then turned away by gate.
	next := now.Add(w.policy.delay(n))
	if rounded := next.Truncate(time.Second); !rounded.Equal(next) {
		next = rounded.Add(time.Second)
	}
	st.NextAttemptAt = next.UTC().Format(time.RFC3339)
	w.log.Error(err, msgWriteFailed, w.with("class", class, "attempt", w.attemptLabel(), "nextAttemptAt", st.NextAttemptAt)...)
	return writeOutcome{
		Phase:  phaseBackoff,
		Result: ctrl.Result{RequeueAfter: next.Sub(now)},
		Class:  class,
		Message: fmt.Sprintf("APIM write failed (%s, attempt %s); next attempt at %s",
			class, w.attemptLabel(), st.NextAttemptAt),
	}
}

// invalidMessage is status.message (after the step) of an Invalid resource.
const invalidMessage = "APIM rejected the write; not retrying until the spec changes or the " + retryAnnotation + " annotation is set"

// stalledMessage is status.message (after the step) of a resource Stalled after n failures.
func stalledMessage(n int32) string {
	return fmt.Sprintf("APIM write stalled after %d failures in a row; not retrying until the spec changes or the %s annotation is set",
		n, retryAnnotation)
}

// attemptLabel renders the attempt as "n/max".
func (w *apimWrite) attemptLabel() string {
	return fmt.Sprintf("%d/%d", w.attempt, w.policy.MaxAttempts)
}

// with returns the resource's log keys followed by keysAndValues.
func (w *apimWrite) with(keysAndValues ...any) []any {
	out := make([]any, 0, len(w.keysAndValues)+len(keysAndValues))
	out = append(out, w.keysAndValues...)
	return append(out, keysAndValues...)
}
