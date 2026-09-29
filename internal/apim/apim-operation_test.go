package apim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// cannedResponse is one answer from a scriptedTransport.
type cannedResponse struct {
	status int
	header http.Header
	body   string
}

// scriptedTransport stands in for ARM across several requests: it answers each
// request with the next canned response and records every request it saw.
type scriptedTransport struct {
	responses []cannedResponse
	requests  []*http.Request
}

func (st *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	st.requests = append(st.requests, req)
	if len(st.requests) > len(st.responses) {
		return nil, fmt.Errorf("unexpected request %d: %s %s", len(st.requests), req.Method, req.URL)
	}
	canned := st.responses[len(st.requests)-1]
	header := canned.header
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: canned.status,
		Status:     fmt.Sprintf("%d %s", canned.status, http.StatusText(canned.status)),
		Body:       io.NopCloser(strings.NewReader(canned.body)),
		Header:     header,
		Request:    req,
	}, nil
}

// headerWith builds a response header the way net/http reads it back.
func headerWith(key, value string) http.Header {
	header := http.Header{}
	header.Set(key, value)
	return header
}

// failingTransport fails every request, as a dropped connection does.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection reset by peer")
}

const testOperationURL = "https://management.azure.com/subscriptions/sub-1/resourceGroups/rg-1" +
	"/providers/Microsoft.ApiManagement/service/apim-1/operationresults/op-1?api-version=2021-08-01"

func deploymentConfig() APIMDeploymentConfig {
	return APIMDeploymentConfig{
		SubscriptionID: "sub-1", ResourceGroup: "rg-1", ServiceName: "apim-1",
		APIID: "api-1", RoutePrefix: "/api", BearerToken: "tok",
	}
}

// apiNotFound answers the If-Match lookup that precedes every import.
var apiNotFound = cannedResponse{status: http.StatusNotFound}

// TestImportReturnsTheOperationAPIMIsStillRunning pins the fix for the 2026-09
// incident: an import APIM accepts with 202 is handed back to the caller to
// track, not waited on and then abandoned, so nothing imports on top of it.
func TestImportReturnsTheOperationAPIMIsStillRunning(t *testing.T) {
	st := &scriptedTransport{responses: []cannedResponse{
		apiNotFound,
		{status: http.StatusAccepted, header: headerWith("Azure-AsyncOperation", testOperationURL)},
	}}
	withTransport(t, st)

	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if err != nil {
		t.Fatalf("ImportOpenAPIDefinitionToAPIM() error = %v", err)
	}
	if !result.Pending() || result.OperationURL != testOperationURL {
		t.Errorf("result = %+v, want pending on %s", result, testOperationURL)
	}
	if len(st.requests) != 2 || st.requests[1].Method != http.MethodPut {
		t.Fatalf("requests = %d, want the lookup and the PUT and no polling", len(st.requests))
	}
	if got := st.requests[1].Header.Get("If-Match"); got != "*" {
		t.Errorf("If-Match = %q, want * for an API that does not exist yet", got)
	}
}

func TestImportResolvesARelativeLocationAgainstARM(t *testing.T) {
	relative := strings.TrimPrefix(testOperationURL, armHost)
	withTransport(t, &scriptedTransport{responses: []cannedResponse{
		apiNotFound,
		{status: http.StatusAccepted, header: headerWith("Location", relative)},
	}})

	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if err != nil {
		t.Fatalf("ImportOpenAPIDefinitionToAPIM() error = %v", err)
	}
	if result.OperationURL != testOperationURL {
		t.Errorf("OperationURL = %q, want %q", result.OperationURL, testOperationURL)
	}
}

// TestImportIsDoneWhenAPIMGivesNoUsableOperationURL keeps the old behaviour for a
// 202 that cannot be tracked, and never keeps a URL off ARM: it would later get
// the operator's ARM token.
func TestImportIsDoneWhenAPIMGivesNoUsableOperationURL(t *testing.T) {
	for name, header := range map[string]http.Header{
		"no header":     {},
		"not ARM":       headerWith("Location", "https://attacker.example/operationresults/op-1"),
		"plain http":    headerWith("Location", strings.Replace(testOperationURL, "https://", "http://", 1)),
		"lookalike ARM": headerWith("Location", "https://management.azure.com.attacker.example/op-1"),
	} {
		t.Run(name, func(t *testing.T) {
			st := &scriptedTransport{responses: []cannedResponse{apiNotFound, {status: http.StatusAccepted, header: header}}}
			withTransport(t, st)

			result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
			if err != nil {
				t.Fatalf("ImportOpenAPIDefinitionToAPIM() error = %v", err)
			}
			if result.Pending() {
				t.Errorf("result = %+v, want not pending", result)
			}
			if len(st.requests) != 2 {
				t.Errorf("requests = %d, want 2", len(st.requests))
			}
		})
	}
}

