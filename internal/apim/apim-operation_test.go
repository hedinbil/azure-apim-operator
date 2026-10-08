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

// TestImportStillRunningCarriesItsOperation pins the fix for the Sep 2026 incident: an
// import APIM accepted comes back at once with the operation URL, so the controller records
// that import and follows it instead of starting another one alongside it. Nothing in the
// write reads the operation, so no failed or slow read can lose it.
func TestImportStillRunningCarriesItsOperation(t *testing.T) {
	_, polls := asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/running"}})

	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if err != nil {
		t.Fatalf("import = %v, want nil", err)
	}
	if got, want := result.OperationURL, hcUnroutableHost+"/operations/running"; got != want {
		t.Errorf("OperationURL = %q, want %q", got, want)
	}
	if got := polls.Load(); got != 0 {
		t.Errorf("the import read its operation %d times, want 0", got)
	}
}

// TestFailedReadLeavesTheOperationRunning: a read the management endpoint fails says
// nothing about the import, which may well still be running. The answers are the ones
// apim-apim-dev-hedinit gave while imports were still running in Sep 2026. Each comes back
// as an error, never as a failed or forgotten operation, so the caller keeps the operation.
func TestFailedReadLeavesTheOperationRunning(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
	}{
		"management endpoint timed out":   {http.StatusUnprocessableEntity, `{"error":{"code":"Timeout","message":"Call to Management API timed out"}}`},
		"management endpoint unreachable": {http.StatusConflict, `{"error":{"code":"ManagementApiRequestFailed"}}`},
		"throttled":                       {http.StatusTooManyRequests, ``},
		"server error":                    {http.StatusServiceUnavailable, ``},
		"role assignment propagating":     {http.StatusUnauthorized, `{"error":{"code":"InvalidAuthenticationToken"}}`},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, answer.status, answer.body)
			})
			state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/unreadable")
			var apimErr *Error
			if !errors.As(err, &apimErr) || apimErr.StatusCode != answer.status {
				t.Fatalf("GetOperationState() = %+v, %v; want the %d as an *apim.Error", state, err, answer.status)
			}
			if state.Status != "" || state.Err != nil {
				t.Errorf("state = %+v alongside the error, want the zero state", state)
			}
		})
	}
}

