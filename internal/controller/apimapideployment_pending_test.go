package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

func TestCheckPendingImport(t *testing.T) {
	// The suite may point the apim package elsewhere; this test needs the real ARM host.
	t.Cleanup(apim.UseEndpoint("https://management.azure.com", http.DefaultClient))
	const operationURL = "https://management.azure.com/subscriptions/00000000-0000-0000-0000-000000000001" +
		"/resourceGroups/test-rg/providers/Microsoft.ApiManagement/service/test-apim/operationresults/op-1?api-version=2021-08-01"

	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	pendingFor := func(url string, startedAt time.Time) *apimv1.APIMPendingImport {
		return &apimv1.APIMPendingImport{OperationURL: url, DesiredHash: "hash-1", StartedAt: startedAt.Format(time.RFC3339)}
	}
	recent := pendingFor(operationURL, now.Add(-5*time.Minute))
	failure := &apim.Error{Operation: "import API", Code: "InternalServerError", Err: apim.ErrAsyncOperationFailed}

	tests := []struct {
		name        string
		pending     *apimv1.APIMPendingImport
		desiredHash string
		state       apim.OperationState
		readErr     error
		wantStep    pendingImportStep
		wantRead    bool
		wantDetail  string
		wantErr     error
	}{
		{
			name: "still running", pending: recent, desiredHash: "hash-1",
			state:    apim.OperationState{Status: apim.OperationRunning},
			wantStep: pendingImportWait, wantRead: true,
		},
		{
			// A failed reading is not a failed import: importing again here is what piled
			// imports on top of each other in the Sep 2026 incident.
			name: "reading the state fails", pending: recent, desiredHash: "hash-1",
			readErr:  errors.New("import API (poll) failed: 422 Unprocessable Entity: Timeout"),
			wantStep: pendingImportWait, wantRead: true, wantDetail: "Timeout",
		},
		{
			name: "finished for the desired state", pending: recent, desiredHash: "hash-1",
			state:    apim.OperationState{Status: apim.OperationSucceeded},
			wantStep: pendingImportDone, wantRead: true,
		},
		{
			name: "finished for an older desired state", pending: recent, desiredHash: "hash-2",
			state:    apim.OperationState{Status: apim.OperationSucceeded},
			wantStep: pendingImportRestart, wantRead: true,
		},
		{
			name: "failed", pending: recent, desiredHash: "hash-1",
			state:    apim.OperationState{Status: apim.OperationFailed, Err: failure},
			wantStep: pendingImportFailed, wantRead: true, wantErr: apim.ErrAsyncOperationFailed,
		},
		{
			name: "forgotten by APIM", pending: recent, desiredHash: "hash-1",
			state:    apim.OperationState{Status: apim.OperationGone},
			wantStep: pendingImportRestart, wantRead: true,
		},
		{
			name: "not an ARM URL", pending: pendingFor("https://attacker.example/op-1", now.Add(-5*time.Minute)), desiredHash: "hash-1",
			wantStep: pendingImportRestart,
		},
		{
			// Counted as a timed-out wait, so the retry policy backs off and eventually stalls.
			name: "older than the limit", pending: pendingFor(operationURL, now.Add(-maxPendingImportAge-time.Minute)), desiredHash: "hash-1",
			wantStep: pendingImportFailed, wantErr: apim.ErrImportWaitTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := false
			outcome := checkPendingImport(context.Background(), tt.pending, tt.desiredHash, now,
				func(_ context.Context, url string) (apim.OperationState, error) {
					read = true
					if url != tt.pending.OperationURL {
						t.Errorf("read %s, want %s", url, tt.pending.OperationURL)
					}
					return tt.state, tt.readErr
				})

			if outcome.step != tt.wantStep {
				t.Errorf("step = %d, want %d (message %q)", outcome.step, tt.wantStep, outcome.message)
			}
			if read != tt.wantRead {
				t.Errorf("read the operation = %t, want %t", read, tt.wantRead)
			}
			if !strings.Contains(outcome.detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", outcome.detail, tt.wantDetail)
			}
			if tt.wantErr != nil && !errors.Is(outcome.err, tt.wantErr) {
				t.Errorf("err = %v, want it to wrap %v", outcome.err, tt.wantErr)
			}
			if tt.wantStep == pendingImportFailed && classifyAPIMError(outcome.err) != errorClassTransient {
				t.Errorf("err %v classifies as %s, want transient", outcome.err, classifyAPIMError(outcome.err))
			}
			if outcome.message == "" {
				t.Error("message is empty")
			}
		})
	}
}

func TestPendingImportPollDelay(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for name, tt := range map[string]struct {
		startedAt string
		want      time.Duration
	}{
		"just accepted":       {now.Format(time.RFC3339), minPendingImportPoll},
		"twenty seconds old":  {now.Add(-20 * time.Second).Format(time.RFC3339), minPendingImportPoll},
		"four minutes old":    {now.Add(-4 * time.Minute).Format(time.RFC3339), 2 * time.Minute},
		"twenty minutes old":  {now.Add(-20 * time.Minute).Format(time.RFC3339), 10 * time.Minute},
		"an hour old":         {now.Add(-time.Hour).Format(time.RFC3339), maxPendingImportPoll},
		"unreadable start":    {"yesterday", minPendingImportPoll},
		"start in the future": {now.Add(time.Hour).Format(time.RFC3339), minPendingImportPoll},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pendingImportPollDelay(tt.startedAt, now); got != tt.want {
				t.Errorf("pendingImportPollDelay = %s, want %s", got, tt.want)
			}
		})
	}
}
