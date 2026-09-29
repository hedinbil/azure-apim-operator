package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

const testOperationURL = "https://management.azure.com/subscriptions/00000000-0000-0000-0000-000000000001" +
	"/resourceGroups/test-rg/providers/Microsoft.ApiManagement/service/test-apim-service-instance" +
	"/operationresults/op-1?api-version=2021-08-01"

func TestCheckPendingImport(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	pendingFor := func(operationURL string, startedAt time.Time) *apimv1.APIMPendingImport {
		return &apimv1.APIMPendingImport{OperationURL: operationURL, DesiredHash: "hash-1", StartedAt: startedAt.Format(time.RFC3339)}
	}
	recent := pendingFor(testOperationURL, now.Add(-5*time.Minute))

	tests := []struct {
		name        string
		pending     *apimv1.APIMPendingImport
		desiredHash string
		state       apim.OperationState
		readErr     error
		wantStep    pendingImportStep
		wantRead    bool
		wantDetail  string
	}{
		{
			name: "still running", pending: recent, desiredHash: "hash-1",
			state:    apim.OperationState{Status: apim.OperationRunning},
			wantStep: pendingImportWait, wantRead: true,
		},
		{
			// A failed reading is not a failed import: importing again here is what piled
			// imports on top of each other in the 2026-09 incident.
			name: "reading the state fails", pending: recent, desiredHash: "hash-1",
			readErr:  errors.New("poll APIM operation: 422 Unprocessable Entity"),
			wantStep: pendingImportWait, wantRead: true, wantDetail: "422",
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
			state:    apim.OperationState{Status: apim.OperationFailed, Detail: `{"status":"Failed","error":{"code":"InternalServerError"}}`},
			wantStep: pendingImportFailed, wantRead: true, wantDetail: "InternalServerError",
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
			name: "older than the limit", pending: pendingFor(testOperationURL, now.Add(-maxPendingImportAge-time.Minute)), desiredHash: "hash-1",
			wantStep: pendingImportRestart,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := false
			outcome := checkPendingImport(context.Background(), tt.pending, tt.desiredHash, now,
				func(_ context.Context, operationURL string) (apim.OperationState, error) {
					read = true
					if operationURL != tt.pending.OperationURL {
						t.Errorf("read %s, want %s", operationURL, tt.pending.OperationURL)
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
			if outcome.message == "" {
				t.Error("message is empty")
			}
		})
	}
}
