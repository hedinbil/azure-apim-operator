package apim

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// failingTransport fails every request, as a dropped connection does.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection reset by peer")
}

// TestImportStillRunningCarriesItsOperation pins the fix for the Sep 2026 incident: the
// wait for an import APIM is still running ends with the operation URL, so the controller
// can keep waiting for that import instead of starting another one alongside it.
func TestImportStillRunningCarriesItsOperation(t *testing.T) {
	withAsyncTiming(t, 40*time.Millisecond, 5*time.Millisecond)
	asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/running"}}, func(_ int, w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, `{"status":"InProgress"}`)
	})

	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, ErrImportWaitTimeout) {
		t.Fatalf("import = %v, want ErrImportWaitTimeout", err)
	}
	if got, want := RunningOperation(err), hcUnroutableHost+"/operations/running"; got != want {
		t.Errorf("RunningOperation = %q, want %q", got, want)
	}
}

// TestFailedPollLeavesTheOperationRunning: a poll the management endpoint fails says
// nothing about the import, which may well still be running. The answers are the ones
// apim-apim-dev-hedinit gave while imports were still running in Sep 2026.
func TestFailedPollLeavesTheOperationRunning(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
	}{
		"management endpoint timed out":   {http.StatusUnprocessableEntity, `{"error":{"code":"Timeout","message":"Call to Management API timed out"}}`},
		"management endpoint unreachable": {http.StatusConflict, `{"error":{"code":"ManagementApiRequestFailed"}}`},
		"throttled":                       {http.StatusTooManyRequests, ``},
		"server error":                    {http.StatusServiceUnavailable, ``},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			withAsyncTiming(t, 2*time.Second, time.Millisecond)
			asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/unreadable"}}, func(_ int, w http.ResponseWriter) {
				writeJSON(w, answer.status, answer.body)
			})

			err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
			if err == nil {
				t.Fatal("import = nil, want the failed poll")
			}
			if got, want := RunningOperation(err), hcUnroutableHost+"/operations/unreadable"; got != want {
				t.Errorf("RunningOperation(%v) = %q, want %q", err, got, want)
			}
		})
	}
}

// TestFailedPollAboutTheOperationEndsIt: an answer about the operation itself is no
// reason to keep waiting for it.
func TestFailedPollAboutTheOperationEndsIt(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
	}{
		"rejected":  {http.StatusBadRequest, `{"error":{"code":"ValidationError"}}`},
		"forgotten": {http.StatusNotFound, `{"error":{"code":"ResourceNotFound"}}`},
		"failed":    {http.StatusOK, `{"status":"Failed","error":{"code":"InternalServerError"}}`},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			withAsyncTiming(t, 2*time.Second, time.Millisecond)
			asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/ended"}}, func(_ int, w http.ResponseWriter) {
				writeJSON(w, answer.status, answer.body)
			})

			err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
			if err == nil {
				t.Fatal("import = nil, want an error")
			}
			if got := RunningOperation(err); got != "" {
				t.Errorf("RunningOperation(%v) = %q, want \"\"", err, got)
			}
		})
	}
}

func TestRunningOperationIgnoresOtherErrors(t *testing.T) {
	for name, err := range map[string]error{
		"nil":                        nil,
		"plain":                      errors.New("boom"),
		"typed without an operation": &Error{Operation: "import API", StatusCode: http.StatusServiceUnavailable},
		"rejected write":             &Error{Operation: "import API", StatusCode: http.StatusBadRequest, OperationURL: hcUnroutableHost + "/operations/x"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RunningOperation(err); got != "" {
				t.Errorf("RunningOperation = %q, want \"\"", got)
			}
		})
	}
}

// TestGetOperationStateClassifiesAPIMAnswers covers what a poll of a pending import can
// say. A failed reading comes back as an error, not as a failed operation.
func TestGetOperationStateClassifiesAPIMAnswers(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		want     OperationStatus
		wantErr  bool
		wantCode string
	}{
		{name: "accepted", status: http.StatusAccepted, want: OperationRunning},
		{name: "in progress", status: http.StatusOK, body: `{"status":"InProgress"}`, want: OperationRunning},
		{name: "succeeded", status: http.StatusOK, body: `{"status":"Succeeded"}`, want: OperationSucceeded},
		{name: "done without status", status: http.StatusOK, body: `{}`, want: OperationSucceeded},
		{name: "provisioning state", status: http.StatusOK, body: `{"properties":{"provisioningState":"Succeeded"}}`, want: OperationSucceeded},
		{
			name: "failed", status: http.StatusOK,
			body: `{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor"}}`,
			want: OperationFailed, wantCode: "InternalServerError",
		},
		{name: "forgotten", status: http.StatusNotFound, body: `{"error":{"code":"ResourceNotFound"}}`, want: OperationGone},
		{name: "rejected", status: http.StatusBadRequest, body: `{"error":{"code":"ValidationError"}}`, want: OperationFailed, wantCode: "ValidationError"},
		{name: "management endpoint timed out", status: http.StatusUnprocessableEntity, body: `{"error":{"code":"Timeout"}}`, wantErr: true},
		{name: "management endpoint unreachable", status: http.StatusConflict, body: `{"error":{"code":"ManagementApiRequestFailed"}}`, wantErr: true},
		{name: "throttled", status: http.StatusTooManyRequests, wantErr: true},
		{name: "server error", status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tt.status, tt.body)
			})

			state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/op-1?api-version=2021-08-01")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("GetOperationState() = %+v, want an error", state)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetOperationState() error = %v", err)
			}
			if state.Status != tt.want {
				t.Errorf("Status = %s, want %s", state.Status, tt.want)
			}
			if tt.wantCode != "" {
				var apimErr *Error
				if !errors.As(state.Err, &apimErr) || apimErr.Code != tt.wantCode {
					t.Errorf("Err = %v, want an *Error with code %s", state.Err, tt.wantCode)
				}
			}
			if got := fake.count(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
			req := fake.request(0)
			if req.Method != http.MethodGet || req.URL.Path != "/operations/op-1" {
				t.Errorf("polled %s %s, want GET /operations/op-1", req.Method, req.URL.Path)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q, want the bearer token", got)
			}
		})
	}
}

func TestGetOperationStateReportsAFailedRequestAsAnError(t *testing.T) {
	t.Cleanup(UseEndpoint(hcUnroutableHost, &http.Client{Transport: failingTransport{}}))

	if state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/op-1"); err == nil {
		t.Fatalf("GetOperationState() = %+v, want an error", state)
	}
}

// TestGetOperationStateSendsTheTokenOnlyToARM: the URL of a pending import is read back
// from a resource's status, which is not proof that APIM issued it.
func TestGetOperationStateSendsTheTokenOnlyToARM(t *testing.T) {
	fake := newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
	})
	for _, operationURL := range []string{
		"https://attacker.example/operations/op-1",
		// Reachable, but not the ARM endpoint the package is configured for.
		fake.server.URL + "/operations/op-1",
		"http://arm.invalid.attacker.example/operations/op-1",
		"http://user@arm.invalid/operations/op-1",
		"",
	} {
		t.Run(operationURL, func(t *testing.T) {
			if _, err := GetOperationState(context.Background(), "tok", operationURL); !errors.Is(err, ErrNotOperationURL) {
				t.Errorf("GetOperationState() error = %v, want ErrNotOperationURL", err)
			}
		})
	}
	if got := fake.count(); got != 0 {
		t.Errorf("sent %d requests, want none", got)
	}
}
