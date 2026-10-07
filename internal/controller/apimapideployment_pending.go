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

// Waiting for an import APIM has already accepted.
//
// APIM imports a large OpenAPI document asynchronously: the PUT comes back 202 and the
// import runs for minutes, far longer when the instance is busy. A wait that ends while
// the import is still running is not a failed import, and APIM does not refuse a second
// import of the same API: it runs it alongside the first. Treating the end of the wait as
// a failure and importing again after the backoff is how one slow import grew into a pile
// of concurrent ones that kept a single-unit Developer instance saturated for days, slowing
// every API on its gateway (retail-polaris-app-api on apim-apim-dev-hedinit, 2026-09-24 to
// 2026-09-29).
//
// So the operation URL of an import still running when the wait ends is kept in
// status.pendingImport and read on later reconciles. The API is not written again until
// APIM reports that operation finished or failed, or no longer knows it. A failed import,
// or one still running after maxPendingImportAge, counts as a failed write through the
// retry policy (Backoff, then Stalled), like any other.

const (
	// minPendingImportPoll and maxPendingImportPoll bound the wait between two readings
	// of a pending import; see pendingImportPollDelay.
	minPendingImportPoll = 15 * time.Second
	maxPendingImportPoll = 15 * time.Minute
	// maxPendingImportAge bounds the wait for one import. APIM itself failed the longest
	// imports of the Sep 2026 incident after about an hour, so one still reported as
	// running after two hours is treated as lost.
	maxPendingImportAge = 2 * time.Hour
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
	// pendingImportFailed: the import failed, or ran past maxPendingImportAge. err goes
	// to the retry policy.
	pendingImportFailed
	// pendingImportRestart: the import finished for an older desired state, or can no
	// longer be tracked. The API has to be written again.
	pendingImportRestart
)

type pendingImportOutcome struct {
	step pendingImportStep
	// message is the status message for this outcome.
	message string
	// detail is why an import is still waited on, for status.lastError.
	detail string
	// err is why the import failed, for the retry policy to classify.
	err error
}

// checkPendingImport decides what an import APIM accepted earlier means for this reconcile.
// readOperation reads the operation once; it is never called with a URL that is not on
// Azure Resource Manager, or for an import older than maxPendingImportAge.
func checkPendingImport(
	ctx context.Context,
	pending *apimv1.APIMPendingImport,
	desiredHash string,
	now time.Time,
	readOperation func(ctx context.Context, operationURL string) (apim.OperationState, error),
) pendingImportOutcome {
	if !apim.IsOperationURL(pending.OperationURL) {
		return pendingImportOutcome{
			step:    pendingImportRestart,
			message: "The recorded APIM operation URL is not an Azure Resource Manager URL; importing again",
		}
	}
	if startedAt, err := time.Parse(time.RFC3339, pending.StartedAt); err == nil && now.Sub(startedAt) > maxPendingImportAge {
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

	state, err := readOperation(ctx, pending.OperationURL)
	if err != nil {
		return pendingImportOutcome{
			step:    pendingImportWait,
			message: fmt.Sprintf("Waiting for the import APIM accepted at %s; reading its state failed", pending.StartedAt),
			detail:  err.Error(),
		}
	}

	switch state.Status {
	case apim.OperationSucceeded:
		if pending.DesiredHash == desiredHash {
			return pendingImportOutcome{
				step:    pendingImportDone,
				message: fmt.Sprintf("APIM finished the import it accepted at %s", pending.StartedAt),
			}
		}
		return pendingImportOutcome{
			step:    pendingImportRestart,
			message: fmt.Sprintf("APIM finished the import it accepted at %s, but the desired state has changed since; importing again", pending.StartedAt),
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
			step:    pendingImportRestart,
			message: fmt.Sprintf("APIM no longer knows the import it accepted at %s; importing again", pending.StartedAt),
		}
	default:
		return pendingImportOutcome{
			step:    pendingImportWait,
			message: fmt.Sprintf("APIM is still importing the definition it accepted at %s; the API is not written again until that finishes", pending.StartedAt),
		}
	}
}

// recordPendingImport keeps the operation of an import APIM is still running in
// status.pendingImport and returns the requeue that polls it. recorded is false when the
// status could not keep it: the patch failed, or the API server pruned the field because
// the installed CRD predates it. The caller then treats the write as failed, so the retry
// policy's backoff, rather than an immediate re-import, decides when to try again.
func (r *APIMAPIDeploymentReconciler) recordPendingImport(
	ctx context.Context,
	logger logr.Logger,
	deployment *apimv1.APIMAPIDeployment,
	operationURL, desiredHash string,
	now time.Time,
) (result ctrl.Result, recorded bool) {
	startedAt := now.UTC().Format(time.RFC3339)
	if err := updateAPIMAPIDeploymentStatus(ctx, r.Client, deployment, func(status *apimv1.APIMAPIDeploymentStatus) {
		status.Phase = apimDeploymentPhaseImporting
		status.Status = apimDeploymentStatusPending
		status.Message = fmt.Sprintf("APIM accepted the import at %s and is still running it; waiting for it before writing the API again", startedAt)
		status.LastError = ""
		status.PendingImport = &apimv1.APIMPendingImport{
			OperationURL: operationURL,
			DesiredHash:  desiredHash,
			StartedAt:    startedAt,
		}
	}); err != nil {
		logger.Error(err, "❌ Failed to record the import APIM is still running", "apiID", deployment.Spec.APIID)
		return ctrl.Result{}, false
	}
	if deployment.Status.PendingImport == nil {
		logger.Error(errors.New("status.pendingImport was not stored: the installed APIMAPIDeployment CRD is older than this operator"),
			"🚫 Cannot track the import APIM is still running", "apiID", deployment.Spec.APIID)
		return ctrl.Result{}, false
	}
	logger.Info("⏳ APIM is still running the import; waiting for it", "apiID", deployment.Spec.APIID, "operationURL", operationURL)
	return ctrl.Result{RequeueAfter: minPendingImportPoll}, true
}

// forgetPendingImport drops a pending import that ended without the follow-up steps
// running for the current desired state. Such an import may have changed the API, so
// nothing is known to be applied any more and the next attempt imports again.
func forgetPendingImport(status *apimv1.APIMAPIDeploymentStatus) {
	status.PendingImport = nil
	status.AppliedHash = ""
}
