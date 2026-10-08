package apim

// Exhaustive cases for the ARM request helper, the typed errors it returns, the 202 answer
// of an API upsert and the read of the operation behind it, run against an httptest server
// standing in for ARM. The point is that a controller always gets back something it can
// classify: an *Error with the HTTP status, the Azure codes and the method, an operation
// to follow, or a transport error that is clearly not an ARM answer.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Request shape: method, URL, query, If-Match, Content-Type and body per call.
// ---------------------------------------------------------------------------

// hcWant is one request a call must send.
type hcWant struct {
	method string
	// path is the escaped path below hcServicePath ("" is the service itself).
	path string
	// query is the exact query; api-version is added automatically.
	query map[string]string
	// ifMatch and contentType are the exact header values; "" means absent.
	ifMatch     string
	contentType string
	// body checks the request body; nil means there must be none.
	body func(t *testing.T, body []byte)
}

func hcPropsEqual(want map[string]any) func(t *testing.T, body []byte) {
	return func(t *testing.T, body []byte) {
		t.Helper()
		if got := hcProps(t, body); !reflect.DeepEqual(got, want) {
			t.Errorf("properties = %#v, want %#v", got, want)
		}
	}
}

func TestCallRequestShapes(t *testing.T) {
	// getAbsent answers the etag GET with 404 and every write with 200.
	getAbsent := func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet {
			hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound"}}`)
			return
		}
		hcRespond(w, http.StatusOK, nil, `{}`)
	}
	getWithETag := func(etag string) func(w http.ResponseWriter, r *http.Request, _ []byte) {
		return func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.Method == http.MethodGet && etag != "" {
				w.Header().Set("ETag", etag)
			}
			hcRespond(w, http.StatusOK, nil, `{}`)
		}
	}
	ok := func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		hcRespond(w, http.StatusOK, nil, `{"properties":{}}`)
	}

	openAPI := []byte(`{"openapi":"3.0.0"}`)
	// The import goes as a JSON envelope: the document unchanged in properties.value,
	// with the path, the backend serviceUrl and the subscription requirement set in the
	// same write. There are no separate PATCH requests for them any more.
	importBodyWith := func(subscriptionRequired bool) func(t *testing.T, body []byte) {
		return func(t *testing.T, body []byte) {
			t.Helper()
			var env struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("import body is not JSON: %v (%s)", err, body)
			}
			want := map[string]any{
				"format": "openapi+json", "value": string(openAPI), "path": "/orders",
				"serviceUrl": "https://orders.internal", "subscriptionRequired": subscriptionRequired,
			}
			if !reflect.DeepEqual(env.Properties, want) {
				t.Errorf("import properties = %v, want %v", env.Properties, want)
			}
		}
	}
	importBody := importBodyWith(true)
	importWith := func(cfg APIMDeploymentConfig) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := ImportOpenAPIDefinitionToAPIM(ctx, cfg, openAPI)
			return err
		}
	}
	upsertWebSocketWith := func(cfg APIMDeploymentConfig) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := UpsertWebSocketAPI(ctx, cfg)
			return err
		}
	}
	revision2 := deploymentConfig()
	revision2.Revision = "2"
	noSubscription := deploymentConfig()
	noSubscription.SubscriptionRequired = false
	importQuery := map[string]string{}
	importRevQuery := map[string]string{"createRevision": "true"}
	wsProps := map[string]any{
		"type": "websocket", "displayName": "orders", "path": "orders",
		"protocols": []any{"wss"}, "serviceUrl": "https://orders.internal", "subscriptionRequired": true,
	}

	cases := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request, body []byte)
		call    func(ctx context.Context) error
		want    []hcWant
	}{
		{"GetAPI", ok, func(ctx context.Context) error {
			_, _, err := GetAPI(ctx, deploymentConfig())
			return err
		}, []hcWant{{method: http.MethodGet, path: "/apis/orders"}}},

		{"import new API", getAbsent, importWith(deploymentConfig()), []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: "*",
				contentType: "application/json", body: importBody},
		}},
		{"import existing API with weak etag", getWithETag(`W/"e1"`), importWith(deploymentConfig()), []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: `"e1"`,
				contentType: "application/json", body: importBody},
		}},
		{"import existing API with unquoted etag", getWithETag(`e2`), importWith(deploymentConfig()), []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: `"e2"`,
				contentType: "application/json", body: importBody},
		}},
		{"import existing API without etag", getWithETag(""), importWith(deploymentConfig()), []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: "*",
				contentType: "application/json", body: importBody},
		}},
		{"import with subscriptionRequired false sends it, not omits it", getAbsent, importWith(noSubscription), []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: "*",
				contentType: "application/json", body: importBodyWith(false)},
		}},
		{"import revision skips the GET", ok, importWith(revision2), []hcWant{
			{method: http.MethodPut, path: "/apis/orders;rev=2", query: importRevQuery, ifMatch: "*",
				contentType: "application/json", body: importBody},
		}},

		{"websocket new API", getAbsent, upsertWebSocketWith(deploymentConfig()), []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", ifMatch: "*", contentType: contentTypeJSON,
				body: hcPropsEqual(wsProps)},
		}},
		{"websocket existing API", getWithETag(`"w1"`), upsertWebSocketWith(deploymentConfig()), []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", ifMatch: `"w1"`, contentType: contentTypeJSON,
				body: hcPropsEqual(wsProps)},
		}},
		{"websocket revision", ok, upsertWebSocketWith(revision2), []hcWant{
			{method: http.MethodPut, path: "/apis/orders;rev=2", ifMatch: "*", contentType: contentTypeJSON,
				body: hcPropsEqual(wsProps)},
		}},

		{"product assignment per product", ok, func(ctx context.Context) error {
			return AssignProductsToAPI(ctx, deploymentConfig())
		}, []hcWant{
			{method: http.MethodPut, path: "/products/p1/apis/orders"},
			{method: http.MethodPut, path: "/products/p2/apis/orders"},
		}},
		{"tag assignment per tag", ok, func(ctx context.Context) error {
			cfg := deploymentConfig()
			cfg.TagIDs = []string{"t1", "t2"}
			return AssignTagsToAPI(ctx, cfg)
		}, []hcWant{
			{method: http.MethodPut, path: "/apis/orders/tags/t1"},
			{method: http.MethodPut, path: "/apis/orders/tags/t2"},
		}},

		{"service details", ok, func(ctx context.Context) error {
			_, _, err := GetAPIMServiceDetails(ctx, deploymentConfig())
			return err
		}, []hcWant{{method: http.MethodGet, path: ""}}},

		{"product upsert published", ok, func(ctx context.Context) error {
			cfg := productConfig()
			cfg.DisplayName, cfg.Description, cfg.Published = "Product one", "desc", true
			return UpsertProduct(ctx, cfg)
		}, []hcWant{{method: http.MethodPut, path: "/products/p1", ifMatch: "*", contentType: contentTypeJSON,
			body: hcPropsEqual(map[string]any{
				"displayName": "Product one", "description": "desc", "subscriptionRequired": true,
				"approvalRequired": false, "subscriptionsLimit": float64(1000), "state": "published",
			})}}},
		{"product upsert not published", ok, func(ctx context.Context) error {
			cfg := productConfig()
			cfg.DisplayName = "Product one"
			return UpsertProduct(ctx, cfg)
		}, []hcWant{{method: http.MethodPut, path: "/products/p1", ifMatch: "*", contentType: contentTypeJSON,
			body: hcPropsEqual(map[string]any{
				"displayName": "Product one", "description": "", "subscriptionRequired": true,
				"approvalRequired": false, "subscriptionsLimit": float64(1000), "state": "notPublished",
			})}}},
		{"product delete", ok, func(ctx context.Context) error {
			return DeleteProduct(ctx, productConfig())
		}, []hcWant{{method: http.MethodDelete, path: "/products/p1",
			query: map[string]string{"deleteSubscriptions": "true"}, ifMatch: "*"}}},

		{"tag upsert", ok, func(ctx context.Context) error {
			return UpsertTag(ctx, tagConfig())
		}, []hcWant{{method: http.MethodPut, path: "/tags/t1", ifMatch: "*", contentType: contentTypeJSON,
			body: hcPropsEqual(map[string]any{"displayName": "Tag one"})}}},

		{"inbound policy on the API", ok, func(ctx context.Context) error {
			return UpsertInboundPolicy(ctx, policyConfig())
		}, []hcWant{{method: http.MethodPut, path: "/apis/orders/policies/policy", ifMatch: "*",
			contentType: contentTypeJSON,
			body:        hcPropsEqual(map[string]any{"format": "xml", "value": "<policies/>"})}}},
		{"inbound policy on an operation", ok, func(ctx context.Context) error {
			cfg := policyConfig()
			cfg.OperationID = "get-order"
			return UpsertInboundPolicy(ctx, cfg)
		}, []hcWant{{method: http.MethodPut, path: "/apis/orders/operations/get-order/policies/policy", ifMatch: "*",
			contentType: contentTypeJSON,
			body:        hcPropsEqual(map[string]any{"format": "xml", "value": "<policies/>"})}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newHCFake(t, tc.handler)
			if err := tc.call(context.Background()); err != nil {
				t.Fatalf("call = %v, want nil", err)
			}
			got := fake.requests()
			if len(got) != len(tc.want) {
				t.Fatalf("fake ARM saw %d requests, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				hcCheckRequest(t, i, got[i], want)
			}
		})
	}
}

