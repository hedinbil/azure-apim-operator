/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// Exhaustive table tests of the shared APIM retry helper (retry.go): how errors are
// classified, how long the waits are, what gate decides for every combination of phase,
// spec change, retry annotation and time, and how failures and successes move the
// status. Everything runs on a fake clock and a fixed random source; nothing sleeps.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
	"github.com/hedinit/azure-apim-operator/internal/apim"
)

// ---------------------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------------------

// rcWantClassForStatus is the agreed rule: 400, 401 and 403 are permanent, 404 is
// permanent on a write (anything but GET), everything else is transient.
func rcWantClassForStatus(method string, status int) apimErrorClass {
	switch {
	case status == http.StatusBadRequest, status == http.StatusUnauthorized, status == http.StatusForbidden:
		return errorClassPermanent
	case status == http.StatusNotFound && method != http.MethodGet:
		return errorClassPermanent
	}
	return errorClassTransient
}

// TestRetryCasesClassifyEveryHTTPStatus sweeps every 4xx and 5xx status for every method
// the package uses, with no Azure code and with a code nobody listed, bare and wrapped.
func TestRetryCasesClassifyEveryHTTPStatus(t *testing.T) {
	methods := []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete}
	for _, method := range methods {
		for status := 400; status <= 599; status++ {
			want := rcWantClassForStatus(method, status)
			for _, code := range []string{"", "SomeCodeNobodyListed"} {
				err := rcAPIMErr(method, status, code, "")
				if got := classifyAPIMError(err); got != want {
					t.Errorf("%s %d code %q: class = %s, want %s", method, status, code, got, want)
				}
				if got := classifyAPIMError(fmt.Errorf("step: %w", err)); got != want {
					t.Errorf("%s %d code %q wrapped: class = %s, want %s", method, status, code, got, want)
				}
			}
		}
	}
}

