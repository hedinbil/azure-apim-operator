// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file holds the one request helper every ARM call in the package goes through, and
// the typed error it returns, so a controller can tell a throttled or overloaded APIM from
// a request APIM will never accept.
package apim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrImportWaitTimeout marks an operation APIM accepted (202) and still reported as running
// after the caller stopped waiting for it. It may still complete in APIM; the caller treats
// it as transient. Test with errors.Is.
var ErrImportWaitTimeout = errors.New("timed out waiting for APIM async operation")

// ErrAsyncOperationFailed marks an *Error that came from the result of an asynchronous
// operation (status Failed or Canceled) rather than from an HTTP status. Test with errors.Is.
var ErrAsyncOperationFailed = errors.New("APIM async operation failed")

// ErrDependencyNotFound marks an *Error for a 404 on a write whose target hangs off a
// resource another custom resource creates: the product an API is assigned to, the tag
// it gets, or the API (or operation) a policy is set on. Such a 404 usually means that
// resource is not in APIM yet, because its own resource has not been reconciled or is
// still backing off, and it clears up once it is. The controllers do not watch each
// other, so the write has to be retried rather than given up on. Test with errors.Is.
var ErrDependencyNotFound = errors.New("a resource this write depends on is not in APIM yet")

// contentTypeJSON is the content type of every JSON body this package sends.
const contentTypeJSON = "application/json"

// maxErrorBodyInMessage bounds how much of an error body or Azure error message ends up in
// an error message, and from there in the resource status and the logs.
const maxErrorBodyInMessage = 1024

// maxResponseBody bounds an ARM response read into memory. Nothing this package reads comes
// near it; it only keeps a misbehaving endpoint from filling the operator's memory.
const maxResponseBody = 4 << 20

// Error is a failed ARM call: a non-2xx answer, an async operation that ended Failed or
// Canceled, or an async wait that timed out. Transport failures (DNS, TLS, connection
// reset) are not wrapped in it; they come back as the *url.Error net/http returns.
type Error struct {
	// Operation is what was being done, e.g. "import API" or "assign API to product p1".
	Operation string
	// Method is the HTTP method of the request that failed.
	Method string
	// StatusCode is the HTTP status ARM answered with. Zero when the failure came from
	// an async operation result or a wait timeout rather than an HTTP status.
	StatusCode int
	// Code is the Azure error code from the body (error.code), e.g. "PreconditionFailed".
	Code string
	// DetailCode is the first detail's code (error.details[0].code), when there is one.
	DetailCode string
	// Message is the Azure error message, or the raw body when it was not ARM's JSON shape.
	Message string
	// Err is a sentinel this error wraps: ErrImportWaitTimeout, ErrAsyncOperationFailed,
	// ErrDependencyNotFound or ErrNoOperationURL.
	Err error
}

// Error renders the operation, status and Azure code first so a status message or log
// line says what failed and how before the (possibly long) message.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Operation)
	b.WriteString(" failed")
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, ": %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if e.Code != "" {
		b.WriteString(": ")
		b.WriteString(e.Code)
		if e.DetailCode != "" && e.DetailCode != e.Code {
			b.WriteString("/")
			b.WriteString(e.DetailCode)
		}
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Unwrap exposes the sentinel, so errors.Is(err, ErrImportWaitTimeout) works through it.
func (e *Error) Unwrap() error { return e.Err }

// IsNotFound reports whether err is an ARM 404.
func IsNotFound(err error) bool {
	var apimErr *Error
	return errors.As(err, &apimErr) && apimErr.StatusCode == http.StatusNotFound
}

// armRequest is one call to ARM. Everything in this package builds one of these and sends
// it, so authentication, body handling and error shaping live in one place.
type armRequest struct {
	// operation names the call in errors, e.g. "upsert tag".
	operation string
	method    string
	url       string
	token     string
	// body is sent as is; nil sends no body.
	body []byte
	// contentType is set when non-empty.
	contentType string
	// ifMatch is set when non-empty.
	ifMatch string
	// dependent marks a write to a path under a resource another custom resource creates
	// (a product assignment, a tag assignment, a policy). A 404 on it wraps
	// ErrDependencyNotFound, so it is retried instead of given up on.
	dependent bool
}

// armResponse is a completed response with its body already read and closed.
type armResponse struct {
	statusCode int
	status     string
	header     http.Header
	body       []byte
}

// send performs the request through httpClient. A non-2xx answer comes back as *Error
// together with the response, so a caller that expects a 404 can still look at it.
func (r armRequest) send(ctx context.Context) (*armResponse, error) {
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", r.operation, err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	if r.ifMatch != "" {
		req.Header.Set("If-Match", r.ifMatch)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.operation, err)
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if readErr == nil && len(respBody) > maxResponseBody {
		readErr = fmt.Errorf("response larger than %d bytes", maxResponseBody)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		logger.Error(closeErr, "⚠️ Failed to close response body", "operation", r.operation)
	}
	if readErr != nil {
		return nil, fmt.Errorf("%s: read response body: %w", r.operation, readErr)
	}

	out := &armResponse{
		statusCode: resp.StatusCode,
		status:     resp.Status,
		header:     resp.Header,
		body:       respBody,
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpErr := newHTTPError(r.operation, r.method, resp.StatusCode, respBody)
		if r.dependent && resp.StatusCode == http.StatusNotFound {
			httpErr.Err = ErrDependencyNotFound
		}
		return out, httpErr
	}
	return out, nil
}

// newHTTPError builds the *Error for a non-2xx answer from its body.
func newHTTPError(operation, method string, statusCode int, body []byte) *Error {
	code, detailCode, message := parseARMError(body)
	return &Error{
		Operation:  operation,
		Method:     method,
		StatusCode: statusCode,
		Code:       code,
		DetailCode: detailCode,
		Message:    message,
	}
}

// armErrorBody is the error shape ARM and APIM use, both for a failed request and inside
// a failed async operation result. Some resource providers leave out the "error" wrapper,
// so the top-level code and message are read as well.
type armErrorBody struct {
	Error *armErrorDetail `json:"error"`
	armErrorDetail
}

type armErrorDetail struct {
	Code    string           `json:"code"`
	Message string           `json:"message"`
	Details []armErrorDetail `json:"details"`
}

// parseARMError pulls the Azure error code, the first detail's code and the message out of
// an error body. A body that is not ARM's JSON shape becomes the message, truncated.
func parseARMError(body []byte) (code, detailCode, message string) {
	var parsed armErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil {
		detail := parsed.armErrorDetail
		if parsed.Error != nil {
			detail = *parsed.Error
		}
		if detail.Code != "" || detail.Message != "" {
			code, message = detail.Code, detail.Message
			if len(detail.Details) > 0 {
				detailCode = detail.Details[0].Code
				// "One or more fields contain incorrect values" says nothing; the detail
				// says which field.
				if detail.Details[0].Message != "" {
					message = strings.TrimSpace(message + " " + detail.Details[0].Message)
				}
			}
			return code, detailCode, truncateMessage(message)
		}
	}
	return "", "", truncateMessage(strings.TrimSpace(string(body)))
}

// truncateMessage cuts message to maxErrorBodyInMessage bytes, on a rune boundary.
func truncateMessage(message string) string {
	// Replace invalid bytes first: each run becomes a three-byte U+FFFD, which can make the
	// message longer than the body it came from.
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= maxErrorBodyInMessage {
		return message
	}
	return strings.ToValidUTF8(message[:maxErrorBodyInMessage], "") + "…"
}