func hcCheckRequest(t *testing.T, i int, got hcRecorded, want hcWant) {
	t.Helper()
	if got.method != want.method {
		t.Errorf("request %d method = %s, want %s", i, got.method, want.method)
	}
	if wantPath := hcServicePath + want.path; got.escapedPath != wantPath {
		t.Errorf("request %d path =\n  %s\nwant\n  %s", i, got.escapedPath, wantPath)
	}
	wantQuery := map[string][]string{"api-version": {apiVersion}}
	for k, v := range want.query {
		wantQuery[k] = []string{v}
	}
	if !reflect.DeepEqual(got.query, wantQuery) {
		t.Errorf("request %d query = %v, want %v", i, got.query, wantQuery)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer tok" {
		t.Errorf("request %d Authorization = %q, want Bearer tok", i, auth)
	}
	if v := got.header.Get("If-Match"); v != want.ifMatch {
		t.Errorf("request %d If-Match = %q, want %q", i, v, want.ifMatch)
	}
	if v := got.header.Get("Content-Type"); v != want.contentType {
		t.Errorf("request %d Content-Type = %q, want %q", i, v, want.contentType)
	}
	if want.body == nil {
		if len(got.body) != 0 {
			t.Errorf("request %d body = %q, want none", i, got.body)
		}
	} else {
		want.body(t, got.body)
	}
}

// TestCallsWithNothingToDoSendNothing keeps the guards that skip a call instead of
// sending a request APIM would reject (or worse, apply to a collection).
func TestCallsWithNothingToDoSendNothing(t *testing.T) {
	cases := map[string]func(ctx context.Context) error{
		"product upsert without id": func(ctx context.Context) error {
			cfg := productConfig()
			cfg.ProductID = ""
			return UpsertProduct(ctx, cfg)
		},
		"product delete without id": func(ctx context.Context) error {
			cfg := productConfig()
			cfg.ProductID = ""
			return DeleteProduct(ctx, cfg)
		},
		"no products to assign": func(ctx context.Context) error {
			cfg := deploymentConfig()
			cfg.ProductIDs = nil
			return AssignProductsToAPI(ctx, cfg)
		},
		"no tags to assign": func(ctx context.Context) error {
			cfg := deploymentConfig()
			cfg.TagIDs = []string{}
			return AssignTagsToAPI(ctx, cfg)
		},
		"policy without api": func(ctx context.Context) error {
			cfg := policyConfig()
			cfg.APIID = ""
			return UpsertInboundPolicy(ctx, cfg)
		},
		"policy without content": func(ctx context.Context) error {
			cfg := policyConfig()
			cfg.PolicyContent = ""
			return UpsertInboundPolicy(ctx, cfg)
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				hcRespond(w, http.StatusInternalServerError, nil, `{}`)
			})
			if err := call(context.Background()); err != nil {
				t.Errorf("call = %v, want nil", err)
			}
			if n := len(fake.requests()); n != 0 {
				t.Errorf("fake ARM saw %d requests, want none", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Success: every 2xx is success for every call.
// ---------------------------------------------------------------------------

func TestEveryCallAcceptsEvery2xx(t *testing.T) {
	statuses := []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent}
	for _, call := range hcCalls() {
		for _, status := range statuses {
			t.Run(fmt.Sprintf("%s/%d", call.name, status), func(t *testing.T) {
				body := `{"properties":{}}`
				fake := newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
					// A 202 without operation headers: nothing to follow.
					hcRespond(w, status, nil, body)
				})
				err := call.run(context.Background())
				if call.async && status == http.StatusAccepted {
					// An API upsert APIM accepted without naming an operation may still be
					// running; reporting it as done would let the next write overlap it.
					var apimErr *Error
					if !errors.Is(err, ErrNoOperationURL) || !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusAccepted {
						t.Fatalf("%s() with a bare 202 = %v, want ErrNoOperationURL with status 202", call.name, err)
					}
					return
				}
				if call.name == "GetAPIMServiceDetails" && status == http.StatusNoContent {
					// A read with no body cannot be parsed into hostnames; it must say so
					// rather than return two empty hosts as if all were well.
					if err == nil {
						t.Fatal("GetAPIMServiceDetails() with an empty 204 = nil, want a parse error")
					}
					return
				}
				if err != nil {
					t.Fatalf("%s() with %d = %v, want nil", call.name, status, err)
				}
				for _, r := range fake.requests() {
					if strings.HasPrefix(r.escapedPath, "/operations") {
						t.Errorf("read %s without an operation header", r.escapedPath)
					}
				}
				if sent, seen := fake.router.sent.Load(), len(fake.requests()); sent != int64(seen) || seen == 0 {
					t.Errorf("httpClient sent %d requests and the fake saw %d: every request must go through httpClient", sent, seen)
				}
			})
		}
	}
}