// TestGetOperationStateClassifiesAPIMAnswers covers what a read of a pending import can
// say. A 2xx that says Failed or Canceled, or a 4xx carrying the operation's own error (a
// 400 ValidationError), is a failed operation; only a 404 is a forgotten one; any other
// failed reading comes back as an error.
func TestGetOperationStateClassifiesAPIMAnswers(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		want     OperationStatus
		wantErr  bool
		wantCode string
		// wantStatus is the HTTP status a failed operation keeps: 0 for a 2xx body that
		// says Failed, the 4xx for an answer that carries the operation's error.
		wantStatus int
	}{
		{name: "accepted", status: http.StatusAccepted, want: OperationRunning},
		{name: "accepted with a status", status: http.StatusAccepted, body: `{"status":"Succeeded"}`, want: OperationRunning},
		{name: "in progress", status: http.StatusOK, body: `{"status":"InProgress"}`, want: OperationRunning},
		{name: "unknown status", status: http.StatusOK, body: `{"status":"Updating"}`, want: OperationRunning},
		{name: "succeeded", status: http.StatusOK, body: `{"status":"Succeeded"}`, want: OperationSucceeded},
		{name: "success", status: http.StatusOK, body: `{"status":"success"}`, want: OperationSucceeded},
		{name: "done without status", status: http.StatusOK, body: `{}`, want: OperationSucceeded},
		{name: "201 without status", status: http.StatusCreated, body: `{"id":"x"}`, want: OperationSucceeded},
		{name: "204", status: http.StatusNoContent, want: OperationSucceeded},
		{name: "provisioning state", status: http.StatusOK, body: `{"properties":{"provisioningState":"Succeeded"}}`, want: OperationSucceeded},
		{
			name: "failed", status: http.StatusOK,
			body: `{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor"}}`,
			want: OperationFailed, wantCode: "InternalServerError",
		},
		{name: "canceled", status: http.StatusOK, body: `{"status":"Canceled"}`, want: OperationFailed},
		{name: "cancelled", status: http.StatusOK, body: `{"status":"cancelled"}`, want: OperationFailed},
		{name: "provisioning state failed", status: http.StatusOK, body: `{"properties":{"provisioningState":"Failed"}}`, want: OperationFailed},
		{name: "forgotten", status: http.StatusNotFound, body: `{"error":{"code":"ResourceNotFound"}}`, want: OperationGone},
		{
			name: "operation rejected the document", status: http.StatusBadRequest, body: `{"error":{"code":"ValidationError"}}`,
			want: OperationFailed, wantCode: "ValidationError", wantStatus: http.StatusBadRequest,
		},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"code":"InvalidAuthenticationToken"}}`, wantErr: true},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"code":"AuthorizationFailed"}}`, wantErr: true},
		{name: "management endpoint timed out", status: http.StatusUnprocessableEntity, body: `{"error":{"code":"Timeout"}}`, wantErr: true},
		{name: "management endpoint unreachable", status: http.StatusConflict, body: `{"error":{"code":"ManagementApiRequestFailed"}}`, wantErr: true},
		{name: "throttled", status: http.StatusTooManyRequests, wantErr: true},
		{name: "internal error", status: http.StatusInternalServerError, body: `{"error":{"code":"InternalServerError"}}`, wantErr: true},
		{name: "server error", status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tt.status, tt.body)
			})

			state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/op-1?api-version=2021-08-01")
			if got := fake.count(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
			req := fake.request(0)
			if req.Method != http.MethodGet || req.URL.Path != "/operations/op-1" {
				t.Errorf("read %s %s, want GET /operations/op-1", req.Method, req.URL.Path)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q, want the bearer token", got)
			}
			if tt.wantErr {
				var apimErr *Error
				if !errors.As(err, &apimErr) || apimErr.StatusCode != tt.status || apimErr.Method != http.MethodGet {
					t.Fatalf("GetOperationState() = %+v, %v; want an *apim.Error with %d from the GET", state, err, tt.status)
				}
				if errors.Is(err, ErrAsyncOperationFailed) {
					t.Errorf("a failed read must not look like a failed operation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetOperationState() error = %v", err)
			}
			if state.Status != tt.want {
				t.Errorf("Status = %s, want %s", state.Status, tt.want)
			}
			if tt.want != OperationFailed {
				if state.Err != nil {
					t.Errorf("Err = %v, want nil for %s", state.Err, state.Status)
				}
				return
			}
			var apimErr *Error
			if !errors.As(state.Err, &apimErr) || !errors.Is(state.Err, ErrAsyncOperationFailed) {
				t.Fatalf("Err = %v, want an *apim.Error wrapping ErrAsyncOperationFailed", state.Err)
			}
			if apimErr.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", apimErr.Code, tt.wantCode)
			}
			if apimErr.StatusCode != tt.wantStatus || apimErr.Method != http.MethodGet {
				t.Errorf("StatusCode/Method = %d/%s, want %d/GET for an operation result", apimErr.StatusCode, apimErr.Method, tt.wantStatus)
			}
			if apimErr.Operation != "APIM write of the API" {
				t.Errorf("Operation = %q, want APIM write of the API", apimErr.Operation)
			}
		})
	}
}

