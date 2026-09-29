package controller

import (
	"context"
	"fmt"
	"time"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// Waiting for an import APIM has already accepted.
//
// APIM imports a large OpenAPI document asynchronously: the PUT comes back 202 and the
// import runs for minutes, far longer when the instance is busy. The operator used to wait
// at most three minutes, report an error and PUT the whole document again a minute later,
// and it treated a failed status poll the same way. APIM does not refuse the second import,
// it runs it alongside the first, so one slow import grew into a pile of concurrent ones
// that kept a single-unit Developer instance saturated for days, slowing every API on its
// gateway (retail-polaris-app-api on apim-apim-dev-hedinit, 2026-09-24 to 2026-09-29).
//
// Now the operation URL of an accepted import is kept in status.pendingImport and read on
// later reconciles. The API is not written again until APIM reports that operation
// finished or failed, no longer knows it, or it outlives maxPendingImportAge.

const (
	// pendingImportPollInterval is how often an accepted import is read.
	pendingImportPollInterval = 15 * time.Second
	// maxPendingImportAge bounds the wait for one import. APIM itself failed the longest
	// imports of the 2026-09 incident after about an hour, so one still reported as
	// running after two hours is treated as lost.
	maxPendingImportAge = 2 * time.Hour
	// requeueImportFailure is the pause after APIM reports that an import failed.
	requeueImportFailure = 60 * time.Second
)

type pendingImportStep int

const (
	// pendingImportWait: the import is still running, or its state could not be read.
	pendingImportWait pendingImportStep = iota
	// pendingImportDone: the import finished for the current desired state; the steps
	// that follow an import still have to run.
	pendingImportDone
	// pendingImportFailed: APIM reports that the import failed.
	pendingImportFailed
	// pendingImportRestart: the import finished for an older desired state, or can no
	// longer be tracked. The API has to be written again.
	pendingImportRestart
)

type pendingImportOutcome struct {
	step pendingImportStep
	// message is the status message for this outcome.
	message string
	// detail is why an import is still waited on or failed, for status.lastError.
	detail string
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
			step:    pendingImportRestart,
			message: fmt.Sprintf("APIM has not finished the import it accepted at %s within %s; importing again", pending.StartedAt, maxPendingImportAge),
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
		return pendingImportOutcome{
			step:    pendingImportFailed,
			message: fmt.Sprintf("APIM reported that the import it accepted at %s failed", pending.StartedAt),
			detail:  state.Detail,
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