// TestOnlyAPIUpsertsReport202: product, tag, policy and assignment calls take a 202 as
// done even with an operation header; only the API import and websocket upsert hand the
// operation back. No call reads the operation. Pins today's scope of the 202 handling.
func TestOnlyAPIUpsertsReport202(t *testing.T) {
	upserts := map[string]func(ctx context.Context) (WriteResult, error){
		"ImportOpenAPIDefinitionToAPIM": hcImport,
		"UpsertWebSocketAPI":            hcUpsertWebSocket,
	}
	for _, call := range hcCalls() {
		if !call.write {
			continue
		}
		t.Run(call.name, func(t *testing.T) {
			fake := newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/operations/"):
					hcRespond(w, http.StatusOK, nil, `{"status":"Succeeded"}`)
				case r.Method == http.MethodGet:
					hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound"}}`)
				default:
					hcRespond(w, http.StatusAccepted, http.Header{"Azure-Asyncoperation": {"/operations/x"}}, ``)
				}
			})
			if upsert, ok := upserts[call.name]; ok != call.async {
				t.Fatalf("%s: async = %v but listed as an API upsert = %v", call.name, call.async, ok)
			} else if ok {
				result, err := upsert(context.Background())
				if err != nil {
					t.Fatalf("%s() = %v, want nil", call.name, err)
				}
				if result.OperationURL != hcUnroutableHost+"/operations/x" {
					t.Errorf("%s() OperationURL = %q, want the Azure-AsyncOperation", call.name, result.OperationURL)
				}
			} else if err := call.run(context.Background()); err != nil {
				t.Fatalf("%s() = %v, want nil", call.name, err)
			}
			if sent, seen := fake.router.sent.Load(), len(fake.requests()); sent != int64(seen) {
				t.Errorf("httpClient sent %d requests and the fake saw %d: every request must go through httpClient", sent, seen)
			}
			if reads := fake.countPath("/operations/"); reads != 0 {
				t.Errorf("%s read the operation %d times, want 0", call.name, reads)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Errors: every non-2xx status becomes an *Error for every call.
// ---------------------------------------------------------------------------

func TestEveryCallTypesEveryErrorStatus(t *testing.T) {
	statuses := []int{
		http.StatusNotModified,
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusRequestTimeout, http.StatusConflict, http.StatusGone,
		http.StatusPreconditionFailed, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
	for _, call := range hcCalls() {
		for _, status := range statuses {
			t.Run(fmt.Sprintf("%s/%d", call.name, status), func(t *testing.T) {
				body := fmt.Sprintf(`{"error":{"code":"Code%d","message":"message %d","details":[{"code":"Detail%d"}]}}`,
					status, status, status)
				fake := newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
					if r.Method == http.MethodGet && call.method != http.MethodGet {
						// The etag lookup before an API upsert: the API is new, so the
						// failure under test is the write's. A failed lookup has its own
						// test (TestFailedExistenceCheckSendsNoUpsert).
						hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound"}}`)
						return
					}
					hcRespond(w, status, nil, body)
				})
				err := call.run(context.Background())

				if status == http.StatusNotFound && (call.name == "GetAPI" || call.name == "DeleteProduct") {
					// GetAPI: absent. DeleteProduct: already gone.
					if err != nil {
						t.Fatalf("%s() with 404 = %v, want nil", call.name, err)
					}
					return
				}

				var apimErr *Error
				if !errors.As(err, &apimErr) {
					t.Fatalf("%s() = %v (%T), want *apim.Error", call.name, err, err)
				}
				if apimErr.StatusCode != status {
					t.Errorf("StatusCode = %d, want %d", apimErr.StatusCode, status)
				}
				if status != http.StatusNotModified {
					if apimErr.Code != fmt.Sprintf("Code%d", status) || apimErr.DetailCode != fmt.Sprintf("Detail%d", status) {
						t.Errorf("codes = %q/%q, want Code%d/Detail%d", apimErr.Code, apimErr.DetailCode, status, status)
					}
					if !strings.Contains(apimErr.Message, fmt.Sprintf("message %d", status)) {
						t.Errorf("Message = %q, want the Azure message", apimErr.Message)
					}
				}
				if apimErr.Method != call.method {
					t.Errorf("Method = %q, want %q", apimErr.Method, call.method)
				}
				if apimErr.Operation == "" || apimErr.Operation == "get API" && call.name != "GetAPI" {
					t.Errorf("Operation = %q, want the call's own operation", apimErr.Operation)
				}
				if errors.Is(err, ErrImportWaitTimeout) || errors.Is(err, ErrAsyncOperationFailed) || errors.Is(err, ErrNoOperationURL) {
					t.Errorf("an HTTP failure must not match an async sentinel: %v", err)
				}
				// Only an API write that a gateway answered leaves its outcome unknown.
				gateway := status == http.StatusBadGateway || status == http.StatusGatewayTimeout
				if got := errors.Is(err, ErrWriteOutcomeUnknown); got != (call.async && gateway) {
					t.Errorf("errors.Is(err, ErrWriteOutcomeUnknown) = %v for %s with %d", got, call.name, status)
				}
				if got := IsNotFound(err); got != (status == http.StatusNotFound) {
					t.Errorf("IsNotFound = %v for %d", got, status)
				}
				if !strings.Contains(err.Error(), fmt.Sprint(status)) {
					t.Errorf("Error() = %q, want it to carry the status", err.Error())
				}

				// One failing write is one request: retrying is the controller's job, and
				// a call that kept going after a failure would hide which step broke.
				if writes := fake.countWrites(); call.write && writes != 1 {
					t.Errorf("%s sent %d write requests after a %d, want exactly 1", call.name, writes, status)
				}
			})
		}
	}
}

// countWrites counts the requests the fake saw that were not GETs.
func (f *hcFake) countWrites() int {
	writes := 0
	for _, r := range f.requests() {
		if r.method != http.MethodGet {
			writes++
		}
	}
	return writes
}

// TestErrorWrappingKeepsTheType: the controller may wrap what it gets back; the status
// and sentinels must survive that.
func TestErrorWrappingKeepsTheType(t *testing.T) {
	base := &Error{Operation: "upsert tag t1", Method: http.MethodPut, StatusCode: 409, Code: "Conflict"}
	wrapped := fmt.Errorf("reconcile: %w", fmt.Errorf("step: %w", base))
	var apimErr *Error
	if !errors.As(wrapped, &apimErr) || apimErr.StatusCode != 409 || apimErr.Code != "Conflict" {
		t.Errorf("errors.As through two wraps = %+v", apimErr)
	}
	timeout := fmt.Errorf("wrapped: %w", &Error{Operation: "import API", Err: ErrImportWaitTimeout})
	if !errors.Is(timeout, ErrImportWaitTimeout) || errors.Is(timeout, ErrAsyncOperationFailed) {
		t.Error("a wrapped wait timeout must match ErrImportWaitTimeout and only that")
	}
	failed := fmt.Errorf("wrapped: %w", &Error{Operation: "import API", Err: ErrAsyncOperationFailed})
	if !errors.Is(failed, ErrAsyncOperationFailed) || errors.Is(failed, ErrImportWaitTimeout) {
		t.Error("a wrapped async failure must match ErrAsyncOperationFailed and only that")
	}
	if (&Error{}).Unwrap() != nil {
		t.Error("an HTTP error unwraps to nil")
	}
}

// ---------------------------------------------------------------------------
// Error bodies: malformed, empty, non-JSON, nested details.
// ---------------------------------------------------------------------------

func TestErrorBodiesOverHTTP(t *testing.T) {
	longText := strings.Repeat("y", 5000)
	// 2-byte runes, so the 1024-byte cut lands inside one when unaligned.
	multiByte := "x" + strings.Repeat("é", 1000)
	cases := []struct {
		name        string
		contentType string
		body        string
		code        string
		detail      string
		message     string // exact, unless check is set
		check       func(t *testing.T, message string)
	}{
		{name: "empty body", body: ``},
		{name: "whitespace body", body: " \n\t ", message: ""},
		{name: "truncated JSON", body: `{"error":{"code":"Conf`, message: `{"error":{"code":"Conf`},
		{name: "JSON null", body: `null`, message: "null"},
		{name: "JSON array", body: `[{"code":"X"}]`, message: `[{"code":"X"}]`},
		{name: "JSON string", body: `"boom"`, message: `"boom"`},
		{name: "error is a string", body: `{"error":"boom"}`, message: `{"error":"boom"}`},
		{name: "error is empty", body: `{"error":{}}`, message: `{"error":{}}`},
		{name: "code has the wrong type", body: `{"error":{"code":123,"message":"m"}}`,
			message: `{"error":{"code":123,"message":"m"}}`},
		{name: "code only", body: `{"error":{"code":"Conflict"}}`, code: "Conflict"},
		{name: "message only", body: `{"error":{"message":"just text"}}`, message: "just text"},
		{name: "top-level shape", body: `{"code":"PreconditionFailed","message":"etag"}`,
			code: "PreconditionFailed", message: "etag"},
		{name: "wrapped wins over top level", body: `{"code":"Top","message":"top","error":{"code":"Inner","message":"inner"}}`,
			code: "Inner", message: "inner"},
		{name: "null error falls back to top level", body: `{"error":null,"code":"Top","message":"top"}`,
			code: "Top", message: "top"},
		{name: "unknown fields are ignored", body: `{"error":{"code":"C","message":"m","target":"x","innererror":{"a":1}},"extra":true}`,
			code: "C", message: "m"},
		{name: "details empty", body: `{"error":{"code":"C","message":"m","details":[]}}`, code: "C", message: "m"},
		{name: "detail with code only", body: `{"error":{"code":"ValidationError","message":"bad","details":[{"code":"InvalidFormat"}]}}`,
			code: "ValidationError", detail: "InvalidFormat", message: "bad"},
		{name: "detail message appended", body: `{"error":{"code":"ValidationError","message":"One or more fields contain incorrect values:","details":[{"code":"ValidationError","message":"Path is required."}]}}`,
			code: "ValidationError", detail: "ValidationError",
			message: "One or more fields contain incorrect values: Path is required."},
		{name: "only the first detail counts", body: `{"error":{"code":"C","message":"m","details":[{"code":"D1","message":"one"},{"code":"D2","message":"two"}]}}`,
			code: "C", detail: "D1", message: "m one"},
		{name: "first detail without code", body: `{"error":{"code":"C","message":"m","details":[{"message":"one"},{"code":"D2"}]}}`,
			code: "C", detail: "", message: "m one"},
		{name: "nested details are not flattened", body: `{"error":{"code":"C","message":"m","details":[{"code":"D1","details":[{"code":"Deep"}]}]}}`,
			code: "C", detail: "D1", message: "m"},
		{name: "message empty, detail message used", body: `{"error":{"code":"C","details":[{"code":"D","message":"from detail"}]}}`,
			code: "C", detail: "D", message: "from detail"},
		{name: "management API timeout (incident)", body: `{"error":{"code":"ManagementApiRequestFailed","message":"Management API timed out","details":[{"code":"Timeout","message":"Request timed out"}]}}`,
			code: "ManagementApiRequestFailed", detail: "Timeout", message: "Management API timed out Request timed out"},
		{name: "HTML gateway page", contentType: "text/html", body: "<html><body>502 Bad Gateway</body></html>",
			message: "<html><body>502 Bad Gateway</body></html>"},
		{name: "plain text is trimmed", contentType: "text/plain", body: "  upstream connect error \n", message: "upstream connect error"},
		{name: "long text is truncated", contentType: "text/plain", body: longText,
			message: strings.Repeat("y", maxErrorBodyInMessage) + "…"},
		{name: "long JSON message is truncated", body: `{"error":{"code":"ValidationError","message":"` + longText + `"}}`,
			code: "ValidationError", message: strings.Repeat("y", maxErrorBodyInMessage) + "…"},
		{name: "long JSON detail message is truncated",
			body: `{"error":{"code":"ValidationError","message":"m","details":[{"code":"D","message":"` + longText + `"}]}}`,
			code: "ValidationError", detail: "D", message: "m " + strings.Repeat("y", maxErrorBodyInMessage-2) + "…"},
		{name: "long multi-byte JSON message never splits a rune", body: `{"error":{"code":"C","message":"` + multiByte + `"}}`,
			code: "C",
			check: func(t *testing.T, message string) {
				if !utf8.ValidString(message) || !strings.HasSuffix(message, "…") || len(message) > maxErrorBodyInMessage+len("…") {
					t.Errorf("message %d bytes, valid UTF-8 %v; want a valid cut of at most %d bytes plus the ellipsis",
						len(message), utf8.ValidString(message), maxErrorBodyInMessage)
				}
			}},
		{name: "truncation never splits a rune", contentType: "text/plain", body: multiByte,
			check: func(t *testing.T, message string) {
				if !utf8.ValidString(message) {
					t.Errorf("message is not valid UTF-8 after truncation")
				}
				if !strings.HasSuffix(message, "…") || len(message) > maxErrorBodyInMessage+len("…") {
					t.Errorf("message length %d, want at most %d plus the ellipsis", len(message), maxErrorBodyInMessage)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				h := http.Header{}
				if tc.contentType != "" {
					h.Set("Content-Type", tc.contentType)
				}
				hcRespond(w, http.StatusInternalServerError, h, tc.body)
			})
			err := UpsertTag(context.Background(), tagConfig())
			var apimErr *Error
			if !errors.As(err, &apimErr) {
				t.Fatalf("UpsertTag() = %v (%T), want *apim.Error", err, err)
			}
			if apimErr.StatusCode != http.StatusInternalServerError {
				t.Errorf("StatusCode = %d, want 500", apimErr.StatusCode)
			}
			if apimErr.Code != tc.code || apimErr.DetailCode != tc.detail {
				t.Errorf("codes = %q/%q, want %q/%q", apimErr.Code, apimErr.DetailCode, tc.code, tc.detail)
			}
			if tc.check != nil {
				tc.check(t, apimErr.Message)
			} else if apimErr.Message != tc.message {
				t.Errorf("Message = %q, want %q", apimErr.Message, tc.message)
			}
			// Error() must never be empty or panic, whatever the body.
			if !strings.HasPrefix(err.Error(), "upsert tag t1 failed: 500 Internal Server Error") {
				t.Errorf("Error() = %q", err.Error())
			}
		})
	}
}

