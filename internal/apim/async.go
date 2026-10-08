// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file handles the long-running operations APIM starts when it answers a write with
// 202 Accepted, typically the import of a large OpenAPI document.
//
// The package never waits for such an operation. A write that APIM accepts with 202 returns
// at once with the URL of the operation (WriteResult.OperationURL); the caller records it and
// reads it on later reconciles (GetOperationState), so the operation is known from the moment
// APIM accepts it. Waiting inside a reconcile instead lost the operation whenever the wait was
// cut short (a timeout, a failed poll, the operator stopping), and the next attempt then sent
// a second import on top of the one still running, the pattern of the Sep 2026 incident.
package apim

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNoOperationURL marks a 202 that named no operation to follow, or one outside Azure
// Resource Manager. The write may be running in APIM; nothing tells when it ends.
var ErrNoOperationURL = errors.New("APIM accepted the write but named no Azure Resource Manager operation to follow")

// ErrWriteOutcomeUnknown marks a write of an API that got no answer saying how it ended: the
// connection failed or timed out after the request may have reached APIM, or a gateway in
// front of ARM answered 502 or 504. APIM may be running it. Test with errors.Is.
var ErrWriteOutcomeUnknown = errors.New("the outcome of the write is unknown; APIM may still be running it")

// writeOutcomeUnknown reports whether err, from sending a write, leaves its outcome unknown.
func writeOutcomeUnknown(err error) bool {
	var apimErr *Error
	if !errors.As(err, &apimErr) {
		return true // no HTTP answer at all
	}
	return apimErr.StatusCode == http.StatusBadGateway || apimErr.StatusCode == http.StatusGatewayTimeout
}

// WriteResult says how APIM took a write of an API (an import or a websocket API upsert).
type WriteResult struct {
	// OperationURL is set when APIM accepted the write (202) and runs it in the background:
	// where to read its outcome (see GetOperationState). Empty when APIM finished the write
	// before answering.
	OperationURL string
	// RetryAfter is APIM's Retry-After on the 202: how soon it suggests asking. Zero when
	// absent.
	RetryAfter time.Duration
}

// Accepted reports whether APIM is still running the write in the background.
func (r WriteResult) Accepted() bool { return r.OperationURL != "" }

// acceptedWrite builds the WriteResult of a 202, or an *Error wrapping ErrNoOperationURL
// when the answer names no operation this package may follow.
func acceptedWrite(operation, method string, header http.Header) (WriteResult, error) {
	opURL := strings.TrimSpace(header.Get("Azure-AsyncOperation"))
	if opURL == "" {
		opURL = strings.TrimSpace(header.Get("Location"))
	}
	if strings.HasPrefix(opURL, "/") {
		opURL = armHost + opURL
	}
	if opURL == "" || !IsOperationURL(opURL) {
		return WriteResult{}, &Error{
			Operation:  operation,
			Method:     method,
			StatusCode: http.StatusAccepted,
			Message:    fmt.Sprintf("operation URL %q", opURL),
			Err:        ErrNoOperationURL,
		}
	}
	return WriteResult{OperationURL: opURL, RetryAfter: retryAfter(header, 0)}, nil
}

// IsOperationURL reports whether u is on the Azure Resource Manager endpoint this package
// talks to. It is the only place an operation URL may point: reading it sends the operator's
// ARM token there, and a URL read back from a resource's status is not proof that APIM
// issued it. Scheme and host are compared case-insensitively and a default port is ignored,
// so the same endpoint written two ways is still the same endpoint.
func IsOperationURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil || parsed.User != nil {
		return false
	}
	return origin(parsed) == origin(mustParse(armHost))
}

// origin is scheme://host[:port] in lower case, without the scheme's default port.
func origin(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

func mustParse(u string) *url.URL {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil
	}
	return parsed
}

// maxRetryAfterSeconds is the largest delay-seconds value time.Duration can hold.
// Anything above it would wrap around to a negative or short delay.
const maxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)

// retryAfter reads a Retry-After header, either delay-seconds or an HTTP date, and
// falls back to def when it is absent, unparseable or not in the future. A number of
// seconds too large for time.Duration comes back as the longest Duration; the caller
// bounds it.
func retryAfter(header http.Header, def time.Duration) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return def
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return def
		}
		if seconds > maxRetryAfterSeconds {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return def
}

// extractAsyncStatus reads the state of an operation from its body: the status field of an
// Azure-AsyncOperation result, or properties.provisioningState of a resource read through a
// Location URL. Empty when neither is present.
func extractAsyncStatus(body []byte) string {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	if status, ok := payload["status"].(string); ok {
		return status
	}
	if props, ok := payload["properties"].(map[string]interface{}); ok {
		if status, ok := props["provisioningState"].(string); ok {
			return status
		}
	}
	return ""
}