// TestOperationRead4xxClassification pins which error answers to an operation read are the
// operation's outcome and which are a failed reading. A 4xx is the operation's outcome
// (Failed, with the 4xx kept on the *Error) unless its status says the reading itself
// failed (401, 403, 404, 408, 409, 429) or its Azure code or first detail code says APIM
// or ARM was busy. Everything else, 5xx and 3xx included, is a failed reading.
func TestOperationRead4xxClassification(t *testing.T) {
	type outcome int
	const (
		failed  outcome = iota // the operation failed
		readErr                // the reading failed
		gone                   // 404
	)
	validation := `{"error":{"code":"ValidationError","message":"Parsing error(s): bad document"}}`
	busy := func(code string) string { return `{"error":{"code":"` + code + `","message":"busy"}}` }
	busyDetail := func(code string) string {
		return `{"error":{"code":"BadRequest","message":"m","details":[{"code":"` + code + `","message":"d"}]}}`
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   outcome
	}{
		// The operation's own error.
		{"400 ValidationError", http.StatusBadRequest, validation, failed},
		{"400 without a body", http.StatusBadRequest, ``, failed},
		{"400 plain text", http.StatusBadRequest, `bad document`, failed},
		{"405", http.StatusMethodNotAllowed, validation, failed},
		{"410", http.StatusGone, validation, failed},
		{"412", http.StatusPreconditionFailed, `{"error":{"code":"PreconditionFailed"}}`, failed},
		{"413", http.StatusRequestEntityTooLarge, validation, failed},
		{"415", http.StatusUnsupportedMediaType, validation, failed},
		{"422 ValidationError", http.StatusUnprocessableEntity, validation, failed},
		{"400 with a busy code as the message only", http.StatusBadRequest, `{"error":{"code":"ValidationError","message":"Timeout"}}`, failed},
		{"400 with a busy code in a later detail", http.StatusBadRequest,
			`{"error":{"code":"ValidationError","details":[{"code":"InvalidFormat"},{"code":"Timeout"}]}}`, failed},

		// Statuses that say the reading failed, whatever the body says.
		{"401 ValidationError", http.StatusUnauthorized, validation, readErr},
		{"403 ValidationError", http.StatusForbidden, validation, readErr},
		{"408 ValidationError", http.StatusRequestTimeout, validation, readErr},
		{"409 ValidationError", http.StatusConflict, validation, readErr},
		{"429 ValidationError", http.StatusTooManyRequests, validation, readErr},
		{"404", http.StatusNotFound, validation, gone},

		// Azure codes that say APIM or ARM was busy, as the code or the first detail's code.
		{"409 ManagementApiRequestFailed", http.StatusConflict, busy("ManagementApiRequestFailed"), readErr},
		{"422 ManagementApiRequestFailed", http.StatusUnprocessableEntity, busy("ManagementApiRequestFailed"), readErr},
		{"422 Timeout", http.StatusUnprocessableEntity, busy("Timeout"), readErr},
		{"400 ManagementApiRequestFailed", http.StatusBadRequest, busy("ManagementApiRequestFailed"), readErr},
		{"400 Timeout", http.StatusBadRequest, busy("Timeout"), readErr},
		{"400 Conflict", http.StatusBadRequest, busy("Conflict"), readErr},
		{"400 InternalServerError", http.StatusBadRequest, busy("InternalServerError"), readErr},
		{"400 ExpiredAuthenticationToken", http.StatusBadRequest, busy("ExpiredAuthenticationToken"), readErr},
		{"400 AuthorizationFailed", http.StatusBadRequest, busy("AuthorizationFailed"), readErr},
		{"400 detail ManagementApiRequestFailed", http.StatusBadRequest, busyDetail("ManagementApiRequestFailed"), readErr},
		{"422 detail Timeout", http.StatusUnprocessableEntity, busyDetail("Timeout"), readErr},
		{"400 top-level busy code", http.StatusBadRequest, `{"code":"Timeout","message":"slow"}`, readErr},

		// Not a 4xx.
		{"302", http.StatusFound, validation, readErr},
		{"500 ValidationError", http.StatusInternalServerError, validation, readErr},
		{"502", http.StatusBadGateway, validation, readErr},
		{"503", http.StatusServiceUnavailable, validation, readErr},
		{"504", http.StatusGatewayTimeout, validation, readErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, tc.status, tc.body)
			})
			state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/op-4xx")
			switch tc.want {
			case gone:
				if err != nil || state.Status != OperationGone || state.Err != nil {
					t.Fatalf("GetOperationState() = %+v, %v; want Gone", state, err)
				}
			case readErr:
				var apimErr *Error
				if !errors.As(err, &apimErr) || apimErr.StatusCode != tc.status {
					t.Fatalf("GetOperationState() = %+v, %v; want the %d as an *apim.Error", state, err, tc.status)
				}
				if state != (OperationState{}) {
					t.Errorf("state = %+v alongside the error, want the zero state", state)
				}
				if apimErr.Operation != "read APIM operation" || apimErr.Method != http.MethodGet {
					t.Errorf("Operation/Method = %q/%q, want read APIM operation/GET", apimErr.Operation, apimErr.Method)
				}
				if errors.Is(err, ErrAsyncOperationFailed) {
					t.Errorf("a failed reading must not look like a failed operation: %v", err)
				}
			case failed:
				if err != nil {
					t.Fatalf("GetOperationState() error = %v, want the failure in the state", err)
				}
				if state.Status != OperationFailed {
					t.Fatalf("Status = %s, want Failed", state.Status)
				}
				var apimErr *Error
				if !errors.As(state.Err, &apimErr) {
					t.Fatalf("Err = %v (%T), want *apim.Error", state.Err, state.Err)
				}
				if !errors.Is(state.Err, ErrAsyncOperationFailed) || errors.Is(state.Err, ErrImportWaitTimeout) {
					t.Errorf("Err = %v, want it to wrap ErrAsyncOperationFailed only", state.Err)
				}
				if apimErr.StatusCode != tc.status || apimErr.Method != http.MethodGet {
					t.Errorf("StatusCode/Method = %d/%s, want %d/GET", apimErr.StatusCode, apimErr.Method, tc.status)
				}
				if apimErr.Operation != "APIM write of the API" {
					t.Errorf("Operation = %q, want APIM write of the API", apimErr.Operation)
				}
				if IsNotFound(state.Err) {
					t.Errorf("a failed operation is not a 404: %v", state.Err)
				}
			}
		})
	}
}