func TestErrorRenderingCases(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{"bare", &Error{Operation: "upsert tag t1"}, "upsert tag t1 failed"},
		{"status only", &Error{Operation: "op", StatusCode: 429}, "op failed: 429 Too Many Requests"},
		{"unknown status", &Error{Operation: "op", StatusCode: 499}, "op failed: 499 "},
		{"same code and detail shown once", &Error{Operation: "op", StatusCode: 400, Code: "ValidationError", DetailCode: "ValidationError"},
			"op failed: 400 Bad Request: ValidationError"},
		{"detail without code is not shown", &Error{Operation: "op", StatusCode: 400, DetailCode: "D"},
			"op failed: 400 Bad Request"},
		{"message without code", &Error{Operation: "op", StatusCode: 502, Message: "Bad Gateway"},
			"op failed: 502 Bad Gateway: Bad Gateway"},
		{"async failure with codes", &Error{Operation: "upsert WebSocket API", Code: "Timeout", DetailCode: "X", Err: ErrAsyncOperationFailed, Message: "operation status Failed: slow"},
			"upsert WebSocket API failed: APIM async operation failed: Timeout/X: operation status Failed: slow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 202 Accepted: the operation goes back to the caller, nothing waits for it.
// ---------------------------------------------------------------------------

// hcAsyncUpserts are the two calls that answer a 202 with a WriteResult.
var hcAsyncUpserts = []struct {
	name      string
	operation string
	run       func(ctx context.Context) (WriteResult, error)
}{
	{"import", "import API", func(ctx context.Context) (WriteResult, error) {
		return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), []byte(`{}`))
	}},
	{"websocket", "upsert WebSocket API", hcUpsertWebSocket},
}