func TestImportIsDoneWhenAPIMAnswersSynchronously(t *testing.T) {
	withTransport(t, &scriptedTransport{responses: []cannedResponse{apiNotFound, {status: http.StatusCreated}}})

	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if err != nil {
		t.Fatalf("ImportOpenAPIDefinitionToAPIM() error = %v", err)
	}
	if result.Pending() {
		t.Errorf("result = %+v, want not pending", result)
	}
}

func TestImportReportsAnAPIMError(t *testing.T) {
	withTransport(t, &scriptedTransport{responses: []cannedResponse{
		apiNotFound,
		{status: http.StatusBadRequest, body: `{"error":{"code":"ValidationError"}}`},
	}})

	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "ValidationError") {
		t.Fatalf("ImportOpenAPIDefinitionToAPIM() error = %v, want APIM's ValidationError", err)
	}
	if result.Pending() {
		t.Errorf("result = %+v, want not pending", result)
	}
}

// TestGetOperationStateClassifiesAPIMAnswers covers what a poll can say. The 409
// and 422 bodies are the ones apim-apim-dev-hedinit returned while imports were
// still running: they describe the management endpoint, not the import.
func TestGetOperationStateClassifiesAPIMAnswers(t *testing.T) {
	tests := []struct {
		name       string
		response   cannedResponse
		want       OperationStatus
		wantErr    bool
		wantDetail string
	}{
		{name: "accepted", response: cannedResponse{status: http.StatusAccepted}, want: OperationRunning},
		{name: "in progress", response: cannedResponse{status: http.StatusOK, body: `{"status":"InProgress"}`}, want: OperationRunning},
		{name: "succeeded", response: cannedResponse{status: http.StatusOK, body: `{"status":"Succeeded"}`}, want: OperationSucceeded},
		{name: "done without status", response: cannedResponse{status: http.StatusOK}, want: OperationSucceeded},
		{
			name:     "provisioning state",
			response: cannedResponse{status: http.StatusOK, body: `{"properties":{"provisioningState":"Succeeded"}}`},
			want:     OperationSucceeded,
		},
		{
			name:       "failed",
			response:   cannedResponse{status: http.StatusOK, body: `{"status":"Failed","error":{"code":"InternalServerError"}}`},
			want:       OperationFailed,
			wantDetail: "InternalServerError",
		},
		{name: "forgotten", response: cannedResponse{status: http.StatusNotFound}, want: OperationGone},
		{
			name:       "rejected",
			response:   cannedResponse{status: http.StatusBadRequest, body: `{"error":{"code":"ValidationError"}}`},
			want:       OperationFailed,
			wantDetail: "ValidationError",
		},
		{
			name:     "management endpoint timed out",
			response: cannedResponse{status: http.StatusUnprocessableEntity, body: `{"error":{"code":"Timeout","message":"Call to Management API timed out"}}`},
			wantErr:  true,
		},
		{
			name:     "management endpoint unreachable",
			response: cannedResponse{status: http.StatusConflict, body: `{"error":{"code":"ManagementApiRequestFailed"}}`},
			wantErr:  true,
		},
		{name: "throttled", response: cannedResponse{status: http.StatusTooManyRequests}, wantErr: true},
		{name: "server error", response: cannedResponse{status: http.StatusServiceUnavailable}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &scriptedTransport{responses: []cannedResponse{tt.response}}
			withTransport(t, st)

			state, err := GetOperationState(context.Background(), "tok", testOperationURL)
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
			if !strings.Contains(state.Detail, tt.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", state.Detail, tt.wantDetail)
			}
			if got := st.requests[0].Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q, want the bearer token", got)
			}
			if got := st.requests[0].URL.String(); got != testOperationURL {
				t.Errorf("polled %s, want %s", got, testOperationURL)
			}
		})
	}
}

func TestGetOperationStateReportsAFailedRequestAsAnError(t *testing.T) {
	withTransport(t, failingTransport{})

	if state, err := GetOperationState(context.Background(), "tok", testOperationURL); err == nil {
		t.Fatalf("GetOperationState() = %+v, want an error", state)
	}
}

// TestGetOperationStateSendsTheTokenOnlyToARM: the URL may come from a resource's
// status, which is not proof that APIM issued it.
func TestGetOperationStateSendsTheTokenOnlyToARM(t *testing.T) {
	for _, operationURL := range []string{
		"https://attacker.example/operationresults/op-1",
		strings.Replace(testOperationURL, "https://", "http://", 1),
		"https://management.azure.com.attacker.example/op-1",
		"https://user@management.azure.com/op-1",
		"",
	} {
		t.Run(operationURL, func(t *testing.T) {
			st := &scriptedTransport{}
			withTransport(t, st)

			_, err := GetOperationState(context.Background(), "tok", operationURL)
			if !errors.Is(err, ErrNotOperationURL) {
				t.Errorf("GetOperationState() error = %v, want ErrNotOperationURL", err)
			}
			if len(st.requests) != 0 {
				t.Errorf("sent %d requests, want none", len(st.requests))
			}
		})
	}
}
