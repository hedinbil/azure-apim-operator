package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// Following an import APIM has accepted.
//
// APIM imports a large OpenAPI document asynchronously: the PUT comes back 202 and the
// import runs for minutes, far longer when the instance is busy. APIM does not refuse a
// second import of the same API while the first runs: it runs both. Losing track of an
// accepted import and importing again is how one slow import grew into a pile of concurrent
// ones that kept a single-unit instance saturated for days (retail-polaris-app-api on
// apim-apim-dev-hedinit 2026-09-24 to 2026-09-29, and on apim-apim-prod-hedinit 2026-09-25
// to 2026-09-28).
//
// So the operation URL of every import APIM accepts is kept in status.pendingImport the
// moment the 202 arrives (recordPendingImport), and read on later reconciles. The API is
// not written again until APIM reports that operation finished. Every other way it can end
// (failed, still running after maxPendingImportAge, finished for a desired state that has
// changed since, forgotten by APIM) counts as a failed write through the retry policy
// (Backoff, then Stalled), like any other.

const (
	// minPendingImportPoll and maxPendingImportPoll bound the wait between two readings
	// of a pending import; see pendingImportPollDelay.
	minPendingImportPoll = 15 * time.Second
	maxPendingImportPoll = 15 * time.Minute
	// maxPendingImportAge bounds the wait for one import. APIM itself failed the longest
	// imports of the Sep 2026 incident after about an hour, so one still reported as
	// running after two hours is treated as lost.
	maxPendingImportAge = 2 * time.Hour
	// unknownWriteRetryFloor is the least wait before writing an API again after a write
	// that APIM may still be running: one sent without an answer, or accepted without an
	// operation the operator could keep. Imports that APIM ran in the background during
	// the Sep 2026 incident took up to an hour; the usual first backoff of a minute would
	// put a second one on top of the first.
	unknownWriteRetryFloor = 30 * time.Minute
	// recordPendingImportAttempts is how often the status patch that records an accepted
	// import is tried before the import counts as not recorded.
	recordPendingImportAttempts = 3
)

// pendingImportPollDelay is how long to wait before reading a pending import again: half
// its age, between minPendingImportPoll and maxPendingImportPoll. Most imports finish
// within minutes and are read often while young; one that runs until maxPendingImportAge
// is read about twenty times rather than hundreds, and every reading is a reconcile that
// fetches the OpenAPI document and writes the status.
func pendingImportPollDelay(startedAt string, now time.Time) time.Duration {
	started, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return minPendingImportPoll
	}
	return min(max(now.Sub(started)/2, minPendingImportPoll), maxPendingImportPoll)
}

type pendingImportStep int

const (
	// pendingImportWait: the import is still running, or its state could not be read.
	pendingImportWait pendingImportStep = iota
	// pendingImportDone: the import finished for the current desired state; the steps
	// that follow an import still have to run.
	pendingImportDone
	// pendingImportFailed: the write has to be made again. err goes to the retry policy,
	// which decides when (Backoff) or whether (Stalled, Invalid). That covers an import
	// APIM reports as failed, one still running after maxPendingImportAge, one that
	// finished for a desired state that has changed since, and one that can no longer be
	// followed. Routing every one of them through the policy is what keeps an import that
	// keeps ending this way (a document that differs on every fetch, an operation APIM
	// forgets before it is read) from turning into imports without end.
	pendingImportFailed
)

// errPendingImportOutdated, errPendingImportGone and errPendingImportUnusable say why a
// tracked import has to be made again although APIM did not report it as failed. None of
// them is about the request, so the retry policy treats them as transient.
var (
	errPendingImportOutdated = errors.New("the API definition changed while APIM was importing the previous one")
	errPendingImportGone     = errors.New("APIM no longer knows the import it accepted; its outcome is unknown")
	errPendingImportUnusable = errors.New("the recorded APIM operation URL is not on Azure Resource Manager")
)

type pendingImportOutcome struct {
	step pendingImportStep
	// message is the status message for this outcome.
	message string
	// detail is why an import is still waited on, for status.lastError.
	detail string
	// err is why the import has to be made again, for the retry policy to classify.
	err error
}