// hcAsyncFake answers the etag GET with 404, the PUT with 202 plus accepted headers,
// and every /operations/ request with read(n), n counting from 1. accepted may refer to
// the fake's own URL through the placeholder "{host}".
func hcAsyncFake(t *testing.T, accepted http.Header, read func(n int, w http.ResponseWriter, r *http.Request)) (*hcFake, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	var host atomic.Value
	fake := newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/operations/"):
			read(int(reads.Add(1)), w, r)
		case r.Method == http.MethodGet:
			hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound"}}`)
		default:
			h := http.Header{}
			for k, values := range accepted {
				for _, v := range values {
					h.Add(k, strings.ReplaceAll(v, "{host}", host.Load().(string)))
				}
			}
			hcRespond(w, http.StatusAccepted, h, ``)
		}
	})
	host.Store(fake.server.URL)
	return fake, &reads
}

// hcSucceeded answers an operation read with Succeeded.
func hcSucceeded(_ int, w http.ResponseWriter, _ *http.Request) {
	hcRespond(w, http.StatusOK, nil, `{"status":"Succeeded"}`)
}

// TestAcceptedOperationURLSources: where the operation URL of a 202 comes from, for both
// API upserts, and that the URL handed back reads the right operation. The write itself
// never reads it.
func TestAcceptedOperationURLSources(t *testing.T) {
	cases := []struct {
		name     string
		accepted http.Header
		// wantPath is the escaped path the operation is read at; "" means the 202 is
		// refused with ErrNoOperationURL.
		wantPath  string
		wantQuery url.Values
	}{
		{"Azure-AsyncOperation relative", http.Header{"Azure-Asyncoperation": {"/operations/aao"}}, "/operations/aao", url.Values{}},
		{"Location relative", http.Header{"Location": {"/operations/loc"}}, "/operations/loc", url.Values{}},
		{"Azure-AsyncOperation absolute", http.Header{"Azure-Asyncoperation": {hcUnroutableHost + "/operations/abs"}}, "/operations/abs", url.Values{}},
		{"Location absolute", http.Header{"Location": {hcUnroutableHost + "/operations/locabs"}}, "/operations/locabs", url.Values{}},
		{"absolute in another case", http.Header{"Location": {"HTTP://ARM.INVALID/operations/upper"}}, "/operations/upper", url.Values{}},
		{"Azure-AsyncOperation wins over Location", http.Header{
			"Azure-Asyncoperation": {"/operations/preferred"},
			"Location":             {"/operations/ignored"},
		}, "/operations/preferred", url.Values{}},
		{"whitespace around the URL", http.Header{"Azure-Asyncoperation": {"   /operations/padded  "}}, "/operations/padded", url.Values{}},
		{"blank Azure-AsyncOperation falls back to Location", http.Header{
			"Azure-Asyncoperation": {"   "},
			"Location":             {"/operations/fallback"},
		}, "/operations/fallback", url.Values{}},
		{"query string is kept", http.Header{"Azure-Asyncoperation": {"/operations/q?api-version=2021-08-01&asyncResponse=true"}},
			"/operations/q", url.Values{"api-version": {"2021-08-01"}, "asyncResponse": {"true"}}},
		{"neither header", http.Header{}, "", nil},
		// The fake's own address is reachable but is not the ARM endpoint the package is
		// configured for: the operator's token must not be sent there.
		{"absolute on another host", http.Header{"Azure-Asyncoperation": {"{host}/operations/abs"}}, "", nil},
		{"Location on another host", http.Header{"Location": {"{host}/operations/locabs"}}, "", nil},
		{"credentials in the URL", http.Header{"Location": {"http://user:pw@arm.invalid/operations/x"}}, "", nil},
		{"relative without a leading slash", http.Header{"Azure-Asyncoperation": {"operations/noslash"}}, "", nil},
	}
	for _, upsert := range hcAsyncUpserts {
		for _, tc := range cases {
			t.Run(upsert.name+"/"+tc.name, func(t *testing.T) {
				fake, reads := hcAsyncFake(t, tc.accepted, hcSucceeded)
				result, err := upsert.run(context.Background())
				if got := reads.Load(); got != 0 {
					t.Errorf("the write read its operation %d times, want 0", got)
				}
				if tc.wantPath == "" {
					var apimErr *Error
					if !errors.Is(err, ErrNoOperationURL) || !errors.As(err, &apimErr) {
						t.Fatalf("%s = %+v, %v; want ErrNoOperationURL", upsert.name, result, err)
					}
					if apimErr.StatusCode != http.StatusAccepted || apimErr.Operation != upsert.operation || apimErr.Method != http.MethodPut {
						t.Errorf("err = %+v, want status 202 on the %s PUT", apimErr, upsert.operation)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s = %v, want nil", upsert.name, err)
				}
				if !result.Accepted() {
					t.Fatalf("WriteResult = %+v, want Accepted", result)
				}

				// The URL handed back reads the operation with a bare authenticated GET.
				state, err := GetOperationState(context.Background(), "tok", result.OperationURL)
				if err != nil || state.Status != OperationSucceeded {
					t.Fatalf("GetOperationState(%q) = %+v, %v; want Succeeded", result.OperationURL, state, err)
				}
				var readReq hcRecorded
				for _, r := range fake.requests() {
					if strings.HasPrefix(r.escapedPath, "/operations/") {
						readReq = r
					}
				}
				if readReq.escapedPath != tc.wantPath {
					t.Errorf("read %s, want %s", readReq.escapedPath, tc.wantPath)
				}
				if !reflect.DeepEqual(url.Values(readReq.query), tc.wantQuery) {
					t.Errorf("read query = %v, want %v", readReq.query, tc.wantQuery)
				}
				if readReq.method != http.MethodGet || len(readReq.body) != 0 {
					t.Errorf("read = %s with %d body bytes, want a bare GET", readReq.method, len(readReq.body))
				}
				if readReq.header.Get("Authorization") != "Bearer tok" {
					t.Errorf("read Authorization = %q", readReq.header.Get("Authorization"))
				}
				if readReq.header.Get("If-Match") != "" || readReq.header.Get("Content-Type") != "" {
					t.Errorf("read carries write headers: %v", readReq.header)
				}
			})
		}
	}
}

// TestOperationStateAnswers covers every answer an operation read can get: the forms of
// "still running", the forms of "done", and the forms of "ended Failed or Canceled".
func TestOperationStateAnswers(t *testing.T) {
	type answer struct {
		status int
		body   string
	}
	cases := []struct {
		name   string
		answer answer
		want   OperationStatus
		code   string
		detail string
	}{
		{"status InProgress", answer{http.StatusOK, `{"status":"InProgress"}`}, OperationRunning, "", ""},
		{"status Running", answer{http.StatusOK, `{"status":"Running"}`}, OperationRunning, "", ""},
		{"unknown status", answer{http.StatusOK, `{"status":"Updating"}`}, OperationRunning, "", ""},
		{"202 empty", answer{http.StatusAccepted, ``}, OperationRunning, "", ""},
		{"202 with status", answer{http.StatusAccepted, `{"status":"InProgress"}`}, OperationRunning, "", ""},
		{"202 claiming success", answer{http.StatusAccepted, `{"status":"Succeeded"}`}, OperationRunning, "", ""},
		{"provisioningState InProgress", answer{http.StatusOK, `{"properties":{"provisioningState":"InProgress"}}`}, OperationRunning, "", ""},

		{"Succeeded", answer{http.StatusOK, `{"status":"Succeeded"}`}, OperationSucceeded, "", ""},
		{"succeeded lowercase", answer{http.StatusOK, `{"status":"succeeded"}`}, OperationSucceeded, "", ""},
		{"Success", answer{http.StatusOK, `{"status":"Success"}`}, OperationSucceeded, "", ""},
		{"provisioningState Succeeded", answer{http.StatusOK, `{"properties":{"provisioningState":"Succeeded"}}`}, OperationSucceeded, "", ""},
		{"200 without status", answer{http.StatusOK, `{}`}, OperationSucceeded, "", ""},
		{"201 without status", answer{http.StatusCreated, `{"id":"x"}`}, OperationSucceeded, "", ""},
		{"204", answer{http.StatusNoContent, ``}, OperationSucceeded, "", ""},

		{"Failed with nested error", answer{http.StatusOK,
			`{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor","details":[{"code":"DeadOperationMonitor","message":"monitor gone"}]}}`},
			OperationFailed, "InternalServerError", "DeadOperationMonitor"},
		{"Failed with top-level code", answer{http.StatusOK, `{"status":"Failed","code":"Timeout","message":"slow"}`}, OperationFailed, "Timeout", ""},
		{"Failed without error", answer{http.StatusOK, `{"status":"Failed"}`}, OperationFailed, "", ""},
		{"failed lowercase", answer{http.StatusOK, `{"status":"failed","error":{"code":"Conflict"}}`}, OperationFailed, "Conflict", ""},
		{"Canceled", answer{http.StatusOK, `{"status":"Canceled"}`}, OperationFailed, "", ""},
		{"Cancelled", answer{http.StatusOK, `{"status":"Cancelled"}`}, OperationFailed, "", ""},
		{"provisioningState Failed", answer{http.StatusOK, `{"properties":{"provisioningState":"Failed"}}`}, OperationFailed, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				hcRespond(w, tc.answer.status, nil, tc.answer.body)
			})
			state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/seq")
			if err != nil {
				t.Fatalf("GetOperationState() error = %v", err)
			}
			if state.Status != tc.want {
				t.Fatalf("Status = %s, want %s", state.Status, tc.want)
			}
			if tc.want != OperationFailed {
				if state.Err != nil {
					t.Errorf("Err = %v, want nil for %s", state.Err, state.Status)
				}
				return
			}
			var apimErr *Error
			if !errors.As(state.Err, &apimErr) {
				t.Fatalf("Err = %v (%T), want *apim.Error", state.Err, state.Err)
			}
			if !errors.Is(state.Err, ErrAsyncOperationFailed) || errors.Is(state.Err, ErrImportWaitTimeout) {
				t.Errorf("Err = %v, want ErrAsyncOperationFailed only", state.Err)
			}
			if apimErr.StatusCode != 0 {
				t.Errorf("StatusCode = %d, want 0 for an async result", apimErr.StatusCode)
			}
			if apimErr.Code != tc.code || apimErr.DetailCode != tc.detail {
				t.Errorf("codes = %q/%q, want %q/%q", apimErr.Code, apimErr.DetailCode, tc.code, tc.detail)
			}
			if apimErr.Method != http.MethodGet {
				t.Errorf("Method = %q, want GET", apimErr.Method)
			}
			if apimErr.Operation != "APIM write of the API" {
				t.Errorf("Operation = %q, want APIM write of the API", apimErr.Operation)
			}
			if !strings.Contains(apimErr.Message, "operation status") {
				t.Errorf("Message = %q, want it to name the operation status", apimErr.Message)
			}
		})
	}
}