// TestOperationRejectedRendersTheOutcome: the error of an operation APIM rejected names the
// write, the status, the sentinel and the Azure error, in that order, for the resource
// status the controller writes it to.
func TestOperationRejectedRendersTheOutcome(t *testing.T) {
	newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest,
			`{"error":{"code":"ValidationError","message":"One or more fields contain incorrect values:","details":[{"code":"ValidationError","message":"Parsing error(s): JSON is invalid."}]}}`)
	})
	state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/op-render")
	if err != nil || state.Status != OperationFailed {
		t.Fatalf("GetOperationState() = %+v, %v; want Failed", state, err)
	}
	want := "APIM write of the API failed: 400 Bad Request: APIM async operation failed: ValidationError: " +
		"One or more fields contain incorrect values: Parsing error(s): JSON is invalid."
	if got := state.Err.Error(); got != want {
		t.Errorf("Error() = %q\nwant      %q", got, want)
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
		"https://arm.invalid/operations/op-1",
		"http://arm.invalid:8080/operations/op-1",
		"/operations/op-1",
		"operations/op-1",
		"http://arm.invalid/%zz",
		"",
	} {
		t.Run(operationURL, func(t *testing.T) {
			state, err := GetOperationState(context.Background(), "tok", operationURL)
			if !errors.Is(err, ErrNotOperationURL) {
				t.Errorf("GetOperationState() = %+v, %v; want ErrNotOperationURL", state, err)
			}
		})
	}
	if got := fake.count(); got != 0 {
		t.Errorf("sent %d requests, want none", got)
	}
	if got := fake.router.sent.Load(); got != 0 {
		t.Errorf("httpClient sent %d requests, want none", got)
	}
}