// checkPendingImport decides what an import APIM accepted earlier means for this reconcile.
// readOperation reads the operation once; it is never called with a URL that is not on
// Azure Resource Manager. The operation is always read before its age is held against it,
// so an import that finished just before maxPendingImportAge counts as finished.
func checkPendingImport(
	ctx context.Context,
	pending *apimv1.APIMPendingImport,
	desiredHash string,
	now time.Time,
	readOperation func(ctx context.Context, operationURL string) (apim.OperationState, error),
) pendingImportOutcome {
	if !apim.IsOperationURL(pending.OperationURL) {
		return pendingImportOutcome{
			step:    pendingImportFailed,
			message: "The recorded APIM operation cannot be followed",
			err:     errPendingImportUnusable,
		}
	}

	state, readErr := readOperation(ctx, pending.OperationURL)
	if readErr == nil {
		switch state.Status {
		case apim.OperationSucceeded:
			if pending.DesiredHash == desiredHash {
				return pendingImportOutcome{
					step:    pendingImportDone,
					message: fmt.Sprintf("APIM finished the import it accepted at %s", pending.StartedAt),
				}
			}
			return pendingImportOutcome{
				step:    pendingImportFailed,
				message: fmt.Sprintf("APIM finished the import it accepted at %s, but the desired state has changed since", pending.StartedAt),
				err:     errPendingImportOutdated,
			}
		case apim.OperationFailed:
			failure := state.Err
			if failure == nil {
				failure = errors.New("APIM reported that the import failed")
			}
			return pendingImportOutcome{
				step:    pendingImportFailed,
				message: fmt.Sprintf("APIM reported that the import it accepted at %s failed", pending.StartedAt),
				err:     failure,
			}
		case apim.OperationGone:
			return pendingImportOutcome{
				step:    pendingImportFailed,
				message: fmt.Sprintf("APIM no longer knows the import it accepted at %s", pending.StartedAt),
				err:     errPendingImportGone,
			}
		}
	}

	// Still running, or the reading failed: keep waiting, up to maxPendingImportAge. A
	// start time that does not parse counts as too old, or the wait would never end.
	if startedAt, err := time.Parse(time.RFC3339, pending.StartedAt); err != nil || now.Sub(startedAt) > maxPendingImportAge {
		return pendingImportOutcome{
			step:    pendingImportFailed,
			message: fmt.Sprintf("APIM has not finished the import it accepted at %s", pending.StartedAt),
			err: &apim.Error{
				Operation: "import API",
				Method:    http.MethodGet,
				Message:   fmt.Sprintf("operation still running after %s", maxPendingImportAge),
				Err:       apim.ErrImportWaitTimeout,
			},
		}
	}
	if readErr != nil {
		return pendingImportOutcome{
			step:    pendingImportWait,
			message: fmt.Sprintf("Waiting for the import APIM accepted at %s; reading its state failed", pending.StartedAt),
			detail:  readErr.Error(),
		}
	}
	return pendingImportOutcome{
		step:    pendingImportWait,
		message: fmt.Sprintf("APIM is still importing the definition it accepted at %s; the API is not written again until that finishes", pending.StartedAt),
	}
}

// recordPendingImport keeps the operation of an import APIM has just accepted in
// status.pendingImport and returns the requeue that reads it first. It runs as soon as APIM
// answers 202, before anything else can happen to this reconcile, so the operation is known
// from the moment it exists. recorded is false when the status could not keep it: the patch
// failed recordPendingImportAttempts times, or the API server pruned the field because the
// installed CRD predates it. The caller then counts the write as failed and keeps the next
// attempt clear of the running import; pending is the record to try once more with that.
func (r *APIMAPIDeploymentReconciler) recordPendingImport(
	ctx context.Context,
	logger logr.Logger,
	deployment *apimv1.APIMAPIDeployment,
	accepted apim.WriteResult,
	desiredHash string,
	now time.Time,
) (pending *apimv1.APIMPendingImport, result ctrl.Result, recorded bool) {
	startedAt := now.UTC().Format(time.RFC3339)
	pending = &apimv1.APIMPendingImport{
		OperationURL: accepted.OperationURL,
		DesiredHash:  desiredHash,
		StartedAt:    startedAt,
	}
	// Detached from the reconcile: an operator that is shutting down still records the
	// operation it started, instead of leaving the next leader to import on top of it.
	recordCtx, cancel := outcomeContext(ctx)
	defer cancel()
	var err error
	for attempt := 1; attempt <= recordPendingImportAttempts; attempt++ {
		if err = updateAPIMAPIDeploymentStatus(recordCtx, r.Client, deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
			status.Phase = apimDeploymentPhaseImporting
			status.Status = apimDeploymentStatusPending
			status.Message = fmt.Sprintf("APIM accepted the import at %s and is running it; waiting for it before writing the API again", startedAt)
			status.LastError = ""
			status.PendingImport = pending.DeepCopy()
		}); err == nil || recordCtx.Err() != nil {
			break
		}
	}
	if err != nil {
		logger.Error(err, "❌ Failed to record the import APIM accepted", "apiID", deployment.Spec.APIID)
		return pending, ctrl.Result{}, false
	}
	if deployment.Status.PendingImport == nil {
		logger.Error(errors.New("status.pendingImport was not stored: the installed APIMAPIDeployment CRD is older than this operator"),
			"🚫 Cannot track the import APIM accepted", "apiID", deployment.Spec.APIID)
		return pending, ctrl.Result{}, false
	}
	logger.Info("⏳ APIM accepted the import; following it", "apiID", deployment.Spec.APIID, "operationURL", accepted.OperationURL)
	firstRead := min(max(accepted.RetryAfter, minPendingImportPoll), maxPendingImportPoll)
	return pending, ctrl.Result{RequeueAfter: firstRead}, true
}

// recordImported records that the API itself is written for desiredHash (and that no
// import is pending any more), before the product, tag and host steps run. If one of them
// fails, the next attempt runs those steps without importing the definition again. Best
// effort: without the record the next attempt imports again, as it always used to.
func (r *APIMAPIDeploymentReconciler) recordImported(
	ctx context.Context,
	logger logr.Logger,
	deployment *apimv1.APIMAPIDeployment,
	desiredHash string,
) {
	recordCtx, cancel := outcomeContext(ctx)
	defer cancel()
	if err := updateAPIMAPIDeploymentStatus(recordCtx, r.Client, deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
		status.ImportedHash = desiredHash
		status.PendingImport = nil
	}); err != nil {
		logger.Error(err, "⚠️ Failed to record the finished import; the next attempt imports again", "apiID", deployment.Spec.APIID)
	}
}

// forgetPendingImport drops a pending import that ended without the follow-up steps
// running for the current desired state. Such an import may have changed the API, so
// nothing is known to be applied any more and the next attempt imports again.
func forgetPendingImport(status *apimv1.APIMAPIDeploymentStatus) {
	status.PendingImport = nil
	status.AppliedHash = ""
}