// TestOperationReadHTTPErrors: a read that fails is returned as the read's own *Error, so
// the caller reads again later; only a 404 says the operation is gone. A 400 or 422 whose
// Azure code is not one of APIM's busy codes is the operation's own outcome (Failed), with
// the status kept; TestOperationRead4xxClassification has the full table.
func TestOperationReadHTTPErrors(t *testing.T) {
	statuses := []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
	for _, status := range statuses {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fake := newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				hcRespond(w, status, nil, fmt.Sprintf(`{"error":{"code":"Read%d","message":"read failed"}}`, status))
			})
			state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/err")
			if n := len(fake.requests()); n != 1 {
				t.Errorf("sent %d requests, want 1: retrying is the caller's job", n)
			}
			if status == http.StatusNotFound {
				if err != nil || state.Status != OperationGone {
					t.Fatalf("GetOperationState() = %+v, %v; want Gone", state, err)
				}
				return
			}
			if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
				var failed *Error
				if err != nil || state.Status != OperationFailed || !errors.As(state.Err, &failed) {
					t.Fatalf("GetOperationState() = %+v, %v; want Failed with an *apim.Error", state, err)
				}
				if failed.StatusCode != status || failed.Code != fmt.Sprintf("Read%d", status) ||
					failed.Operation != "APIM write of the API" || !errors.Is(state.Err, ErrAsyncOperationFailed) {
					t.Errorf("Err = %+v, want status %d, code Read%d, the write's operation and ErrAsyncOperationFailed",
						failed, status, status)
				}
				return
			}
			var apimErr *Error
			if !errors.As(err, &apimErr) {
				t.Fatalf("GetOperationState() = %+v, %v (%T); want *apim.Error", state, err, err)
			}
			if apimErr.StatusCode != status || apimErr.Code != fmt.Sprintf("Read%d", status) {
				t.Errorf("status/code = %d/%q, want %d/Read%d", apimErr.StatusCode, apimErr.Code, status, status)
			}
			if apimErr.Method != http.MethodGet || apimErr.Operation != "read APIM operation" {
				t.Errorf("Method/Operation = %q/%q, want GET/read APIM operation", apimErr.Method, apimErr.Operation)
			}
			if errors.Is(err, ErrAsyncOperationFailed) || errors.Is(err, ErrImportWaitTimeout) {
				t.Errorf("a read HTTP failure must not match an async sentinel: %v", err)
			}
		})
	}
}

