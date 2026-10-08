// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file reads an asynchronous APIM operation that a write started (see WriteResult), so
// the caller can follow it across reconciles instead of sending another write on top of it.
package apim

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// OperationStatus is the state of an asynchronous APIM operation.
type OperationStatus string

const (
	// OperationRunning means APIM is still working on the operation.
	OperationRunning OperationStatus = "Running"
	// OperationSucceeded means the operation completed.
	OperationSucceeded OperationStatus = "Succeeded"
	// OperationFailed means APIM reported that the operation ended Failed or Canceled;
	// OperationState.Err says why.
	OperationFailed OperationStatus = "Failed"
	// OperationGone means APIM no longer knows the operation (404), for example because its
	// result has expired. Whether it succeeded is unknown.
	OperationGone OperationStatus = "Gone"
)

// OperationState is one reading of an asynchronous APIM operation.
type OperationState struct {
	Status OperationStatus
	// Err is why a failed operation failed: an *Error wrapping ErrAsyncOperationFailed.
	Err error
}

// ErrNotOperationURL is returned for an operation URL that is not on Azure Resource Manager.
var ErrNotOperationURL = errors.New("not an Azure Resource Manager URL")

// GetOperationState reads an asynchronous APIM operation once.
//
// A failed operation is a 2xx answer that says Failed or Canceled, or, as ARM's Location
// contract has it, a 4xx answer that carries the operation's own error (a 400
// ValidationError for a document APIM rejected). A 404 says APIM no longer knows the
// operation. Every other failure (a timeout, a reset connection, 5xx, 408, 409, 429, 401
// or 403 while a token expires or a role assignment propagates, and any answer whose Azure
// code says APIM was busy) is returned as an error: the reading failed, not the operation,
// and the caller reads it again later rather than start over. The Developer tier's
// management endpoint regularly answers 409 or 422 (ManagementApiRequestFailed, Timeout)
// while the operation itself carries on.
func GetOperationState(ctx context.Context, bearerToken, operationURL string) (OperationState, error) {
	if !IsOperationURL(operationURL) {
		return OperationState{}, fmt.Errorf("read APIM operation %q: %w", operationURL, ErrNotOperationURL)
	}

	resp, err := armRequest{
		operation: "read APIM operation",
		method:    http.MethodGet,
		url:       operationURL,
		token:     bearerToken,
	}.send(ctx)
	var httpErr *Error
	switch {
	case IsNotFound(err):
		return OperationState{Status: OperationGone}, nil
	case errors.As(err, &httpErr) && operationRejected(httpErr):
		httpErr.Operation = operationLabel
		httpErr.Err = ErrAsyncOperationFailed
		return OperationState{Status: OperationFailed, Err: httpErr}, nil
	case err != nil:
		return OperationState{}, err
	case resp.statusCode == http.StatusAccepted:
		return OperationState{Status: OperationRunning}, nil
	}

	// A terminal HTTP status without a status field means the operation is done: a Location
	// URL answers with the finished resource itself.
	status := extractAsyncStatus(resp.body)
	switch strings.ToLower(status) {
	case "succeeded", "success", "":
		return OperationState{Status: OperationSucceeded}, nil
	case "failed", "canceled", "cancelled":
		code, detailCode, message := parseARMError(resp.body)
		return OperationState{Status: OperationFailed, Err: &Error{
			Operation:  operationLabel,
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

// operationLabel names a failed operation in its error. The operation may be an import or
// a websocket API upsert; reading it cannot tell which.
const operationLabel = "APIM write of the API"

// busyCodes are Azure error codes that say APIM or ARM could not answer, not that the
// operation failed.
var busyCodes = map[string]bool{
	"ManagementApiRequestFailed": true,
	"Timeout":                    true,
	"Conflict":                   true,
	"InternalServerError":        true,
	"ExpiredAuthenticationToken": true,
	"AuthorizationFailed":        true,
}

// operationRejected reports whether an error answer to an operation read is the outcome of
// the operation (see GetOperationState) rather than a failed reading.
func operationRejected(e *Error) bool {
	if e.StatusCode < 400 || e.StatusCode >= 500 || busyCodes[e.Code] || busyCodes[e.DetailCode] {
		return false
	}
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	}
	return true
}
