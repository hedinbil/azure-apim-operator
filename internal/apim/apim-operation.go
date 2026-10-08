// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file reads an asynchronous APIM operation that a write left running when its wait
// ended (see waitForAsyncImportCompletion), so the caller can keep waiting for it across
// reconciles instead of sending another import on top of it.
package apim

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// OperationStatus is the state of an asynchronous APIM operation.
type OperationStatus string

const (
	// OperationRunning means APIM is still working on the operation.
	OperationRunning OperationStatus = "Running"
	// OperationSucceeded means the operation completed.
	OperationSucceeded OperationStatus = "Succeeded"
	// OperationFailed means APIM gave up on the operation; OperationState.Err says why.
	OperationFailed OperationStatus = "Failed"
	// OperationGone means APIM no longer knows the operation (404), for example because its
	// result has expired. Whether it succeeded is unknown.
	OperationGone OperationStatus = "Gone"
)

// OperationState is one reading of an asynchronous APIM operation.
type OperationState struct {
	Status OperationStatus
	// Err is why a failed operation failed: an *Error wrapping ErrAsyncOperationFailed for
	// an operation result of Failed or Canceled, or the *Error of the poll answer.
	Err error
}

// ErrNotOperationURL is returned for an operation URL that is not on Azure Resource Manager.
var ErrNotOperationURL = errors.New("not an Azure Resource Manager URL")

// IsOperationURL reports whether u is on the Azure Resource Manager endpoint this package
// talks to. It is the only place an operation URL may point: polling sends the operator's
// ARM token there, and a URL read back from a resource's status is not proof that APIM
// issued it.
func IsOperationURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Scheme+"://"+parsed.Host == armHost && parsed.User == nil
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

	resp, err := armRequest{
		operation: "import API (poll)",
		method:    http.MethodGet,
		url:       operationURL,
		token:     bearerToken,
	}.send(ctx)
	switch {
	case IsNotFound(err):
		return OperationState{Status: OperationGone}, nil
	case err != nil && isTransientPollError(err):
		return OperationState{}, err
	case err != nil:
		return OperationState{Status: OperationFailed, Err: err}, nil
	case resp.statusCode == http.StatusAccepted:
		return OperationState{Status: OperationRunning}, nil
	}

	// A terminal HTTP status without a status field means the operation is done.
	status := extractAsyncStatus(resp.body)
	switch strings.ToLower(status) {
	case "succeeded", "success", "":
		return OperationState{Status: OperationSucceeded}, nil
	case "failed", "canceled", "cancelled":
		code, detailCode, message := parseARMError(resp.body)
		return OperationState{Status: OperationFailed, Err: &Error{
			Operation:  "import API",
			Method:     http.MethodGet,
			Code:       code,
			DetailCode: detailCode,
			Message:    strings.TrimSpace("operation status " + status + ": " + message),
			Err:        ErrAsyncOperationFailed,
		}}, nil
	default:
		return OperationState{Status: OperationRunning}, nil
	}
}

// RunningOperation returns the URL of an asynchronous operation APIM accepted when err
// says the wait for it ended without learning its outcome: the wait timed out, or a poll
// failed without saying anything about the operation. Such an operation may still be
// running, and importing again would run a second import alongside it. Any other error,
// including a poll that failed for a reason about the operation itself, returns "".
func RunningOperation(err error) string {
	var apimErr *Error
	if !errors.As(err, &apimErr) || apimErr.OperationURL == "" {
		return ""
	}
	if errors.Is(err, ErrImportWaitTimeout) || isTransientPollError(apimErr) {
		return apimErr.OperationURL
	}
	return ""
}

// isTransientPollError reports whether a failed poll says nothing about the operation:
// the request did not get through, or APIM's management plane was throttled, overloaded
// or unreachable. Every code here was seen while imports were still running in Sep 2026.
func isTransientPollError(err error) bool {
	var apimErr *Error
	if !errors.As(err, &apimErr) {
		return true
	}
	if apimErr.StatusCode == http.StatusTooManyRequests || apimErr.StatusCode >= http.StatusInternalServerError {
		return true
	}
	switch apimErr.Code {
	case "ManagementApiRequestFailed", "Timeout":
		return true
	}
	return false
}