func TestOperationReadNonJSONBodies(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool // false: Succeeded (terminal HTTP status without a status field)
	}{
		{"200 HTML is a terminal status", http.StatusOK, `<html>ok</html>`, false},
		{"200 truncated JSON is a terminal status", http.StatusOK, `{"status":"Succ`, false},
		{"200 null", http.StatusOK, `null`, false},
		{"500 HTML is an error", http.StatusInternalServerError, `<html>boom</html>`, true},
		{"503 empty is an error", http.StatusServiceUnavailable, ``, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				hcRespond(w, tc.status, http.Header{"Content-Type": {"text/html"}}, tc.body)
			})
			state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/nj")
			if tc.wantErr {
				var apimErr *Error
				if !errors.As(err, &apimErr) || apimErr.StatusCode != tc.status {
					t.Fatalf("GetOperationState() = %+v, %v; want *apim.Error with %d", state, err, tc.status)
				}
				return
			}
			if err != nil || state.Status != OperationSucceeded {
				t.Fatalf("GetOperationState() = %+v, %v; want Succeeded", state, err)
			}
		})
	}
}

// TestAPIUpsertNeverWaits: an operation that stays running for good, behind a 202 asking
// to come back in an hour, still leaves the write at once, with a context that has no
// deadline at all.
func TestAPIUpsertNeverWaits(t *testing.T) {
	for _, upsert := range hcAsyncUpserts {
		t.Run(upsert.name, func(t *testing.T) {
			_, reads := hcAsyncFake(t, http.Header{"Location": {"/operations/slow"}, "Retry-After": {"3600"}},
				func(_ int, w http.ResponseWriter, _ *http.Request) {
					hcRespond(w, http.StatusAccepted, nil, ``)
				})
			start := time.Now()
			result, err := upsert.run(context.Background())
			if err != nil {
				t.Fatalf("%s = %v, want nil", upsert.name, err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("%s took %s; a 202 must return at once", upsert.name, elapsed)
			}
			if result.OperationURL != hcUnroutableHost+"/operations/slow" || result.RetryAfter != time.Hour {
				t.Errorf("WriteResult = %+v, want the operation and a 1h Retry-After", result)
			}
			if reads.Load() != 0 {
				t.Errorf("read the operation %d times, want 0", reads.Load())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Retry-After on the 202.
// ---------------------------------------------------------------------------

// TestAcceptedRetryAfterCases: the Retry-After on a 202 goes back to the caller as a
// duration; anything unusable comes back as zero (the caller's own default applies).
func TestAcceptedRetryAfterCases(t *testing.T) {
	future := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(http.TimeFormat) }
	const longest = time.Duration(math.MaxInt64)
	cases := []struct {
		name     string
		value    string
		min, max time.Duration
	}{
		{"absent", "", 0, 0},
		{"seconds", "30", 30 * time.Second, 30 * time.Second},
		{"an hour in seconds", "3600", time.Hour, time.Hour},
		{"HTTP date in an hour", future(time.Hour), time.Hour - 5*time.Second, time.Hour},
		{"HTTP date in a year", future(365 * 24 * time.Hour), 365*24*time.Hour - 5*time.Second, 365 * 24 * time.Hour},
		{"68 years in seconds", "2147483648", 2147483648 * time.Second, 2147483648 * time.Second},
		// seconds*time.Second would wrap negative here; it comes back as the longest delay.
		{"seconds that overflow time.Duration", "9223372037", longest, longest},
		{"seconds far beyond int64", "99999999999999999999", 0, 0},
		{"zero", "0", 0, 0},
		{"negative", "-5", 0, 0},
		{"date in the past", "Mon, 02 Jan 2006 15:04:05 GMT", 0, 0},
		{"garbage", "later", 0, 0},
		{"fraction", "1.5", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted := http.Header{"Azure-Asyncoperation": {"/operations/ra"}}
			if tc.value != "" {
				accepted.Set("Retry-After", tc.value)
			}
			_, reads := hcAsyncFake(t, accepted, hcSucceeded)
			result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
			if err != nil {
				t.Fatalf("import = %v, want nil", err)
			}
			if result.RetryAfter < tc.min || result.RetryAfter > tc.max {
				t.Errorf("RetryAfter = %s, want between %s and %s", result.RetryAfter, tc.min, tc.max)
			}
			if reads.Load() != 0 {
				t.Errorf("read the operation %d times, want 0", reads.Load())
			}
		})
	}
}

// TestRetryAfterOverflowIsBounded pins the parser directly: a number of seconds too
// large for time.Duration must not come back as a negative or tiny delay.
// time.Duration(seconds) * time.Second overflows int64 for anything above
// 9223372036 s; unchecked, that gave a negative or arbitrary short delay, and a caller
// would read the operation again with no pause.
func TestRetryAfterOverflowIsBounded(t *testing.T) {
	def := 10 * time.Second
	for _, value := range []string{"9223372037", "18446744074", "9223372036854775807"} {
		h := http.Header{"Retry-After": {value}}
		if got := retryAfter(h, def); got < def {
			t.Errorf("retryAfter(%q) = %s, want a long delay (>= %s), not an overflowed one", value, got, def)
		}
	}
}

// ---------------------------------------------------------------------------
// Transport failures: never an *Error, always naming the operation.
// ---------------------------------------------------------------------------

func TestEveryCallReportsTransportFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close() // nothing listens there any more
	t.Cleanup(UseEndpoint("http://"+addr, &http.Client{Timeout: 2 * time.Second}))

	for _, call := range hcCalls() {
		t.Run(call.name, func(t *testing.T) {
			err := call.run(context.Background())
			if err == nil {
				t.Fatalf("%s() = nil, want a transport error", call.name)
			}
			var apimErr *Error
			if errors.As(err, &apimErr) {
				t.Errorf("transport failure came back as *apim.Error: %v", err)
			}
			var urlErr *url.Error
			if !errors.As(err, &urlErr) {
				t.Errorf("err = %v (%T), want the *url.Error from net/http", err, err)
			}
			if IsNotFound(err) {
				t.Error("a transport failure is not a 404")
			}
			// The API upserts fail on their existence check, before any write went out.
			if errors.Is(err, ErrWriteOutcomeUnknown) {
				t.Errorf("%s() = %v, want no ErrWriteOutcomeUnknown: nothing was written", call.name, err)
			}
		})
	}
}

// TestTruncatedResponseBody: the server promises more body than it sends and hangs up.
func TestTruncatedResponseBody(t *testing.T) {
	newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("fake ARM cannot hijack")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		writeRaw(buf, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n{\"error\":")
		_ = conn.Close()
	})
	err := UpsertTag(context.Background(), tagConfig())
	if err == nil {
		t.Fatal("UpsertTag() = nil, want an error")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
	if !strings.Contains(err.Error(), "upsert tag t1") {
		t.Errorf("Error() = %q, want it to name the operation", err.Error())
	}
}

func writeRaw(buf *bufio.ReadWriter, s string) {
	_, _ = buf.WriteString(s)
	_ = buf.Flush()
}

func TestCancelledContextSendsNothing(t *testing.T) {
	for _, call := range hcCalls() {
		t.Run(call.name, func(t *testing.T) {
			fake := newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				hcRespond(w, http.StatusOK, nil, `{"properties":{}}`)
			})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := call.run(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s() = %v, want context.Canceled", call.name, err)
			}
			if n := len(fake.requests()); n != 0 {
				t.Errorf("fake ARM saw %d requests with a cancelled context", n)
			}
		})
	}
}

// TestSlowARMHitsTheContextDeadline: a request that outlives the reconcile context
// comes back as context.DeadlineExceeded, which the controller treats as transient.
func TestSlowARMHitsTheContextDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		hcRespond(w, http.StatusOK, nil, `{}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := UpsertTag(ctx, tagConfig())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("UpsertTag() = %v, want context.DeadlineExceeded", err)
	}
	var apimErr *Error
	if errors.As(err, &apimErr) {
		t.Errorf("a deadline came back as *apim.Error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reads: GetAPI etag handling and service details parsing.
// ---------------------------------------------------------------------------

func TestGetAPIETagForms(t *testing.T) {
	cases := []struct{ header, want string }{
		{`"abc"`, `"abc"`},
		{`W/"abc"`, `"abc"`},
		{`abc`, `"abc"`},
		{` "abc" `, `"abc"`},
		{``, ``},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				h := http.Header{}
				if tc.header != "" {
					h.Set("ETag", tc.header)
				}
				hcRespond(w, http.StatusOK, h, `{}`)
			})
			etag, exists, err := GetAPI(context.Background(), deploymentConfig())
			if err != nil || !exists || etag != tc.want {
				t.Errorf("GetAPI() = %q, %v, %v; want %q, true, nil", etag, exists, err, tc.want)
			}
		})
	}
}

func TestGetAPIMServiceDetailsParsing(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		api, portal   string
		wantParseFail bool
	}{
		{"both hosts", `{"properties":{"hostnameConfigurations":[{"type":"Proxy","hostName":"api.example"},{"type":"DeveloperPortal","hostName":"dev.example"}]}}`,
			"api.example", "dev.example", false},
		{"only proxy", `{"properties":{"hostnameConfigurations":[{"type":"Proxy","hostName":"api.example"}]}}`, "api.example", "", false},
		{"other types ignored", `{"properties":{"hostnameConfigurations":[{"type":"Management","hostName":"m"},{"type":"Scm","hostName":"s"}]}}`, "", "", false},
		{"last proxy wins", `{"properties":{"hostnameConfigurations":[{"type":"Proxy","hostName":"a"},{"type":"Proxy","hostName":"b"}]}}`, "b", "", false},
		{"no configurations", `{"properties":{}}`, "", "", false},
		{"malformed JSON", `{"properties":`, "", "", true},
		{"wrong shape", `{"properties":{"hostnameConfigurations":"x"}}`, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				hcRespond(w, http.StatusOK, nil, tc.body)
			})
			api, portal, err := GetAPIMServiceDetails(context.Background(), deploymentConfig())
			if tc.wantParseFail {
				if err == nil {
					t.Fatal("GetAPIMServiceDetails() = nil error, want a parse error")
				}
				var apimErr *Error
				if errors.As(err, &apimErr) {
					t.Errorf("a parse failure is not an ARM answer: %v", err)
				}
				return
			}
			if err != nil || api != tc.api || portal != tc.portal {
				t.Errorf("GetAPIMServiceDetails() = %q, %q, %v; want %q, %q, nil", api, portal, err, tc.api, tc.portal)
			}
		})
	}
}
