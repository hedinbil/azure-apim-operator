// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file covers APIM's asynchronous writes: a PUT that APIM accepts with 202 and keeps
// working on, and the operation URL it hands back for the caller to poll.
package apim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// UpsertResult is APIM's answer to a PUT that creates, updates or imports an API.
type UpsertResult struct {
	// OperationURL is set when APIM accepted the request (202) and is still working on
	// it. The API must not be written again until GetOperationState reports the
	// operation finished: APIM does not refuse a second import of the same API, it runs
	// it alongside the first, and on a single Developer unit a pile of large imports
	// starves both the management endpoint and the gateway.
	OperationURL string
}

// Pending reports whether APIM is still processing the request.
func (r UpsertResult) Pending() bool {
	return r.OperationURL != ""
}

// OperationStatus is the state of an asynchronous APIM operation.
type OperationStatus string

const (
	// OperationRunning means APIM is still working on the operation.
	OperationRunning OperationStatus = "Running"
	// OperationSucceeded means the operation completed.
	OperationSucceeded OperationStatus = "Succeeded"
	// OperationFailed means APIM gave up on the operation; OperationState.Detail says why.
	OperationFailed OperationStatus = "Failed"
	// OperationGone means APIM no longer knows the operation (404), for example because its
	// result has expired. Whether it succeeded is unknown.
	OperationGone OperationStatus = "Gone"
)

// OperationState is one reading of an asynchronous APIM operation.
type OperationState struct {
	Status OperationStatus
	// Detail is APIM's response for a failed operation.
	Detail string
}

// ErrNotOperationURL is returned for an operation URL that is not on Azure Resource Manager.
var ErrNotOperationURL = errors.New("not an Azure Resource Manager URL")

// IsOperationURL reports whether u is an https URL on the Azure Resource Manager host. It is
// the only place an operation URL may point: polling sends the operator's ARM token there, and
// a URL read back from a resource's status is not proof that APIM issued it.
func IsOperationURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Scheme+"://"+parsed.Host == armHost && parsed.User == nil
}

// asyncOperationURL returns where to poll a request APIM accepted with 202, or "" when APIM
// gave no usable URL.
func asyncOperationURL(resp *http.Response) string {
	operationURL := strings.TrimSpace(resp.Header.Get("Azure-AsyncOperation"))
	if operationURL == "" {
		operationURL = strings.TrimSpace(resp.Header.Get("Location"))
	}
	if strings.HasPrefix(operationURL, "/") {
		operationURL = armHost + operationURL
	}
	if !IsOperationURL(operationURL) {
		return ""
	}
	return operationURL
}

// GetOperationState reads an asynchronous APIM operation once.
//
// An error means the reading failed, not the operation, and the caller should read it again
// later rather than start over: the Developer tier's management endpoint regularly answers
// 409 or 422 (ManagementApiRequestFailed, Timeout) while the operation itself carries on.
func GetOperationState(ctx context.Context, bearerToken, operationURL string) (OperationState, error) {
	if !IsOperationURL(operationURL) {
		return OperationState{}, fmt.Errorf("poll APIM operation %q: %w", operationURL, ErrNotOperationURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, operationURL, nil)
	if err != nil {
		return OperationState{}, fmt.Errorf("build APIM operation poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return OperationState{}, fmt.Errorf("poll APIM operation: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return OperationState{}, fmt.Errorf("read APIM operation poll response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusAccepted:
		return OperationState{Status: OperationRunning}, nil
	case resp.StatusCode == http.StatusNotFound:
		return OperationState{Status: OperationGone}, nil
	case isTransientManagementError(resp.StatusCode, body):
		return OperationState{}, fmt.Errorf("poll APIM operation: %s\n%s", resp.Status, string(body))
	case resp.StatusCode >= 300:
		return OperationState{Status: OperationFailed, Detail: fmt.Sprintf("%s\n%s", resp.Status, string(body))}, nil
	}

	// A terminal HTTP status without a status field means the operation is done.
	switch strings.ToLower(extractAsyncStatus(body)) {
	case "succeeded", "success", "":
		return OperationState{Status: OperationSucceeded}, nil
	case "failed", "canceled", "cancelled":
		return OperationState{Status: OperationFailed, Detail: string(body)}, nil
	default:
		return OperationState{Status: OperationRunning}, nil
	}
}

// isTransientManagementError reports whether an error response says APIM's management plane
// was unreachable or overloaded, rather than that the request or operation was wrong.
func isTransientManagementError(statusCode int, body []byte) bool {
	if statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError {
		return true
	}
	if statusCode != http.StatusConflict && statusCode != http.StatusUnprocessableEntity {
		return false
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	switch payload.Error.Code {
	case "ManagementApiRequestFailed", "Timeout":
		return true
	}
	return false
}
