// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file waits for the long-running operations APIM starts when it answers a write
// with 202 Accepted, typically the import of a large OpenAPI document.
package apim

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AsyncWaitTimeout is how long a write waits for APIM to finish an operation it accepted
// with 202. A 1.75 MB document has been seen to need more than the 3 minutes this used
// to be (Sep 2026); giving up early only made the next attempt overlap the first one.
// A variable so tests can shorten it.
var AsyncWaitTimeout = 5 * time.Minute

// AsyncPollInterval is how often the operation is polled when APIM does not say, through
// Retry-After, when to ask again. A variable so tests can shorten it.
var AsyncPollInterval = 10 * time.Second

// waitForAsyncImportCompletion polls Azure APIM long-running operation URLs until completion.
// APIM may return either Azure-AsyncOperation or Location headers on 202 responses. It
// returns nil on success, an *Error wrapping ErrAsyncOperationFailed when the operation
// ends Failed or Canceled, and an *Error wrapping ErrImportWaitTimeout when it is still
// running after AsyncWaitTimeout. A Retry-After header on the 202 or on a poll answer
// sets the next delay, never beyond the remaining wait.
func waitForAsyncImportCompletion(ctx context.Context, bearerToken, apiID, operation string, initial *armResponse) error {
	pollURL := strings.TrimSpace(initial.header.Get("Azure-AsyncOperation"))
	if pollURL == "" {
		pollURL = strings.TrimSpace(initial.header.Get("Location"))
	}
	if pollURL == "" {
		logger.Info("ℹ️ Import returned 202 without polling URL headers; cannot verify completion", "apiID", apiID)
		return nil
	}

	if strings.HasPrefix(pollURL, "/") {
		pollURL = armHost + pollURL
	}

	logger.Info("⏳ Polling APIM async import status", "apiID", apiID, "pollURL", pollURL,
		"timeout", AsyncWaitTimeout.String())

	deadline := time.Now().Add(AsyncWaitTimeout)
	delay := retryAfter(initial.header, AsyncPollInterval)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &Error{
				Operation: operation,
				Method:    http.MethodGet,
				Message:   fmt.Sprintf("operation still running after %s", AsyncWaitTimeout),
				Err:       ErrImportWaitTimeout,
			}
		}
		if delay <= 0 {
			delay = AsyncPollInterval
		}
		if delay > remaining {
			delay = remaining
		}
		if err := sleep(ctx, delay); err != nil {
			return fmt.Errorf("context cancelled while waiting for async import completion: %w", err)
		}

		resp, err := armRequest{
			operation: operation + " (poll)",
			method:    http.MethodGet,
			url:       pollURL,
			token:     bearerToken,
		}.send(ctx)
		if err != nil {
			return err
		}
		delay = retryAfter(resp.header, AsyncPollInterval)

		status := extractAsyncStatus(resp.body)
		switch strings.ToLower(status) {
		case "succeeded", "success":
			logger.Info("✅ APIM async import completed", "apiID", apiID, "pollURL", pollURL)
			return nil
		case "failed", "canceled", "cancelled":
			code, detailCode, message := parseARMError(resp.body)
			return &Error{
				Operation:  operation,
				Method:     http.MethodGet,
				Code:       code,
				DetailCode: detailCode,
				Message:    strings.TrimSpace("operation status " + status + ": " + message),
				Err:        ErrAsyncOperationFailed,
			}
		case "inprogress", "running", "":
			// If there's no status field and status code is terminal success, consider done.
			if status == "" && resp.statusCode != http.StatusAccepted {
				logger.Info("✅ APIM async import completed (terminal HTTP status)", "apiID", apiID, "httpStatus", resp.status)
				return nil
			}
			logger.Info("⏳ APIM async import still in progress", "apiID", apiID, "httpStatus", resp.status,
				"operationStatus", status, "nextPollIn", delay.String())
		default:
			logger.Info("ℹ️ APIM async import returned unknown status", "apiID", apiID, "operationStatus", status, "httpStatus", resp.status)
		}
	}
}

// sleep waits for d or until ctx is done, whichever is first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maxRetryAfterSeconds is the largest delay-seconds value time.Duration can hold.
// Anything above it would wrap around to a negative or short delay.
const maxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)

// retryAfter reads a Retry-After header, either delay-seconds or an HTTP date, and
// falls back to def when it is absent, unparseable or not in the future. A number of
// seconds too large for time.Duration comes back as the longest Duration; the caller
// bounds it to the remaining wait.
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