// withARMHost points armHost at host for one test without a fake behind it, for the checks
// that never send a request.
func withARMHost(t *testing.T, host string) {
	t.Helper()
	t.Cleanup(UseEndpoint(host, httpClient))
}

// TestIsOperationURL pins what counts as the ARM endpoint: the same origin as armHost,
// written in any case and with or without the default port, and nothing else.
func TestIsOperationURL(t *testing.T) {
	withARMHost(t, "https://management.azure.com")
	cases := []struct {
		url  string
		want bool
	}{
		{"https://management.azure.com/subscriptions/s/providers/Microsoft.ApiManagement/operations/x?api-version=2021-08-01", true},
		{"https://management.azure.com", true},
		{"HTTPS://MANAGEMENT.AZURE.COM/operations/x", true},
		{"https://Management.Azure.Com/operations/x", true},
		{"https://management.azure.com:443/operations/x", true},
		{"https://management.azure.com:8443/operations/x", false},
		{"http://management.azure.com/operations/x", false},
		{"http://management.azure.com:443/operations/x", false},
		{"https://user@management.azure.com/operations/x", false},
		{"https://user:pass@management.azure.com/operations/x", false},
		{"https://management.azure.com@evil.example/operations/x", false},
		{"https://evil.example/operations/x", false},
		{"https://management.azure.com.evil.example/operations/x", false},
		{"https://evilmanagement.azure.com/operations/x", false},
		{"https://management.azure.co/operations/x", false},
		{"//management.azure.com/operations/x", false},
		{"/operations/x", false},
		{"management.azure.com/operations/x", false},
		{"https://management.azure.com/%zz", false},
		{"https://[::1", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := IsOperationURL(tc.url); got != tc.want {
				t.Errorf("IsOperationURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

// TestIsOperationURLFollowsTheConfiguredHost: the test endpoints (plain http, an explicit
// port) are compared the same way as the real one.
func TestIsOperationURLFollowsTheConfiguredHost(t *testing.T) {
	withARMHost(t, "http://127.0.0.1:8080")
	for u, want := range map[string]bool{
		"http://127.0.0.1:8080/operations/x":  true,
		"HTTP://127.0.0.1:8080/operations/x":  true,
		"http://127.0.0.1/operations/x":       false,
		"http://127.0.0.1:8081/operations/x":  false,
		"https://127.0.0.1:8080/operations/x": false,
		"https://management.azure.com/x":      false,
	} {
		if got := IsOperationURL(u); got != want {
			t.Errorf("IsOperationURL(%q) with armHost %s = %v, want %v", u, armHost, got, want)
		}
	}

	withARMHost(t, "http://arm.invalid:80")
	if !IsOperationURL("http://arm.invalid/operations/x") {
		t.Error("the default http port must be ignored on armHost as well")
	}
}

// TestAcceptedWrite pins how a 202's headers become a WriteResult: Azure-AsyncOperation
// before Location, a relative URL resolved against the ARM host, Retry-After passed on, and
// anything that does not lead to ARM refused with ErrNoOperationURL.
func TestAcceptedWrite(t *testing.T) {
	withARMHost(t, "https://management.azure.com")
	const arm = "https://management.azure.com"
	cases := []struct {
		name       string
		header     http.Header
		want       string // the OperationURL; "" means ErrNoOperationURL
		retryAfter time.Duration
	}{
		{name: "Azure-AsyncOperation", header: http.Header{"Azure-Asyncoperation": {arm + "/operations/a"}}, want: arm + "/operations/a"},
		{name: "Location", header: http.Header{"Location": {arm + "/operations/l"}}, want: arm + "/operations/l"},
		{name: "Azure-AsyncOperation preferred over Location", header: http.Header{
			"Azure-Asyncoperation": {arm + "/operations/preferred"},
			"Location":             {arm + "/operations/ignored"},
		}, want: arm + "/operations/preferred"},
		{name: "Azure-AsyncOperation preferred even when Location is relative", header: http.Header{
			"Azure-Asyncoperation": {arm + "/operations/preferred"},
			"Location":             {"/operations/ignored"},
		}, want: arm + "/operations/preferred"},
		{name: "blank Azure-AsyncOperation falls back to Location", header: http.Header{
			"Azure-Asyncoperation": {"  "},
			"Location":             {arm + "/operations/fallback"},
		}, want: arm + "/operations/fallback"},
		{name: "relative Location resolved", header: http.Header{"Location": {"/operations/rel?api-version=2021-08-01"}},
			want: arm + "/operations/rel?api-version=2021-08-01"},
		{name: "relative Azure-AsyncOperation resolved", header: http.Header{"Azure-Asyncoperation": {" /operations/rel "}},
			want: arm + "/operations/rel"},
		{name: "explicit default port", header: http.Header{"Location": {"https://management.azure.com:443/operations/p"}},
			want: "https://management.azure.com:443/operations/p"},
		{name: "Retry-After seconds", header: http.Header{"Location": {"/operations/r"}, "Retry-After": {"15"}},
			want: arm + "/operations/r", retryAfter: 15 * time.Second},
		{name: "Retry-After garbage", header: http.Header{"Location": {"/operations/r"}, "Retry-After": {"soon"}},
			want: arm + "/operations/r"},

		{name: "no header", header: http.Header{}},
		{name: "foreign host", header: http.Header{"Azure-Asyncoperation": {"https://evil.example/operations/x"}}},
		{name: "lookalike host", header: http.Header{"Location": {"https://management.azure.com.evil.example/operations/x"}}},
		{name: "http scheme", header: http.Header{"Location": {"http://management.azure.com/operations/x"}}},
		{name: "userinfo", header: http.Header{"Location": {"https://user@management.azure.com/operations/x"}}},
		{name: "other port", header: http.Header{"Location": {"https://management.azure.com:8443/operations/x"}}},
		{name: "relative without a leading slash", header: http.Header{"Location": {"operations/x"}}},
		{name: "a foreign Azure-AsyncOperation is not replaced by Location", header: http.Header{
			"Azure-Asyncoperation": {"https://evil.example/operations/x"},
			"Location":             {arm + "/operations/ok"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := acceptedWrite("import API", http.MethodPut, tc.header)
			if tc.want == "" {
				if !errors.Is(err, ErrNoOperationURL) {
					t.Fatalf("acceptedWrite() = %+v, %v; want ErrNoOperationURL", result, err)
				}
				var apimErr *Error
				if !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusAccepted ||
					apimErr.Method != http.MethodPut || apimErr.Operation != "import API" {
					t.Errorf("err = %+v, want an *apim.Error for the PUT with status 202", apimErr)
				}
				if result.Accepted() {
					t.Errorf("WriteResult = %+v alongside the error, want empty", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("acceptedWrite() error = %v", err)
			}
			if !result.Accepted() || result.OperationURL != tc.want {
				t.Errorf("OperationURL = %q, want %q", result.OperationURL, tc.want)
			}
			if result.RetryAfter != tc.retryAfter {
				t.Errorf("RetryAfter = %s, want %s", result.RetryAfter, tc.retryAfter)
			}
		})
	}
}

func TestWriteResultAccepted(t *testing.T) {
	if (WriteResult{}).Accepted() {
		t.Error("an empty WriteResult means APIM finished the write")
	}
	if (WriteResult{RetryAfter: time.Second}).Accepted() {
		t.Error("a Retry-After alone does not make a write accepted")
	}
	if !(WriteResult{OperationURL: "https://management.azure.com/operations/x"}).Accepted() {
		t.Error("a WriteResult with an operation URL is accepted")
	}
}