// TestRetryCasesClassifyRelevantStatuses names the statuses that matter, so a failure
// reads as a sentence.
func TestRetryCasesClassifyRelevantStatuses(t *testing.T) {
	cases := []struct {
		name   string
		method string
		status int
		code   string
		want   apimErrorClass
	}{
		{"400 bad request on PUT", http.MethodPut, 400, "ValidationError", errorClassPermanent},
		{"400 bad request on PATCH", http.MethodPatch, 400, "ValidationError", errorClassPermanent},
		{"400 bad request on DELETE (product with subscriptions)", http.MethodDelete, 400, "ValidationError", errorClassPermanent},
		{"400 on the GET poll", http.MethodGet, 400, "", errorClassPermanent},
		{"401 expired or wrong token", http.MethodPut, 401, "InvalidAuthenticationToken", errorClassPermanent},
		{"401 on a GET", http.MethodGet, 401, "", errorClassPermanent},
		{"402", http.MethodPut, 402, "", errorClassTransient},
		{"403 missing role", http.MethodPut, 403, "AuthorizationFailed", errorClassPermanent},
		{"403 on a GET", http.MethodGet, 403, "AuthorizationFailed", errorClassPermanent},
		{"404 on PUT (API missing for a product link)", http.MethodPut, 404, "ResourceNotFound", errorClassPermanent},
		{"404 on PATCH", http.MethodPatch, 404, "ResourceNotFound", errorClassPermanent},
		{"404 on DELETE", http.MethodDelete, 404, "ResourceNotFound", errorClassPermanent},
		{"404 on POST", http.MethodPost, 404, "", errorClassPermanent},
		{"404 on GET (poll URL not there yet)", http.MethodGet, 404, "ResourceNotFound", errorClassTransient},
		{"405", http.MethodPut, 405, "", errorClassTransient},
		{"408 request timeout", http.MethodPut, 408, "", errorClassTransient},
		{"409 conflict", http.MethodPut, 409, "Conflict", errorClassTransient},
		{"409 without a code", http.MethodPut, 409, "", errorClassTransient},
		{"410", http.MethodPut, 410, "", errorClassTransient},
		{"412 precondition failed (etag race)", http.MethodPut, 412, "PreconditionFailed", errorClassTransient},
		{"412 without a code", http.MethodPut, 412, "", errorClassTransient},
		{"413", http.MethodPut, 413, "", errorClassTransient},
		{"415", http.MethodPut, 415, "", errorClassTransient},
		{"422 management API timed out", http.MethodPut, 422, "ManagementApiRequestFailed", errorClassTransient},
		{"422 without a code", http.MethodPut, 422, "", errorClassTransient},
		{"423", http.MethodPut, 423, "", errorClassTransient},
		{"429 throttled", http.MethodPut, 429, "TooManyRequests", errorClassTransient},
		{"429 on a GET", http.MethodGet, 429, "", errorClassTransient},
		{"499", http.MethodPut, 499, "", errorClassTransient},
		{"500", http.MethodPut, 500, "InternalServerError", errorClassTransient},
		{"500 without a code", http.MethodPut, 500, "", errorClassTransient},
		{"501", http.MethodPut, 501, "", errorClassTransient},
		{"502", http.MethodPut, 502, "", errorClassTransient},
		{"503", http.MethodPut, 503, "ServiceUnavailable", errorClassTransient},
		{"504", http.MethodPut, 504, "GatewayTimeout", errorClassTransient},
		{"599", http.MethodPut, 599, "", errorClassTransient},
		{"status 0 (async result) without a code", http.MethodGet, 0, "", errorClassTransient},
		{"status 0 with a permanent-looking code", http.MethodPut, 0, "ValidationError", errorClassTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAPIMError(rcAPIMErr(tc.method, tc.status, tc.code, "")); got != tc.want {
				t.Errorf("class = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRetryCasesClassifyAzureCodes: every listed Azure code makes an error transient,
// whatever status it arrives with, as the top-level code, as the first detail code, and
// as the result of an async operation.
func TestRetryCasesClassifyAzureCodes(t *testing.T) {
	transientCodes := []string{"InternalServerError", "PreconditionFailed", "Timeout", "ManagementApiRequestFailed", "Conflict"}
	statuses := []struct {
		method string
		status int
	}{
		{http.MethodPut, 400}, {http.MethodPut, 401}, {http.MethodPut, 403}, {http.MethodPut, 404},
		{http.MethodPatch, 400}, {http.MethodPatch, 404}, {http.MethodDelete, 400}, {http.MethodDelete, 404},
		{http.MethodGet, 400}, {http.MethodGet, 404}, {http.MethodPut, 409}, {http.MethodPut, 412},
		{http.MethodPut, 422}, {http.MethodPut, 429}, {http.MethodPut, 500}, {http.MethodPut, 503},
		{http.MethodGet, 0},
	}
	for _, code := range transientCodes {
		for _, s := range statuses {
			name := fmt.Sprintf("%s/%s %d", code, s.method, s.status)
			t.Run(name+"/code", func(t *testing.T) {
				if got := classifyAPIMError(rcAPIMErr(s.method, s.status, code, "")); got != errorClassTransient {
					t.Errorf("class = %s, want transient", got)
				}
			})
			t.Run(name+"/detail code", func(t *testing.T) {
				if got := classifyAPIMError(rcAPIMErr(s.method, s.status, "BadRequest", code)); got != errorClassTransient {
					t.Errorf("class = %s, want transient", got)
				}
			})
			t.Run(name+"/both", func(t *testing.T) {
				if got := classifyAPIMError(rcAPIMErr(s.method, s.status, code, code)); got != errorClassTransient {
					t.Errorf("class = %s, want transient", got)
				}
			})
			t.Run(name+"/wrapped twice", func(t *testing.T) {
				err := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", rcAPIMErr(s.method, s.status, code, "")))
				if got := classifyAPIMError(err); got != errorClassTransient {
					t.Errorf("class = %s, want transient", got)
				}
			})
		}
		t.Run(code+"/async operation result", func(t *testing.T) {
			err := &apim.Error{Operation: "import API", Method: http.MethodGet, Code: code, Err: apim.ErrAsyncOperationFailed}
			if got := classifyAPIMError(err); got != errorClassTransient {
				t.Errorf("class = %s, want transient", got)
			}
		})
	}
}

// TestRetryCasesClassifyUnlistedCodes: a code that is not on the list does not change
// what the status says.
func TestRetryCasesClassifyUnlistedCodes(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		code       string
		detailCode string
		want       apimErrorClass
	}{
		{"ValidationError 400", 400, "ValidationError", "", errorClassPermanent},
		{"ValidationError with a field detail", 400, "ValidationError", "InvalidField", errorClassPermanent},
		{"InvalidAuthenticationToken 401", 401, "InvalidAuthenticationToken", "", errorClassPermanent},
		{"ExpiredAuthenticationToken 401", 401, "ExpiredAuthenticationToken", "", errorClassPermanent},
		{"AuthorizationFailed 403", 403, "AuthorizationFailed", "", errorClassPermanent},
		{"ResourceNotFound 404 PUT", 404, "ResourceNotFound", "", errorClassPermanent},
		{"MethodNotAllowedInPricingTier 400", 400, "MethodNotAllowedInPricingTier", "", errorClassPermanent},
		{"ValidationError on a 500 stays transient", 500, "ValidationError", "", errorClassTransient},
		{"AuthorizationFailed on a 503 stays transient", 503, "AuthorizationFailed", "", errorClassTransient},
		{"permanent code with a transient detail", 400, "ValidationError", "PreconditionFailed", errorClassTransient},
		{"transient code with a permanent detail", 400, "Conflict", "ValidationError", errorClassTransient},
		{"DeadOperationMonitor alone as the code", 400, "DeadOperationMonitor", "", errorClassPermanent},
		{"DeadOperationMonitor under InternalServerError", 400, "InternalServerError", "DeadOperationMonitor", errorClassTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAPIMError(rcAPIMErr(http.MethodPut, tc.status, tc.code, tc.detailCode)); got != tc.want {
				t.Errorf("class = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRetryCasesClassifyNetworkErrors: transport failures never reach APIM, so a retry
// can help; all are transient however deeply they are wrapped.
func TestRetryCasesClassifyNetworkErrors(t *testing.T) {
	urlErr := func(inner error) error {
		return &url.Error{Op: "Put", URL: "https://management.azure.com/subscriptions/s", Err: inner}
	}
	cases := []struct {
		name string
		err  error
	}{
		{"connection refused", urlErr(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})},
		{"connection reset", urlErr(&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET})},
		{"broken pipe", urlErr(&net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE})},
		{"DNS not found", urlErr(&net.DNSError{Err: "no such host", Name: "management.azure.com", IsNotFound: true})},
		{"DNS timeout", urlErr(&net.DNSError{Err: "i/o timeout", Name: "management.azure.com", IsTimeout: true})},
		{"TLS unknown authority", urlErr(x509.UnknownAuthorityError{})},
		{"TLS record header", urlErr(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"})},
		{"unexpected EOF", urlErr(io.ErrUnexpectedEOF)},
		{"EOF", urlErr(io.EOF)},
		{"i/o deadline", urlErr(os.ErrDeadlineExceeded)},
		{"client timeout", urlErr(context.DeadlineExceeded)},
		{"bare syscall error", syscall.ECONNRESET},
		{"wrapped by the request helper", fmt.Errorf("upsert tag t1: %w", urlErr(io.ErrUnexpectedEOF))},
		{"wrapped by helper and controller", fmt.Errorf("step 3: %w", fmt.Errorf("import API: %w", urlErr(syscall.ECONNRESET)))},
		{"read body failure", fmt.Errorf("import API: read response body: %w", io.ErrUnexpectedEOF)},
		{"http2 goaway text", errors.New("http2: server sent GOAWAY and closed the connection")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAPIMError(tc.err); got != errorClassTransient {
				t.Errorf("class = %s, want transient", got)
			}
		})
	}
}

// TestRetryCasesClassifyRealTransportFailures produces real net/http failures (a closed
// server, a client timeout) rather than hand-built ones.
func TestRetryCasesClassifyRealTransportFailures(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	err := rcGet(http.DefaultClient, closedURL)
	if err == nil {
		t.Fatal("GET on a closed server succeeded")
	}
	if got := classifyAPIMError(fmt.Errorf("upsert tag: %w", err)); got != errorClassTransient {
		t.Errorf("closed server: class = %s, want transient (%v)", got, err)
	}

	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	c := &http.Client{Timeout: 5 * time.Millisecond}
	err = rcGet(c, slow.URL)
	if err == nil {
		t.Fatal("GET with a 5ms timeout on a blocked server succeeded")
	}
	if got := classifyAPIMError(err); got != errorClassTransient {
		t.Errorf("client timeout: class = %s, want transient (%v)", got, err)
	}
}

// rcGet sends a GET through c and closes any body.
func rcGet(c *http.Client, target string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// TestRetryCasesClassifyContextErrors: an import cut short by its own deadline or by
// the manager shutting down is transient.
func TestRetryCasesClassifyContextErrors(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	causeCtx, cancel3 := context.WithTimeoutCause(context.Background(), -time.Second, errors.New("reconcile budget"))
	defer cancel3()

	cases := []struct {
		name string
		err  error
	}{
		{"DeadlineExceeded", context.DeadlineExceeded},
		{"Canceled", context.Canceled},
		{"real expired context", expired.Err()},
		{"real canceled context", canceled.Err()},
		{"context with a cause", causeCtx.Err()},
		{"wrapped once", fmt.Errorf("wait: %w", context.DeadlineExceeded)},
		{"wrapped three times", fmt.Errorf("a: %w", fmt.Errorf("b: %w", fmt.Errorf("c: %w", context.DeadlineExceeded)))},
		{"async wait cut short", fmt.Errorf("context cancelled while waiting for async import completion: %w", context.Canceled)},
		{"inside url.Error", &url.Error{Op: "Put", URL: "https://x", Err: context.DeadlineExceeded}},
		{"inside an apim.Error with a permanent status", &apim.Error{Method: http.MethodPut, StatusCode: 400, Err: context.DeadlineExceeded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAPIMError(tc.err); got != errorClassTransient {
				t.Errorf("class = %s, want transient", got)
			}
		})
	}
}

// rcUnwrapper is a caller's own error type that wraps another.
type rcUnwrapper struct{ inner error }

func (e rcUnwrapper) Error() string { return "custom: " + e.inner.Error() }
func (e rcUnwrapper) Unwrap() error { return e.inner }

// TestRetryCasesClassifyWrappedErrors: errors.Is/As see through every way a caller may
// wrap, and only %v (which drops the type) loses the classification, towards transient.
func TestRetryCasesClassifyWrappedErrors(t *testing.T) {
	waitTimeout := &apim.Error{Operation: "import API", Method: http.MethodGet, Err: apim.ErrImportWaitTimeout}
	cases := []struct {
		name string
		err  error
		want apimErrorClass
	}{
		{"nil", nil, errorClassTransient},
		{"permanent, bare", rcPermanentErr, errorClassPermanent},
		{"permanent, %w once", fmt.Errorf("step: %w", rcPermanentErr), errorClassPermanent},
		{"permanent, %w five times", fmt.Errorf("1: %w", fmt.Errorf("2: %w", fmt.Errorf("3: %w",
			fmt.Errorf("4: %w", fmt.Errorf("5: %w", rcPermanentErr))))), errorClassPermanent},
		{"permanent, custom Unwrap type", rcUnwrapper{inner: rcPermanentErr}, errorClassPermanent},
		{"permanent, errors.Join", errors.Join(errors.New("context"), rcPermanentErr), errorClassPermanent},
		{"permanent, multi %w with a plain error", fmt.Errorf("%w; %w", errors.New("x"), rcPermanentErr), errorClassPermanent},
		{"permanent, %v loses the type", fmt.Errorf("step: %v", rcPermanentErr), errorClassTransient},
		{"permanent text only", errors.New("upsert tag t1 failed: 400 Bad Request: ValidationError"), errorClassTransient},
		{"transient, bare", rcTransientErr, errorClassTransient},
		{"transient, %w", fmt.Errorf("step: %w", rcTransientErr), errorClassTransient},
		{"wait timeout, apim.Error", waitTimeout, errorClassTransient},
		{"wait timeout, wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", waitTimeout)), errorClassTransient},
		{"wait timeout, bare sentinel", apim.ErrImportWaitTimeout, errorClassTransient},
		{"wait timeout, sentinel wrapped", fmt.Errorf("import: %w", apim.ErrImportWaitTimeout), errorClassTransient},
		{"wait timeout on an apim.Error with a permanent status", &apim.Error{Method: http.MethodPut, StatusCode: 400,
			Err: apim.ErrImportWaitTimeout}, errorClassTransient},
		{"wait timeout joined with a permanent error", errors.Join(rcPermanentErr, apim.ErrImportWaitTimeout), errorClassTransient},
		{"async failed sentinel, bare", apim.ErrAsyncOperationFailed, errorClassTransient},
		{"async failed, unknown code", &apim.Error{Code: "ValidationError", Err: apim.ErrAsyncOperationFailed}, errorClassTransient},
		{"plain unknown error", errors.New("something odd"), errorClassTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAPIMError(tc.err); got != tc.want {
				t.Errorf("classifyAPIMError(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestRetryCasesClassifyRealARMAnswers runs the exported writes against an httptest ARM
// answering with real ARM error bodies, so parsing and classification are tested
// together the way a controller sees them.
func TestRetryCasesClassifyRealARMAnswers(t *testing.T) {
	type answer struct {
		status int
		body   string
	}
	var mu sync.Mutex
	var current answer
	setAnswer := func(a answer) { mu.Lock(); defer mu.Unlock(); current = a }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		a := current
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	defer server.Close()
	defer apim.UseEndpoint(server.URL, server.Client())()

	scope := struct{ sub, rg, svc string }{"sub-rc", "rg-rc", "apim-rc"}
	writes := map[string]func(ctx context.Context) error{
		"UpsertTag": func(ctx context.Context) error {
			return apim.UpsertTag(ctx, apim.APIMTagConfig{SubscriptionID: scope.sub, ResourceGroup: scope.rg,
				ServiceName: scope.svc, TagID: "t1", DisplayName: "T1", BearerToken: "tok"})
		},
		"UpsertProduct": func(ctx context.Context) error {
			return apim.UpsertProduct(ctx, apim.APIMProductConfig{SubscriptionID: scope.sub, ResourceGroup: scope.rg,
				ServiceName: scope.svc, ProductID: "p1", DisplayName: "P1", BearerToken: "tok"})
		},
		"UpsertInboundPolicy": func(ctx context.Context) error {
			return apim.UpsertInboundPolicy(ctx, apim.APIMInboundPolicyConfig{SubscriptionID: scope.sub, ResourceGroup: scope.rg,
				ServiceName: scope.svc, APIID: "orders", PolicyContent: "<policies/>", BearerToken: "tok"})
		},
		"AssignProductsToAPI": func(ctx context.Context) error {
			return apim.AssignProductsToAPI(ctx, apim.APIMDeploymentConfig{SubscriptionID: scope.sub, ResourceGroup: scope.rg,
				ServiceName: scope.svc, APIID: "orders", ProductIDs: []string{"p1"}, BearerToken: "tok"})
		},
		"AssignTagsToAPI": func(ctx context.Context) error {
			return apim.AssignTagsToAPI(ctx, apim.APIMDeploymentConfig{SubscriptionID: scope.sub, ResourceGroup: scope.rg,
				ServiceName: scope.svc, APIID: "orders", TagIDs: []string{"t1"}, BearerToken: "tok"})
		},
	}
	// dependentWrites write under a resource another custom resource creates (the API a
	// policy is set on, the product or tag an API is assigned to). A 404 there means that
	// resource is not in APIM yet, which clears up on its own: transient. A 404 on a
	// resource's own path stays permanent.
	dependentWrites := map[string]bool{"UpsertInboundPolicy": true, "AssignProductsToAPI": true, "AssignTagsToAPI": true}

	cases := []struct {
		name string
		answer
		want apimErrorClass
	}{
		{"400 ValidationError", answer{400, `{"error":{"code":"ValidationError","message":"One or more fields contain incorrect values:",` +
			`"details":[{"code":"ValidationError","target":"displayName","message":"too long"}]}}`}, errorClassPermanent},
		{"400 with a transient detail", answer{400, `{"error":{"code":"BadRequest","details":[{"code":"PreconditionFailed"}]}}`},
			errorClassTransient},
		{"400 without the error wrapper but transient code", answer{400, `{"code":"Conflict","message":"busy"}`}, errorClassTransient},
		{"400 non-JSON", answer{400, `<html>bad request</html>`}, errorClassPermanent},
		{"401", answer{401, `{"error":{"code":"InvalidAuthenticationToken","message":"The access token is invalid."}}`},
			errorClassPermanent},
		{"403", answer{403, `{"error":{"code":"AuthorizationFailed","message":"no role"}}`}, errorClassPermanent},
		{"404 on PUT", answer{404, `{"error":{"code":"ResourceNotFound","message":"API not found"}}`}, errorClassPermanent},
		{"409", answer{409, `{"error":{"code":"Conflict","message":"operation in progress"}}`}, errorClassTransient},
		{"412", answer{412, `{"error":{"code":"PreconditionFailed","message":"etag mismatch"}}`}, errorClassTransient},
		{"422 management API timed out", answer{422, `{"error":{"code":"ManagementApiRequestFailed","message":"Management API timed out"}}`},
			errorClassTransient},
		{"429", answer{429, `{"error":{"code":"TooManyRequests","message":"slow down"}}`}, errorClassTransient},
		{"500", answer{500, `{"error":{"code":"InternalServerError","message":"boom"}}`}, errorClassTransient},
		{"502 HTML", answer{502, `<html>Bad Gateway</html>`}, errorClassTransient},
		{"503 empty", answer{503, ``}, errorClassTransient},
		{"504", answer{504, `{"error":{"code":"GatewayTimeout"}}`}, errorClassTransient},
	}
	for writeName, write := range writes {
		for _, tc := range cases {
			t.Run(writeName+"/"+tc.name, func(t *testing.T) {
				setAnswer(tc.answer)
				err := write(context.Background())
				var apimErr *apim.Error
				if !errors.As(err, &apimErr) {
					t.Fatalf("%s = %v (%T), want *apim.Error", writeName, err, err)
				}
				if apimErr.StatusCode != tc.status {
					t.Errorf("StatusCode = %d, want %d", apimErr.StatusCode, tc.status)
				}
				want := tc.want
				if tc.status == http.StatusNotFound && dependentWrites[writeName] {
					want = errorClassTransient
				}
				if got := classifyAPIMError(err); got != want {
					t.Errorf("class = %s, want %s (err %v)", got, want, err)
				}
			})
		}
	}

	// Deleting a product that is already gone is a success, not an Invalid 404.
	setAnswer(answer{404, `{"error":{"code":"ResourceNotFound"}}`})
	if err := apim.DeleteProduct(context.Background(), apim.APIMProductConfig{SubscriptionID: scope.sub,
		ResourceGroup: scope.rg, ServiceName: scope.svc, ProductID: "gone", BearerToken: "tok"}); err != nil {
		t.Errorf("DeleteProduct on a 404 = %v, want nil", err)
	}
	// A product still in use cannot be deleted: permanent.
	setAnswer(answer{400, `{"error":{"code":"ValidationError","message":"Product has subscriptions"}}`})
	err := apim.DeleteProduct(context.Background(), apim.APIMProductConfig{SubscriptionID: scope.sub,
		ResourceGroup: scope.rg, ServiceName: scope.svc, ProductID: "busy", BearerToken: "tok"})
	if got := classifyAPIMError(err); got != errorClassPermanent {
		t.Errorf("DeleteProduct 400: class = %s, want permanent (err %v)", got, err)
	}
}

// TestRetryCasesClassifyRealAsyncImport runs an import that APIM accepts with 202 and
// then either never finishes (wait timeout) or fails (async result), with the wait
// shortened to milliseconds.
func TestRetryCasesClassifyRealAsyncImport(t *testing.T) {
	prevTimeout, prevInterval := apim.AsyncWaitTimeout, apim.AsyncPollInterval
	apim.AsyncWaitTimeout, apim.AsyncPollInterval = 40*time.Millisecond, 5*time.Millisecond
	defer func() { apim.AsyncWaitTimeout, apim.AsyncPollInterval = prevTimeout, prevInterval }()

	var mu sync.Mutex
	var pollBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/operations/"):
			mu.Lock()
			body := pollBody
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"ResourceNotFound"}}`))
		default:
			w.Header().Set("Azure-AsyncOperation", "/operations/op-rc")
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()
	defer apim.UseEndpoint(server.URL, server.Client())()

	cfg := apim.APIMDeploymentConfig{SubscriptionID: "sub-rc", ResourceGroup: "rg-rc", ServiceName: "apim-rc",
		APIID: "orders", RoutePrefix: "/orders", BearerToken: "tok"}

	cases := []struct {
		name     string
		pollBody string
		sentinel error
		code     string
	}{
		{"still running: wait timeout", `{"status":"InProgress"}`, apim.ErrImportWaitTimeout, ""},
		{"DeadOperationMonitor", `{"status":"Failed","error":{"code":"InternalServerError","details":[{"code":"DeadOperationMonitor"}]}}`,
			apim.ErrAsyncOperationFailed, "InternalServerError"},
		{"Timeout", `{"status":"Failed","error":{"code":"Timeout","message":"took too long"}}`, apim.ErrAsyncOperationFailed, "Timeout"},
		{"Canceled", `{"status":"Canceled"}`, apim.ErrAsyncOperationFailed, ""},
		// An async failure is never permanent: the operation result carries no status.
		{"unlisted code", `{"status":"Failed","error":{"code":"ValidationError","message":"bad doc"}}`,
			apim.ErrAsyncOperationFailed, "ValidationError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			pollBody = tc.pollBody
			mu.Unlock()
			started := time.Now()
			err := apim.ImportOpenAPIDefinitionToAPIM(context.Background(), cfg, []byte(`{"openapi":"3.0.0"}`))
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Errorf("import took %s; the shortened wait was not used", elapsed)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("import = %v, want errors.Is %v", err, tc.sentinel)
			}
			var apimErr *apim.Error
			if !errors.As(err, &apimErr) || apimErr.Code != tc.code {
				t.Errorf("import = %#v, want *apim.Error with code %q", err, tc.code)
			}
			if got := classifyAPIMError(err); got != errorClassTransient {
				t.Errorf("class = %s, want transient", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// Delays
// ---------------------------------------------------------------------------------------

// TestRetryCasesDelaySequence: min(base * 2^(n-1), max) for n = 1..10 and beyond, for
// the production shape and for other shapes.
func TestRetryCasesDelaySequence(t *testing.T) {
	cases := []struct {
		name string
		base time.Duration
		max  time.Duration
		want []time.Duration // for n = 1, 2, ...
	}{
		{"production 1m/30m", time.Minute, 30 * time.Minute, []time.Duration{
			time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
			30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
		}},
		{"milliseconds 1ms/8ms", time.Millisecond, 8 * time.Millisecond, []time.Duration{
			time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 8 * time.Millisecond,
		}},
		{"cap between powers 3s/10s", 3 * time.Second, 10 * time.Second, []time.Duration{
			3 * time.Second, 6 * time.Second, 10 * time.Second, 10 * time.Second,
		}},
		{"base equals max", 5 * time.Minute, 5 * time.Minute, []time.Duration{
			5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
		}},
		{"base above max is capped", 10 * time.Minute, time.Minute, []time.Duration{
			time.Minute, time.Minute, time.Minute,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &retryPolicy{BaseDelay: tc.base, MaxDelay: tc.max, MaxAttempts: 5}
			for i, want := range tc.want {
				n := int32(i + 1)
				if got := p.delay(n); got != want {
					t.Errorf("delay(%d) = %s, want %s", n, got, want)
				}
			}
			for _, n := range []int32{11, 31, 32, 63, 64, 1000, math.MaxInt32} {
				if got := p.delay(n); got != tc.max {
					t.Errorf("delay(%d) = %s, want the cap %s", n, got, tc.max)
				}
			}
		})
	}
}

// TestRetryCasesDelayZeroAndNegativeInputs: n <= 0 is treated as the first failure, and
// non-positive policy values fall back to production.
func TestRetryCasesDelayZeroAndNegativeInputs(t *testing.T) {
	p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5}
	for _, n := range []int32{0, -1, -5, math.MinInt32} {
		if got := p.delay(n); got != time.Minute {
			t.Errorf("delay(%d) = %s, want the base 1m", n, got)
		}
	}

	cases := []struct {
		name   string
		policy *retryPolicy
		base   time.Duration
		max    time.Duration
		maxAtt int32
		jitter float64
	}{
		{"nil", nil, time.Minute, 30 * time.Minute, 5, 0.2},
		{"zero value", &retryPolicy{}, time.Minute, 30 * time.Minute, 5, 0},
		{"zero base", &retryPolicy{MaxDelay: time.Hour, MaxAttempts: 3}, time.Minute, time.Hour, 3, 0},
		{"negative base", &retryPolicy{BaseDelay: -time.Second}, time.Minute, 30 * time.Minute, 5, 0},
		{"zero max", &retryPolicy{BaseDelay: time.Second}, time.Second, 30 * time.Minute, 5, 0},
		{"negative max", &retryPolicy{BaseDelay: time.Second, MaxDelay: -time.Minute}, time.Second, 30 * time.Minute, 5, 0},
		{"zero attempts", &retryPolicy{MaxAttempts: 0}, time.Minute, 30 * time.Minute, 5, 0},
		{"negative attempts", &retryPolicy{MaxAttempts: -3}, time.Minute, 30 * time.Minute, 5, 0},
		{"negative jitter means none", &retryPolicy{Jitter: -0.5}, time.Minute, 30 * time.Minute, 5, -0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before retryPolicy
			if tc.policy != nil {
				before = *tc.policy
			}
			eff := tc.policy.effective()
			if eff.BaseDelay != tc.base || eff.MaxDelay != tc.max || eff.MaxAttempts != tc.maxAtt || eff.Jitter != tc.jitter {
				t.Errorf("effective = base %s max %s attempts %d jitter %v; want %s %s %d %v",
					eff.BaseDelay, eff.MaxDelay, eff.MaxAttempts, eff.Jitter, tc.base, tc.max, tc.maxAtt, tc.jitter)
			}
			if eff.Now == nil || eff.Random == nil {
				t.Error("effective policy lacks a clock or a random source")
			}
			if tc.policy != nil {
				after := *tc.policy
				if after.BaseDelay != before.BaseDelay || after.MaxDelay != before.MaxDelay ||
					after.MaxAttempts != before.MaxAttempts || after.Jitter != before.Jitter ||
					(after.Now == nil) != (before.Now == nil) || (after.Random == nil) != (before.Random == nil) {
					t.Errorf("effective modified the policy: before %+v after %+v", before, after)
				}
			}
			// The first wait of a policy without positive jitter is exactly the base.
			if tc.jitter <= 0 {
				if got := tc.policy.delay(1); got != tc.base {
					t.Errorf("delay(1) = %s, want %s", got, tc.base)
				}
			}
		})
	}
}

// TestRetryCasesJitterExactPoints: the jitter maps the random source linearly onto
// -20 % .. +20 %, applied after the cap.
func TestRetryCasesJitterExactPoints(t *testing.T) {
	for n := int32(1); n <= 10; n++ {
		base := (&retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute}).delay(n)
		lo, hi := time.Duration(float64(base)*0.8), time.Duration(float64(base)*1.2)
		previous := time.Duration(-1)
		for _, r := range []float64{0, 0.001, 0.1, 0.25, 0.5, 0.75, 0.9, 0.999999} {
			p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, Jitter: 0.2, Random: func() float64 { return r }}
			got := p.delay(n)
			if got < lo || got >= hi {
				t.Errorf("n=%d r=%v: delay = %s, want in [%s, %s)", n, r, got, lo, hi)
			}
			if got <= previous {
				t.Errorf("n=%d r=%v: delay %s does not grow with r (previous %s)", n, r, got, previous)
			}
			previous = got
			switch r {
			case 0:
				if got != lo {
					t.Errorf("n=%d r=0: delay = %s, want exactly -20%% = %s", n, got, lo)
				}
			case 0.5:
				if got != base {
					t.Errorf("n=%d r=0.5: delay = %s, want exactly %s", n, got, base)
				}
			}
		}
	}
}

// TestRetryCasesJitterAlwaysWithinBounds samples a seeded random source many times per
// n and checks every wait is within +/-20 % and that the spread really is used.
func TestRetryCasesJitterAlwaysWithinBounds(t *testing.T) {
	rng := rand.New(rand.NewPCG(2026, 925))
	p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Jitter: 0.2, Random: rng.Float64}
	for n := int32(1); n <= 10; n++ {
		base := (&retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute}).delay(n)
		lo, hi := time.Duration(float64(base)*0.8), time.Duration(float64(base)*1.2)
		smallest, largest := time.Duration(math.MaxInt64), time.Duration(0)
		for range 5000 {
			got := p.delay(n)
			if got < lo || got > hi {
				t.Fatalf("n=%d: delay = %s outside [%s, %s]", n, got, lo, hi)
			}
			smallest, largest = min(smallest, got), max(largest, got)
		}
		if smallest > time.Duration(float64(base)*0.82) || largest < time.Duration(float64(base)*1.18) {
			t.Errorf("n=%d: delays spread only %s..%s around %s; jitter not applied", n, smallest, largest, base)
		}
	}
}

// TestRetryCasesNextAttemptAtHonoursJitter: through failed(), the stored time is the
// jittered delay rounded up to a whole second, and the requeue lands exactly when gate
// lets the next write through.
func TestRetryCasesNextAttemptAtHonoursJitter(t *testing.T) {
	for _, r := range []float64{0, 0.3, 0.5, 0.999999} {
		for _, nanos := range []int{0, 1, 400_000_000, 999_999_999} {
			clock := &rcClock{t: rcStart.Add(time.Duration(nanos))}
			p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: 5, Jitter: 0.2,
				Now: clock.now, Random: func() float64 { return r }}
			tag := rcTag("")
			st := &tag.Status.RetryStatus
			for n := int32(1); n <= 4; n++ {
				w := p.begin(logr.Discard(), "APIMTag", tag)
				if proceed, _ := w.gate(*st, false); !proceed {
					t.Fatalf("r=%v ns=%d n=%d: gate refused a due write", r, nanos, n)
				}
				now := clock.now()
				out := w.failed(st, rcTransientErr)
				next, err := time.Parse(time.RFC3339, st.NextAttemptAt)
				if err != nil {
					t.Fatalf("nextAttemptAt %q is not RFC3339: %v", st.NextAttemptAt, err)
				}
				jittered := p.delay(n)
				if wait := next.Sub(now); wait < jittered || wait >= jittered+time.Second {
					t.Errorf("r=%v ns=%d n=%d: nextAttemptAt is %s after now, want [%s, %s)", r, nanos, n, wait,
						jittered, jittered+time.Second)
				}
				if out.Result.RequeueAfter != next.Sub(now) {
					t.Errorf("r=%v ns=%d n=%d: RequeueAfter = %s, want %s", r, nanos, n, out.Result.RequeueAfter, next.Sub(now))
				}
				// One nanosecond early is still backing off; on the dot is due.
				clock.advance(out.Result.RequeueAfter - time.Nanosecond)
				if proceed, res := p.begin(logr.Discard(), "APIMTag", tag).gate(*st, false); proceed || res.RequeueAfter != time.Nanosecond {
					t.Errorf("r=%v ns=%d n=%d: 1ns early gate = %v %+v; want a 1ns requeue", r, nanos, n, proceed, res)
				}
				clock.advance(time.Nanosecond)
			}
		}
	}
}

// ---------------------------------------------------------------------------------------
// gate: every phase x spec change x retry annotation x time
// ---------------------------------------------------------------------------------------

// rcPhaseState is a status as failed() and succeeded() leave it.
type rcPhaseState struct {
	name     string
	failures int32
	// next is how far after rcStart nextAttemptAt lies; zero means no nextAttemptAt.
	next time.Duration
}

var rcPhaseStates = []rcPhaseState{
	{"Succeeded", 0, 0},
	{"Backoff after 1", 1, time.Minute},
	{"Backoff after 4", 4, 8 * time.Minute},
	{"Stalled", 5, 0},
	{"Stalled beyond the cap", 9, 0},
	{"Invalid at first", 1, 0},
	{"Invalid after 3", 3, 0},
}

// rcAnnotationState is the retry annotation on the object against what the status
// remembers.
type rcAnnotationState struct {
	name      string
	present   bool   // the annotation key is on the object
	value     string // its value
	last      string // status.lastRetryAnnotation
	requested bool   // what gate must make of it
}

var rcAnnotationStates = []rcAnnotationState{
	{name: "absent", requested: false},
	{name: "new", present: true, value: "v1", requested: true},
	{name: "same", present: true, value: "v1", last: "v1", requested: false},
	{name: "changed", present: true, value: "v2", last: "v1", requested: true},
	{name: "removed", last: "v1", requested: false},
	{name: "changed back to an older value", present: true, value: "v1", last: "v2", requested: true},
	// An empty value cannot be told apart from "never handled", so it never requests.
	{name: "empty value, never handled", present: true, value: "", requested: false},
	{name: "empty value after v1", present: true, value: "", last: "v1", requested: false},
}

// rcTimePoint is when the reconcile arrives, relative to nextAttemptAt (or to rcStart
// when there is none).
type rcTimePoint struct {
	name   string
	offset time.Duration
}

var rcTimePoints = []rcTimePoint{
	{"well before", -30 * time.Second},
	{"1ns before", -time.Nanosecond},
	{"exactly at", 0},
	{"well after", time.Hour},
}

func TestRetryCasesGateMatrix(t *testing.T) {
	for _, ph := range rcPhaseStates {
		for _, specChanged := range []bool{false, true} {
			for _, ann := range rcAnnotationStates {
				for _, tp := range rcTimePoints {
					name := fmt.Sprintf("%s/spec changed=%v/annotation %s/%s", ph.name, specChanged, ann.name, tp.name)
					t.Run(name, func(t *testing.T) {
						rcCheckGateCase(t, ph, specChanged, ann, tp)
					})
				}
			}
		}
	}
}

func rcCheckGateCase(t *testing.T, ph rcPhaseState, specChanged bool, ann rcAnnotationState, tp rcTimePoint) {
	t.Helper()
	reference := rcStart.Add(ph.next)
	clock := &rcClock{t: reference.Add(tp.offset)}
	p := rcPolicy(clock)

	tag := rcTag("")
	if ann.present {
		tag.Annotations = map[string]string{retryAnnotation: ann.value}
	}
	st := apimv1.RetryStatus{ConsecutiveFailures: ph.failures, LastRetryAnnotation: ann.last}
	if ph.next != 0 {
		st.NextAttemptAt = reference.Format(time.RFC3339)
	}
	original := st

	backingOff := ph.next != 0
	heldForGood := ph.failures > 0 && !backingOff
	reset := specChanged || ann.requested
	stillWaiting := backingOff && tp.offset < 0
	wantProceed := reset || (!heldForGood && !stillWaiting)

	w := p.begin(logr.Discard(), "APIMTag", tag)
	proceed, res := w.gate(st, specChanged)

	if st != original {
		t.Fatalf("gate changed the status: %+v -> %+v", original, st)
	}
	if proceed != wantProceed {
		t.Fatalf("proceed = %v, want %v", proceed, wantProceed)
	}
	if !proceed {
		var want ctrl.Result
		if stillWaiting {
			want.RequeueAfter = -tp.offset
		}
		if res != want {
			t.Errorf("result = %+v, want %+v", res, want)
		}
		return
	}
	if res != (ctrl.Result{}) {
		t.Errorf("a write that may proceed returned %+v, want an empty result", res)
	}
	if w.reset != reset {
		t.Errorf("reset = %v, want %v", w.reset, reset)
	}
	wantAttempt := ph.failures + 1
	if reset {
		wantAttempt = 1
	}
	if w.attempt != wantAttempt {
		t.Errorf("attempt = %d, want %d", w.attempt, wantAttempt)
	}

	// What prepare (called by succeeded/failed, or by the deployment before its import)
	// makes of the decision.
	wantLast := ann.last
	if ann.present && ann.value != "" {
		wantLast = ann.value
	}
	prepared := st
	w.prepare(&prepared)
	wantPrepared := st
	if reset {
		wantPrepared.ConsecutiveFailures, wantPrepared.NextAttemptAt = 0, ""
	}
	wantPrepared.LastRetryAnnotation = wantLast
	if prepared != wantPrepared {
		t.Errorf("after prepare = %+v, want %+v", prepared, wantPrepared)
	}

	// A transient failure now counts from the right place.
	failed := st
	out := w.failed(&failed, rcTransientErr)
	wantFailures := wantPrepared.ConsecutiveFailures + 1
	if failed.ConsecutiveFailures != wantFailures {
		t.Errorf("failures after a transient error = %d, want %d", failed.ConsecutiveFailures, wantFailures)
	}
	wantPhase := phaseBackoff
	if wantFailures >= 5 {
		wantPhase = phaseStalled
	}
	if out.Phase != wantPhase {
		t.Errorf("phase after a transient error = %s, want %s", out.Phase, wantPhase)
	}
	if failed.LastRetryAnnotation != wantLast {
		t.Errorf("lastRetryAnnotation after a failure = %q, want %q", failed.LastRetryAnnotation, wantLast)
	}

	// A success clears everything but the remembered annotation.
	succeeded := st
	w.succeeded(&succeeded)
	if want := (apimv1.RetryStatus{LastRetryAnnotation: wantLast}); succeeded != want {
		t.Errorf("after success = %+v, want %+v", succeeded, want)
	}

	// The handled value never retriggers: the next reconcile with the same annotation and
	// an unchanged spec is not a reset.
	again := p.begin(logr.Discard(), "APIMTag", tag)
	again.gate(failed, false)
	if again.reset {
		t.Error("the same annotation value reset the failures a second time")
	}
}

// TestRetryCasesGateNextAttemptAtFormats: nextAttemptAt is read as an instant whatever
// offset or precision it was written with, and an unreadable one is due rather than a
// reason to stop for good.
func TestRetryCasesGateNextAttemptAtFormats(t *testing.T) {
	clock := &rcClock{t: rcStart}
	p := rcPolicy(clock)
	cases := []struct {
		name     string
		failures int32
		next     string
		proceed  bool
		requeue  time.Duration
	}{
		{"UTC, 2m ahead", 2, "2026-09-25T10:02:00Z", false, 2 * time.Minute},
		{"+02:00 offset, 2m ahead", 2, "2026-09-25T12:02:00+02:00", false, 2 * time.Minute},
		{"-05:00 offset, already past", 2, "2026-09-25T04:59:00-05:00", true, 0},
		{"fractional seconds, 1.5s ahead", 2, "2026-09-25T10:00:01.5Z", false, 1500 * time.Millisecond},
		{"exactly now", 2, "2026-09-25T10:00:00Z", true, 0},
		{"far past", 4, "2000-01-01T00:00:00Z", true, 0},
		{"unreadable", 3, "tomorrow-ish", true, 0},
		{"date only", 3, "2026-09-26", true, 0},
		{"future but no failures (hand edited)", 0, "2026-09-25T10:00:30Z", false, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := apimv1.RetryStatus{ConsecutiveFailures: tc.failures, NextAttemptAt: tc.next}
			proceed, res := p.begin(logr.Discard(), "APIMTag", rcTag("")).gate(st, false)
			if proceed != tc.proceed || res.RequeueAfter != tc.requeue {
				t.Errorf("gate = %v, %s; want %v, %s", proceed, res.RequeueAfter, tc.proceed, tc.requeue)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------
// failed: Backoff -> ... -> Stalled at exactly MaxAttempts; permanent -> Invalid
// ---------------------------------------------------------------------------------------

// rcTransientErrors are the transient failures the incident and the design name.
var rcTransientErrors = []struct {
	name string
	err  error
}{
	{"wait timeout", &apim.Error{Operation: "import API", Method: http.MethodGet, Err: apim.ErrImportWaitTimeout}},
	{"409 Conflict", rcAPIMErr(http.MethodPut, 409, "Conflict", "")},
	{"412 PreconditionFailed", rcAPIMErr(http.MethodPut, 412, "PreconditionFailed", "")},
	{"422 ManagementApiRequestFailed", rcAPIMErr(http.MethodPut, 422, "ManagementApiRequestFailed", "")},
	{"429", rcAPIMErr(http.MethodPut, 429, "", "")},
	{"500", rcAPIMErr(http.MethodPut, 500, "InternalServerError", "")},
	{"503", rcAPIMErr(http.MethodPut, 503, "", "")},
	{"async DeadOperationMonitor", &apim.Error{Code: "InternalServerError", DetailCode: "DeadOperationMonitor", Err: apim.ErrAsyncOperationFailed}},
	{"network", fmt.Errorf("upsert tag: %w", &url.Error{Op: "Put", URL: "https://x", Err: syscall.ECONNRESET})},
	{"context deadline", fmt.Errorf("import: %w", context.DeadlineExceeded)},
	{"unknown", errors.New("something odd")},
}

// rcPermanentErrors are the failures APIM will repeat until something changes.
var rcPermanentErrors = []struct {
	name string
	err  error
}{
	{"400", rcAPIMErr(http.MethodPut, 400, "ValidationError", "")},
	{"400 non-JSON", rcAPIMErr(http.MethodPut, 400, "", "")},
	{"401", rcAPIMErr(http.MethodPut, 401, "InvalidAuthenticationToken", "")},
	{"403", rcAPIMErr(http.MethodPatch, 403, "AuthorizationFailed", "")},
	{"404 PUT", rcAPIMErr(http.MethodPut, 404, "ResourceNotFound", "")},
	{"404 PATCH", rcAPIMErr(http.MethodPatch, 404, "ResourceNotFound", "")},
	{"404 DELETE", rcAPIMErr(http.MethodDelete, 404, "", "")},
	{"400 wrapped by a controller step", fmt.Errorf("assign products: %w", rcAPIMErr(http.MethodPut, 400, "", ""))},
}

func TestRetryCasesTransientSequenceStallsAtExactlyMaxAttempts(t *testing.T) {
	for _, maxAttempts := range []int32{1, 2, 3, 5, 7} {
		for _, te := range rcTransientErrors {
			t.Run(fmt.Sprintf("max %d/%s", maxAttempts, te.name), func(t *testing.T) {
				clock := &rcClock{t: rcStart}
				p := &retryPolicy{BaseDelay: time.Minute, MaxDelay: 30 * time.Minute, MaxAttempts: maxAttempts, Now: clock.now}
				tag := rcTag("")
				st := &tag.Status.RetryStatus
				for n := int32(1); n <= maxAttempts; n++ {
					w := p.begin(logr.Discard(), "APIMTag", tag)
					if proceed, _ := w.gate(*st, false); !proceed {
						t.Fatalf("attempt %d: gate refused a due write", n)
					}
					if w.attempt != n {
						t.Errorf("attempt %d: gate numbered it %d", n, w.attempt)
					}
					out := w.failed(st, te.err)
					if out.Class != errorClassTransient {
						t.Fatalf("attempt %d: class = %s, want transient", n, out.Class)
					}
					if st.ConsecutiveFailures != n {
						t.Errorf("attempt %d: consecutiveFailures = %d", n, st.ConsecutiveFailures)
					}
					label := fmt.Sprintf("%d/%d", n, maxAttempts)
					if n < maxAttempts {
						wantDelay := min(time.Minute<<(n-1), 30*time.Minute)
						if out.Phase != phaseBackoff {
							t.Fatalf("attempt %d: phase = %s, want Backoff", n, out.Phase)
						}
						if out.Result.RequeueAfter != wantDelay {
							t.Errorf("attempt %d: RequeueAfter = %s, want %s", n, out.Result.RequeueAfter, wantDelay)
						}
						if want := clock.now().Add(wantDelay).Format(time.RFC3339); st.NextAttemptAt != want {
							t.Errorf("attempt %d: nextAttemptAt = %q, want %q", n, st.NextAttemptAt, want)
						}
						for _, part := range []string{"transient", "attempt " + label, st.NextAttemptAt} {
							if !strings.Contains(out.Message, part) {
								t.Errorf("attempt %d: message %q lacks %q", n, out.Message, part)
							}
						}
						clock.advance(out.Result.RequeueAfter)
						continue
					}
					if out.Phase != phaseStalled {
						t.Fatalf("attempt %d of %d: phase = %s, want Stalled", n, maxAttempts, out.Phase)
					}
					if out.Result != (ctrl.Result{}) || st.NextAttemptAt != "" {
						t.Errorf("stalled: result %+v nextAttemptAt %q; want neither", out.Result, st.NextAttemptAt)
					}
					if !strings.Contains(out.Message, fmt.Sprintf("after %d failures", n)) || !strings.Contains(out.Message, retryAnnotation) {
						t.Errorf("stalled message %q lacks the count or the annotation", out.Message)
					}
				}
				// Stalled holds for good.
				for _, wait := range []time.Duration{0, time.Minute, 24 * time.Hour, 365 * 24 * time.Hour} {
					clock.advance(wait)
					if proceed, res := p.begin(logr.Discard(), "APIMTag", tag).gate(*st, false); proceed || res != (ctrl.Result{}) {
						t.Errorf("stalled +%s: gate = %v %+v; want no write, no requeue", wait, proceed, res)
					}
				}
			})
		}
	}
}

// TestRetryCasesTransientBeyondStalled: a failure recorded on a resource that is
// already at or past the limit (only possible without a gate, e.g. a status written by
// an older operator) stays Stalled and keeps counting.
func TestRetryCasesTransientBeyondStalled(t *testing.T) {
	for _, start := range []int32{4, 5, 6, 50} {
		clock := &rcClock{t: rcStart}
		p := rcPolicy(clock)
		st := apimv1.RetryStatus{ConsecutiveFailures: start}
		out := p.begin(logr.Discard(), "APIMTag", rcTag("")).failed(&st, rcTransientErr)
		if out.Phase != phaseStalled || st.ConsecutiveFailures != start+1 || st.NextAttemptAt != "" {
			t.Errorf("from %d: phase %s, status %+v; want Stalled, %d failures, no nextAttemptAt",
				start, out.Phase, st, start+1)
		}
	}
}

// TestRetryCasesPermanentIsInvalidRegardlessOfCount: a permanent error goes straight to
// Invalid from any count, before the stall check, and never schedules a retry.
func TestRetryCasesPermanentIsInvalidRegardlessOfCount(t *testing.T) {
	for _, pe := range rcPermanentErrors {
		for _, start := range []int32{0, 1, 2, 3, 4, 5, 9} {
			for _, withNext := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/from %d/nextAttemptAt %v", pe.name, start, withNext), func(t *testing.T) {
					clock := &rcClock{t: rcStart}
					p := rcPolicy(clock)
					st := apimv1.RetryStatus{ConsecutiveFailures: start, LastRetryAnnotation: "keep"}
					if withNext {
						st.NextAttemptAt = rcStart.Add(-time.Second).Format(time.RFC3339)
					}
					out := p.begin(logr.Discard(), "APIMTag", rcTag("")).failed(&st, pe.err)
					if out.Phase != phaseInvalid || out.Class != errorClassPermanent {
						t.Fatalf("outcome = %s/%s, want Invalid/permanent", out.Phase, out.Class)
					}
					if out.Result != (ctrl.Result{}) {
						t.Errorf("Invalid requeued: %+v", out.Result)
					}
					if want := (apimv1.RetryStatus{ConsecutiveFailures: start + 1, LastRetryAnnotation: "keep"}); st != want {
						t.Errorf("status = %+v, want %+v", st, want)
					}
					if !strings.Contains(out.Message, retryAnnotation) {
						t.Errorf("message %q does not say how to retry", out.Message)
					}
					// Invalid holds whatever time passes.
					clock.advance(48 * time.Hour)
					if proceed, res := p.begin(logr.Discard(), "APIMTag", rcTag("")).gate(st, false); proceed || res != (ctrl.Result{}) {
						t.Errorf("Invalid gate = %v %+v; want no write, no requeue", proceed, res)
					}
				})
			}
		}
	}
}

// TestRetryCasesResetThenFail: after a spec change or a new retry annotation the count
// starts over, so a Stalled or Invalid resource gets a full set of attempts again.
func TestRetryCasesResetThenFail(t *testing.T) {
	type trigger struct {
		name  string
		spec  bool
		value string
	}
	triggers := []trigger{{"spec change", true, ""}, {"retry annotation", false, "r1"}, {"both", true, "r1"}}
	starts := []apimv1.RetryStatus{
		{ConsecutiveFailures: 5},
		{ConsecutiveFailures: 1},
		{ConsecutiveFailures: 3, NextAttemptAt: "2026-09-25T10:30:00Z"},
	}
	for _, tr := range triggers {
		for _, start := range starts {
			t.Run(fmt.Sprintf("%s/from %+v", tr.name, start), func(t *testing.T) {
				clock := &rcClock{t: rcStart}
				p := rcPolicy(clock)
				tag := rcTag(tr.value)
				st := start
				// Transient: Backoff at 1/5, then four more to Stalled.
				w := p.begin(logr.Discard(), "APIMTag", tag)
				if proceed, _ := w.gate(st, tr.spec); !proceed {
					t.Fatal("reset gate refused the write")
				}
				out := w.failed(&st, rcTransientErr)
				if out.Phase != phaseBackoff || st.ConsecutiveFailures != 1 || out.Result.RequeueAfter != time.Minute {
					t.Fatalf("after reset + failure: %s, %+v, %s; want Backoff, 1 failure, 1m", out.Phase, st, out.Result.RequeueAfter)
				}
				for n := int32(2); n <= 5; n++ {
					clock.advance(out.Result.RequeueAfter)
					w = p.begin(logr.Discard(), "APIMTag", tag)
					if proceed, _ := w.gate(st, false); !proceed {
						t.Fatalf("attempt %d refused", n)
					}
					out = w.failed(&st, rcTransientErr)
				}
				if out.Phase != phaseStalled || st.ConsecutiveFailures != 5 {
					t.Errorf("end: %s with %d failures, want Stalled with 5", out.Phase, st.ConsecutiveFailures)
				}
				// Permanent after a reset: Invalid with a count of 1.
				st2 := start
				w = p.begin(logr.Discard(), "APIMTag", tag)
				w.gate(st2, tr.spec)
				if out := w.failed(&st2, rcPermanentErr); out.Phase != phaseInvalid || st2.ConsecutiveFailures != 1 {
					t.Errorf("permanent after reset: %s with %d failures, want Invalid with 1", out.Phase, st2.ConsecutiveFailures)
				}
			})
		}
	}
}

// TestRetryCasesStatusMessage: the status message names the step, the error and what
// happens next.
func TestRetryCasesStatusMessage(t *testing.T) {
	clock := &rcClock{t: rcStart}
	p := rcPolicy(clock)
	st := apimv1.RetryStatus{}
	out := p.begin(logr.Discard(), "APIMTag", rcTag("")).failed(&st, rcTransientErr)
	got := out.statusMessage("upsert tag", rcTransientErr)
	want := "upsert tag: " + rcTransientErr.Error() + "; APIM write failed (transient, attempt 1/5); next attempt at 2026-09-25T10:01:00Z"
	if got != want {
		t.Errorf("statusMessage =\n  %q\nwant\n  %q", got, want)
	}
}

// ---------------------------------------------------------------------------------------
// succeeded
// ---------------------------------------------------------------------------------------

func TestRetryCasesSuccessClearsEverything(t *testing.T) {
	for _, ph := range rcPhaseStates {
		for _, ann := range rcAnnotationStates {
			for _, specChanged := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/annotation %s/spec changed=%v", ph.name, ann.name, specChanged), func(t *testing.T) {
					clock := &rcClock{t: rcStart.Add(ph.next)} // due
					p := rcPolicy(clock)
					tag := rcTag("")
					if ann.present {
						tag.Annotations = map[string]string{retryAnnotation: ann.value}
					}
					st := apimv1.RetryStatus{ConsecutiveFailures: ph.failures, LastRetryAnnotation: ann.last}
					if ph.next != 0 {
						st.NextAttemptAt = rcStart.Add(ph.next).Format(time.RFC3339)
					}
					w := p.begin(logr.Discard(), "APIMTag", tag)
					w.gate(st, specChanged)
					w.succeeded(&st)
					wantLast := ann.last
					if ann.present && ann.value != "" {
						wantLast = ann.value
					}
					if want := (apimv1.RetryStatus{LastRetryAnnotation: wantLast}); st != want {
						t.Errorf("after success = %+v, want %+v", st, want)
					}
				})
			}
		}
	}
	// Without any gate call (a controller path that always writes) it still clears.
	st := apimv1.RetryStatus{ConsecutiveFailures: 4, NextAttemptAt: "2026-09-25T10:08:00Z"}
	rcPolicy(&rcClock{t: rcStart}).begin(logr.Discard(), "APIMTag", rcTag("")).succeeded(&st)
	if st != (apimv1.RetryStatus{}) {
		t.Errorf("succeeded without gate = %+v, want empty", st)
	}
}

// TestRetryCasesSuccessAfterFailuresStartsOver: after a success, the next failure is
// attempt 1 again with the base delay.
func TestRetryCasesSuccessAfterFailuresStartsOver(t *testing.T) {
	clock := &rcClock{t: rcStart}
	p := rcPolicy(clock)
	tag := rcTag("")
	st := &tag.Status.RetryStatus
	for range 4 {
		w := p.begin(logr.Discard(), "APIMTag", tag)
		w.gate(*st, false)
		clock.advance(w.failed(st, rcTransientErr).Result.RequeueAfter)
	}
	w := p.begin(logr.Discard(), "APIMTag", tag)
	if proceed, _ := w.gate(*st, false); !proceed || w.attempt != 5 {
		t.Fatalf("fifth attempt: proceed %v attempt %d", proceed, w.attempt)
	}
	w.succeeded(st)
	w = p.begin(logr.Discard(), "APIMTag", tag)
	w.gate(*st, false)
	if w.attempt != 1 {
		t.Errorf("attempt after success = %d, want 1", w.attempt)
	}
	out := w.failed(st, rcTransientErr)
	if out.Phase != phaseBackoff || out.Result.RequeueAfter != time.Minute || st.ConsecutiveFailures != 1 {
		t.Errorf("first failure after success: %s %s %d; want Backoff 1m 1", out.Phase, out.Result.RequeueAfter, st.ConsecutiveFailures)
	}
}

// ---------------------------------------------------------------------------------------
// Log lines, per kind
// ---------------------------------------------------------------------------------------

// TestRetryCasesLogLinesPerKind: every kind logs the same lines with its own id key, and
// the stalled line is exactly what the Datadog monitor matches.
func TestRetryCasesLogLinesPerKind(t *testing.T) {
	meta := metav1.ObjectMeta{Name: "obj-rc", Namespace: "team-rc", Generation: 2}
	kinds := []struct {
		kind  string
		obj   client.Object
		idKey string
	}{
		{"APIMAPIDeployment", &apimv1.APIMAPIDeployment{ObjectMeta: meta}, "apiID"},
		{"APIMProduct", &apimv1.APIMProduct{ObjectMeta: meta}, "productID"},
		{"APIMTag", &apimv1.APIMTag{ObjectMeta: meta}, "tagID"},
		{"APIMInboundPolicy", &apimv1.APIMInboundPolicy{ObjectMeta: meta}, "apiID"},
	}
	for _, k := range kinds {
		t.Run(k.kind+"/backoff to stalled", func(t *testing.T) { rcCheckFailureLogLines(t, k.kind, k.obj, k.idKey) })
		t.Run(k.kind+"/rejected, reset, succeeded", func(t *testing.T) { rcCheckOutcomeLogLines(t, k.kind, k.obj, k.idKey) })
	}
}

// rcCheckFailureLogLines drives one resource from its first failure to Stalled with a
// reconcile half way through every wait, and checks each line.
func rcCheckFailureLogLines(t *testing.T, kind string, obj client.Object, idKey string) {
	t.Helper()
	clock := &rcClock{t: rcStart}
	p := rcPolicy(clock)
	logger, lines := rcLogs()
	st := &apimv1.RetryStatus{}
	ids := []any{idKey, "id-rc"}

	for n := 1; n <= 5; n++ {
		w := p.begin(logger, kind, obj, ids...)
		w.gate(*st, false)
		w.starting()
		out := w.failed(st, rcTransientErr)
		if n < 5 {
			// A reconcile half way through the wait is skipped with ⏸️.
			clock.advance(out.Result.RequeueAfter / 2)
			p.begin(logger, kind, obj, ids...).gate(*st, false)
			clock.advance(out.Result.RequeueAfter - out.Result.RequeueAfter/2)
		}
	}
	p.begin(logger, kind, obj, ids...).gate(*st, false) // held

	got := lines()
	wantMsgs := make([]string, 0, 15)
	for range 4 {
		wantMsgs = append(wantMsgs, msgWriteStarting, msgWriteFailed, msgWriteBackingOff)
	}
	wantMsgs = append(wantMsgs, msgWriteStarting, msgWriteStalled, msgWriteHeld)
	if len(got) != len(wantMsgs) {
		t.Fatalf("logged %d lines, want %d: %+v", len(got), len(wantMsgs), got)
	}
	for i, line := range got {
		if line.msg != wantMsgs[i] {
			t.Errorf("line %d = %q, want %q", i, line.msg, wantMsgs[i])
		}
		rcCheckIdentityKeys(t, line, kind, idKey)
	}
	for i := 0; i < 4; i++ {
		attempt := fmt.Sprintf("%d/5", i+1)
		start, fail, skip := got[3*i], got[3*i+1], got[3*i+2]
		if !strings.HasPrefix(start.msg, "▶️") || start.kv["attempt"] != attempt || start.isErr {
			t.Errorf("start line %d = %+v, want ▶️ attempt %s at info", i, start, attempt)
		}
		if !strings.HasPrefix(fail.msg, "💔") || !fail.isErr || fail.err != rcTransientErr ||
			fail.kv["class"] != errorClassTransient || fail.kv["attempt"] != attempt || fail.kv["nextAttemptAt"] == nil {
			t.Errorf("failed line %d = %+v, want 💔 error with class, attempt %s and nextAttemptAt", i, fail, attempt)
		}
		if !strings.HasPrefix(skip.msg, "⏸️") || skip.kv["nextAttemptAt"] != fail.kv["nextAttemptAt"] || skip.isErr {
			t.Errorf("skip line %d = %+v, want ⏸️ with the same nextAttemptAt at info", i, skip)
		}
	}
	stalled := got[len(got)-2]
	if stalled.msg != "🛑 APIM write stalled" || !stalled.isErr {
		t.Errorf("stalled line = %q (error %v); the Datadog monitor matches it verbatim", stalled.msg, stalled.isErr)
	}
	if stalled.kv["attempts"] != int32(5) || stalled.kv["lastError"] != rcTransientErr.Error() {
		t.Errorf("stalled keys = %+v, want attempts 5 and lastError", stalled.kv)
	}
	if held := got[len(got)-1]; !strings.HasPrefix(held.msg, "⏸️") || held.isErr {
		t.Errorf("held line = %+v, want ⏸️ at info", held)
	}
}

// rcCheckOutcomeLogLines checks the rejected, reset and success lines.
func rcCheckOutcomeLogLines(t *testing.T, kind string, obj client.Object, idKey string) {
	t.Helper()
	p := rcPolicy(&rcClock{t: rcStart})
	logger, lines := rcLogs()
	ids := []any{idKey, "id-rc"}
	st := &apimv1.RetryStatus{}
	w := p.begin(logger, kind, obj, ids...)
	w.gate(*st, false)
	w.failed(st, rcPermanentErr)
	w = p.begin(logger, kind, obj, ids...)
	w.gate(*st, true) // spec change on an Invalid resource
	w.succeeded(st)
	got := lines()
	if len(got) != 3 {
		t.Fatalf("logged %d lines, want rejected, reset, succeeded: %+v", len(got), got)
	}
	for _, line := range got {
		rcCheckIdentityKeys(t, line, kind, idKey)
	}
	if got[0].msg != "💔 APIM write rejected; not retrying" || got[0].kv["class"] != errorClassPermanent || !got[0].isErr {
		t.Errorf("rejected line = %+v", got[0])
	}
	if got[1].msg != msgWriteReset || got[1].kv["reason"] != "spec changed" || got[1].kv["previousFailures"] != int32(1) {
		t.Errorf("reset line = %+v", got[1])
	}
	if !strings.HasPrefix(got[2].msg, "💚") || got[2].kv["attempt"] != "1/5" || got[2].isErr {
		t.Errorf("success line = %+v, want 💚 attempt 1/5 at info", got[2])
	}
}

// rcCheckIdentityKeys checks a line names the resource: kind, namespace, name and its id.
func rcCheckIdentityKeys(t *testing.T, line rcLine, kind, idKey string) {
	t.Helper()
	for key, want := range map[string]any{"kind": kind, "namespace": "team-rc", "name": "obj-rc", idKey: "id-rc"} {
		if line.kv[key] != want {
			t.Errorf("line %q: %s = %v, want %v", line.msg, key, line.kv[key], want)
		}
	}
}

// TestRetryCasesQuietGate: a healthy resource's gate, and a retry annotation on a
// healthy resource, log nothing; only real resets and skips are worth a line.
func TestRetryCasesQuietGate(t *testing.T) {
	p := rcPolicy(&rcClock{t: rcStart})
	logger, lines := rcLogs()
	p.begin(logger, "APIMTag", rcTag("")).gate(apimv1.RetryStatus{}, false)
	p.begin(logger, "APIMTag", rcTag("")).gate(apimv1.RetryStatus{}, true)
	p.begin(logger, "APIMTag", rcTag("first")).gate(apimv1.RetryStatus{}, false)
	if got := lines(); len(got) != 0 {
		t.Errorf("logged %+v, want nothing", got)
	}
	// The annotation as the reason.
	p.begin(logger, "APIMTag", rcTag("again")).gate(apimv1.RetryStatus{ConsecutiveFailures: 5}, false)
	got := lines()
	if len(got) != 1 || got[0].msg != msgWriteReset || got[0].kv["reason"] != retryAnnotation+" annotation set" {
		t.Errorf("reset by annotation logged %+v", got)
	}
}
