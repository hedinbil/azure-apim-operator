package apim

// Exhaustive cases for the ARM request helper, the typed errors it returns and the
// async (202) wait, run against an httptest server standing in for ARM. The point is
// that a controller always gets back something it can classify: an *Error with the
// HTTP status, the Azure codes and the method, a wait timeout it can tell apart, or a
// transport error that is clearly not an ARM answer.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// with the path and the backend serviceUrl set in the same write.
	importBody := func(t *testing.T, body []byte) {
		t.Helper()
		var env struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("import body is not JSON: %v (%s)", err, body)
		}
		want := map[string]any{
			"format": "openapi+json", "value": string(openAPI), "path": "/orders",
			"serviceUrl": "https://orders.internal", "subscriptionRequired": true,
		}
		if !reflect.DeepEqual(env.Properties, want) {
			t.Errorf("import properties = %v, want %v", env.Properties, want)
		}
	}
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

		{"import new API", getAbsent, func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), openAPI)
		}, []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: "*",
				contentType: "application/json", body: importBody},
		}},
		{"import existing API with weak etag", getWithETag(`W/"e1"`), func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), openAPI)
		}, []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: `"e1"`,
				contentType: "application/json", body: importBody},
		}},
		{"import existing API with unquoted etag", getWithETag(`e2`), func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), openAPI)
		}, []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: `"e2"`,
				contentType: "application/json", body: importBody},
		}},
		{"import existing API without etag", getWithETag(""), func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), openAPI)
		}, []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: "*",
				contentType: "application/json", body: importBody},
		}},
		{"import when the etag GET fails", func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.Method == http.MethodGet {
				hcRespond(w, http.StatusInternalServerError, nil, `{"error":{"code":"InternalServerError"}}`)
				return
			}
			hcRespond(w, http.StatusCreated, nil, `{}`)
		}, func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), openAPI)
		}, []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", query: importQuery, ifMatch: "*",
				contentType: "application/json", body: importBody},
		}},
		{"import revision skips the GET", ok, func(ctx context.Context) error {
			cfg := deploymentConfig()
			cfg.Revision = "2"
			return ImportOpenAPIDefinitionToAPIM(ctx, cfg, openAPI)
		}, []hcWant{
			{method: http.MethodPut, path: "/apis/orders;rev=2", query: importRevQuery, ifMatch: "*",
				contentType: "application/json", body: importBody},
		}},

		{"websocket new API", getAbsent, func(ctx context.Context) error {
			return UpsertWebSocketAPI(ctx, deploymentConfig())
		}, []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", ifMatch: "*", contentType: contentTypeJSON,
				body: hcPropsEqual(wsProps)},
		}},
		{"websocket existing API", getWithETag(`"w1"`), func(ctx context.Context) error {
			return UpsertWebSocketAPI(ctx, deploymentConfig())
		}, []hcWant{
			{method: http.MethodGet, path: "/apis/orders"},
			{method: http.MethodPut, path: "/apis/orders", ifMatch: `"w1"`, contentType: contentTypeJSON,
				body: hcPropsEqual(wsProps)},
		}},
		{"websocket revision", ok, func(ctx context.Context) error {
			cfg := deploymentConfig()
			cfg.Revision = "2"
			return UpsertWebSocketAPI(ctx, cfg)
		}, []hcWant{
			{method: http.MethodPut, path: "/apis/orders;rev=2", ifMatch: "*", contentType: contentTypeJSON,
				body: hcPropsEqual(wsProps)},
		}},

		{"serviceUrl patch", ok, func(ctx context.Context) error {
			return AssignServiceUrlToApi(ctx, deploymentConfig())
		}, []hcWant{{method: http.MethodPatch, path: "/apis/orders", contentType: contentTypeJSON,
			body: hcPropsEqual(map[string]any{"serviceUrl": "https://orders.internal"})}}},
		{"subscriptionRequired patch true", ok, func(ctx context.Context) error {
			return SetSubscriptionRequired(ctx, deploymentConfig())
		}, []hcWant{{method: http.MethodPatch, path: "/apis/orders", contentType: contentTypeJSON,
			body: hcPropsEqual(map[string]any{"subscriptionRequired": true})}}},
		{"subscriptionRequired patch false is sent, not omitted", ok, func(ctx context.Context) error {
			cfg := deploymentConfig()
			cfg.SubscriptionRequired = false
			return SetSubscriptionRequired(ctx, cfg)
		}, []hcWant{{method: http.MethodPatch, path: "/apis/orders", contentType: contentTypeJSON,
			body: hcPropsEqual(map[string]any{"subscriptionRequired": false})}}},

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
					// A 202 without operation headers: nothing to poll, so nothing waits.
					hcRespond(w, status, nil, body)
				})
				err := call.run(context.Background())
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
						t.Errorf("polled %s without an operation header", r.escapedPath)
					}
				}
				if sent, seen := fake.router.sent.Load(), len(fake.requests()); sent != int64(seen) || seen == 0 {
					t.Errorf("httpClient sent %d requests and the fake saw %d: every request must go through httpClient", sent, seen)
				}
			})
		}
	}
}

// TestOnlyAPIUpsertsWaitFor202: product, tag, policy, patch and assignment calls take a
// 202 as done even with an operation header; only the API import and websocket upsert
// poll. Pins today's scope of the wait.
func TestOnlyAPIUpsertsWaitFor202(t *testing.T) {
	withAsyncTiming(t, time.Second, time.Millisecond)
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
			if err := call.run(context.Background()); err != nil {
				t.Fatalf("%s() = %v, want nil", call.name, err)
			}
			if sent, seen := fake.router.sent.Load(), len(fake.requests()); sent != int64(seen) {
				t.Errorf("httpClient sent %d requests and the fake saw %d: polls included, every request must go through httpClient", sent, seen)
			}
			polls := fake.countPath("/operations/")
			if call.polls && polls != 1 {
				t.Errorf("%s polled %d times, want 1", call.name, polls)
			}
			if !call.polls && polls != 0 {
				t.Errorf("%s polled %d times, want 0", call.name, polls)
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
				fake := newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
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
				if apimErr.Operation == "" || strings.Contains(apimErr.Operation, "(poll)") {
					t.Errorf("Operation = %q, want the call's own operation", apimErr.Operation)
				}
				if errors.Is(err, ErrImportWaitTimeout) || errors.Is(err, ErrAsyncOperationFailed) {
					t.Errorf("an HTTP failure must not match an async sentinel: %v", err)
				}
				if got := IsNotFound(err); got != (status == http.StatusNotFound) {
					t.Errorf("IsNotFound = %v for %d", got, status)
				}
				if !strings.Contains(err.Error(), fmt.Sprint(status)) {
					t.Errorf("Error() = %q, want it to carry the status", err.Error())
				}

				// One failing write is one request: retrying is the controller's job, and
				// a call that kept going after a failure would hide which step broke.
				writes := 0
				for _, r := range fake.requests() {
					if r.method != http.MethodGet {
						writes++
					}
				}
				if call.write && writes != 1 {
					t.Errorf("%s sent %d write requests after a %d, want exactly 1", call.name, writes, status)
				}
			})
		}
	}
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
// Async (202) wait.
// ---------------------------------------------------------------------------

// hcAsyncUpserts are the two calls that wait for a 202.
var hcAsyncUpserts = []struct {
	name      string
	operation string
	run       func(ctx context.Context) error
}{
	{"import", "import API", func(ctx context.Context) error {
		return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), []byte(`{}`))
	}},
	{"websocket", "upsert WebSocket API", func(ctx context.Context) error {
		return UpsertWebSocketAPI(ctx, deploymentConfig())
	}},
}

// hcAsyncFake answers the etag GET with 404, the PUT with 202 plus accepted headers,
// and every /operations/ request with poll(n), n counting from 1. accepted may refer to
// the fake's own URL through the placeholder "{host}".
func hcAsyncFake(t *testing.T, accepted http.Header, poll func(n int, w http.ResponseWriter, r *http.Request)) (*hcFake, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	var host atomic.Value
	fake := newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/operations/"):
			poll(int(polls.Add(1)), w, r)
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
	return fake, &polls
}

func TestAsyncPollURLSources(t *testing.T) {
	cases := []struct {
		name     string
		accepted http.Header
		// wantPath is the escaped path that must be polled; "" means no poll at all.
		wantPath  string
		wantQuery url.Values
	}{
		{"Azure-AsyncOperation relative", http.Header{"Azure-Asyncoperation": {"/operations/aao"}}, "/operations/aao", url.Values{}},
		{"Location relative", http.Header{"Location": {"/operations/loc"}}, "/operations/loc", url.Values{}},
		{"Azure-AsyncOperation absolute", http.Header{"Azure-Asyncoperation": {"{host}/operations/abs"}}, "/operations/abs", url.Values{}},
		{"Location absolute", http.Header{"Location": {"{host}/operations/locabs"}}, "/operations/locabs", url.Values{}},
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
	}
	for _, upsert := range hcAsyncUpserts {
		for _, tc := range cases {
			t.Run(upsert.name+"/"+tc.name, func(t *testing.T) {
				withAsyncTiming(t, 2*time.Second, time.Millisecond)
				fake, polls := hcAsyncFake(t, tc.accepted, func(_ int, w http.ResponseWriter, _ *http.Request) {
					hcRespond(w, http.StatusOK, nil, `{"status":"Succeeded"}`)
				})
				if err := upsert.run(context.Background()); err != nil {
					t.Fatalf("%s = %v, want nil", upsert.name, err)
				}
				if tc.wantPath == "" {
					if polls.Load() != 0 {
						t.Errorf("polled %d times without an operation header", polls.Load())
					}
					return
				}
				if polls.Load() != 1 {
					t.Fatalf("polled %d times, want 1", polls.Load())
				}
				var pollReq hcRecorded
				for _, r := range fake.requests() {
					if strings.HasPrefix(r.escapedPath, "/operations/") {
						pollReq = r
					}
				}
				if pollReq.escapedPath != tc.wantPath {
					t.Errorf("polled %s, want %s", pollReq.escapedPath, tc.wantPath)
				}
				if !reflect.DeepEqual(url.Values(pollReq.query), tc.wantQuery) {
					t.Errorf("poll query = %v, want %v", pollReq.query, tc.wantQuery)
				}
				// The poll is an authenticated GET with nothing else on it.
				if pollReq.method != http.MethodGet || len(pollReq.body) != 0 {
					t.Errorf("poll = %s with %d body bytes, want a bare GET", pollReq.method, len(pollReq.body))
				}
				if pollReq.header.Get("Authorization") != "Bearer tok" {
					t.Errorf("poll Authorization = %q", pollReq.header.Get("Authorization"))
				}
				if pollReq.header.Get("If-Match") != "" || pollReq.header.Get("Content-Type") != "" {
					t.Errorf("poll carries write headers: %v", pollReq.header)
				}
			})
		}
	}
}

// TestAsyncPollURLWithoutLeadingSlashFails: a relative URL ARM never sends, but if it
// did the wait must fail rather than report a completed import it never saw.
func TestAsyncPollURLWithoutLeadingSlashFails(t *testing.T) {
	withAsyncTiming(t, time.Second, time.Millisecond)
	hcAsyncFake(t, http.Header{"Azure-Asyncoperation": {"operations/noslash"}}, func(_ int, w http.ResponseWriter, _ *http.Request) {
		hcRespond(w, http.StatusOK, nil, `{"status":"Succeeded"}`)
	})
	if err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`)); err == nil {
		t.Fatal("import = nil, want an error for an unresolvable poll URL")
	}
}

func TestAsyncPollSequences(t *testing.T) {
	type answer struct {
		status int
		body   string
	}
	inProgressForms := map[string]answer{
		"status InProgress":            {http.StatusOK, `{"status":"InProgress"}`},
		"status Running":               {http.StatusOK, `{"status":"Running"}`},
		"202 empty":                    {http.StatusAccepted, ``},
		"202 with status":              {http.StatusAccepted, `{"status":"InProgress"}`},
		"provisioningState InProgress": {http.StatusOK, `{"properties":{"provisioningState":"InProgress"}}`},
	}
	terminals := []struct {
		name    string
		answer  answer
		wantErr bool
		code    string
		detail  string
	}{
		{"Succeeded", answer{http.StatusOK, `{"status":"Succeeded"}`}, false, "", ""},
		{"succeeded lowercase", answer{http.StatusOK, `{"status":"succeeded"}`}, false, "", ""},
		{"Success", answer{http.StatusOK, `{"status":"Success"}`}, false, "", ""},
		{"provisioningState Succeeded", answer{http.StatusOK, `{"properties":{"provisioningState":"Succeeded"}}`}, false, "", ""},
		{"200 without status", answer{http.StatusOK, `{}`}, false, "", ""},
		{"201 without status", answer{http.StatusCreated, `{"id":"x"}`}, false, "", ""},
		{"204", answer{http.StatusNoContent, ``}, false, "", ""},
		{"Failed with nested error", answer{http.StatusOK,
			`{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor","details":[{"code":"DeadOperationMonitor","message":"monitor gone"}]}}`},
			true, "InternalServerError", "DeadOperationMonitor"},
		{"Failed with top-level code", answer{http.StatusOK, `{"status":"Failed","code":"Timeout","message":"slow"}`}, true, "Timeout", ""},
		{"Failed without error", answer{http.StatusOK, `{"status":"Failed"}`}, true, "", ""},
		{"failed lowercase", answer{http.StatusOK, `{"status":"failed","error":{"code":"Conflict"}}`}, true, "Conflict", ""},
		{"Canceled", answer{http.StatusOK, `{"status":"Canceled"}`}, true, "", ""},
		{"Cancelled", answer{http.StatusOK, `{"status":"Cancelled"}`}, true, "", ""},
		{"provisioningState Failed", answer{http.StatusOK, `{"properties":{"provisioningState":"Failed"}}`}, true, "", ""},
	}
	inProgressNames := []string{"status InProgress", "status Running", "202 empty", "202 with status", "provisioningState InProgress"}

	for _, upsert := range hcAsyncUpserts {
		for _, n := range []int{0, 1, 4} {
			for _, ipName := range inProgressNames {
				if n == 0 && ipName != inProgressNames[0] {
					continue // the in-progress form is irrelevant when there is none
				}
				ip := inProgressForms[ipName]
				for _, term := range terminals {
					t.Run(fmt.Sprintf("%s/%d x %s then %s", upsert.name, n, ipName, term.name), func(t *testing.T) {
						withAsyncTiming(t, 5*time.Second, time.Millisecond)
						_, polls := hcAsyncFake(t, http.Header{"Azure-Asyncoperation": {"/operations/seq"}},
							func(k int, w http.ResponseWriter, _ *http.Request) {
								if k <= n {
									hcRespond(w, ip.status, nil, ip.body)
									return
								}
								hcRespond(w, term.answer.status, nil, term.answer.body)
							})
						err := upsert.run(context.Background())
						if got := int(polls.Load()); got != n+1 {
							t.Errorf("polled %d times, want %d", got, n+1)
						}
						if !term.wantErr {
							if err != nil {
								t.Fatalf("%s = %v, want nil", upsert.name, err)
							}
							return
						}
						var apimErr *Error
						if !errors.As(err, &apimErr) {
							t.Fatalf("%s = %v (%T), want *apim.Error", upsert.name, err, err)
						}
						if !errors.Is(err, ErrAsyncOperationFailed) || errors.Is(err, ErrImportWaitTimeout) {
							t.Errorf("err = %v, want ErrAsyncOperationFailed only", err)
						}
						if apimErr.StatusCode != 0 {
							t.Errorf("StatusCode = %d, want 0 for an async result", apimErr.StatusCode)
						}
						if apimErr.Code != term.code || apimErr.DetailCode != term.detail {
							t.Errorf("codes = %q/%q, want %q/%q", apimErr.Code, apimErr.DetailCode, term.code, term.detail)
						}
						if apimErr.Operation != upsert.operation || apimErr.Method != http.MethodGet {
							t.Errorf("Operation/Method = %q/%q, want %q/GET", apimErr.Operation, apimErr.Method, upsert.operation)
						}
						if !strings.Contains(apimErr.Message, "operation status") {
							t.Errorf("Message = %q, want it to name the operation status", apimErr.Message)
						}
					})
				}
			}
		}
	}
}

// TestAsyncUnknownStatusKeepsPolling: a status this package does not know is neither
// success nor failure; it polls until the wait runs out.
func TestAsyncUnknownStatusKeepsPolling(t *testing.T) {
	withAsyncTiming(t, 60*time.Millisecond, 2*time.Millisecond)
	_, polls := hcAsyncFake(t, http.Header{"Azure-Asyncoperation": {"/operations/unknown"}},
		func(_ int, w http.ResponseWriter, _ *http.Request) {
			hcRespond(w, http.StatusOK, nil, `{"status":"Updating"}`)
		})
	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, ErrImportWaitTimeout) {
		t.Fatalf("import = %v, want ErrImportWaitTimeout", err)
	}
	if polls.Load() < 2 {
		t.Errorf("polled %d times, want it to keep polling", polls.Load())
	}
}

func TestAsyncPollHTTPErrors(t *testing.T) {
	statuses := []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusConflict, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
	for _, upsert := range hcAsyncUpserts {
		for _, status := range statuses {
			for _, afterInProgress := range []int{0, 2} {
				t.Run(fmt.Sprintf("%s/%d after %d in progress", upsert.name, status, afterInProgress), func(t *testing.T) {
					withAsyncTiming(t, 5*time.Second, time.Millisecond)
					_, polls := hcAsyncFake(t, http.Header{"Azure-Asyncoperation": {"/operations/err"}},
						func(k int, w http.ResponseWriter, _ *http.Request) {
							if k <= afterInProgress {
								hcRespond(w, http.StatusOK, nil, `{"status":"InProgress"}`)
								return
							}
							hcRespond(w, status, nil, fmt.Sprintf(`{"error":{"code":"Poll%d","message":"poll failed"}}`, status))
						})
					err := upsert.run(context.Background())
					var apimErr *Error
					if !errors.As(err, &apimErr) {
						t.Fatalf("%s = %v (%T), want *apim.Error", upsert.name, err, err)
					}
					if apimErr.StatusCode != status || apimErr.Code != fmt.Sprintf("Poll%d", status) {
						t.Errorf("status/code = %d/%q, want %d/Poll%d", apimErr.StatusCode, apimErr.Code, status, status)
					}
					// GET matters: the controller treats a 404 on a GET as transient (the
					// operation URL may have expired) and a 404 on a write as permanent.
					if apimErr.Method != http.MethodGet {
						t.Errorf("Method = %q, want GET", apimErr.Method)
					}
					if apimErr.Operation != upsert.operation+" (poll)" {
						t.Errorf("Operation = %q, want %q", apimErr.Operation, upsert.operation+" (poll)")
					}
					if errors.Is(err, ErrAsyncOperationFailed) || errors.Is(err, ErrImportWaitTimeout) {
						t.Errorf("a poll HTTP failure must not match an async sentinel: %v", err)
					}
					// The wait stops at the first HTTP failure; the controller decides what next.
					if got := int(polls.Load()); got != afterInProgress+1 {
						t.Errorf("polled %d times, want %d", got, afterInProgress+1)
					}
				})
			}
		}
	}
}

func TestAsyncPollNonJSONBodies(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool // false: success (terminal HTTP status without a status field)
	}{
		{"200 HTML is a terminal status", http.StatusOK, `<html>ok</html>`, false},
		{"200 truncated JSON is a terminal status", http.StatusOK, `{"status":"Succ`, false},
		{"200 null", http.StatusOK, `null`, false},
		{"500 HTML is an error", http.StatusInternalServerError, `<html>boom</html>`, true},
		{"503 empty is an error", http.StatusServiceUnavailable, ``, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withAsyncTiming(t, 2*time.Second, time.Millisecond)
			hcAsyncFake(t, http.Header{"Azure-Asyncoperation": {"/operations/nj"}},
				func(_ int, w http.ResponseWriter, _ *http.Request) {
					hcRespond(w, tc.status, http.Header{"Content-Type": {"text/html"}}, tc.body)
				})
			err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
			if tc.wantErr {
				var apimErr *Error
				if !errors.As(err, &apimErr) || apimErr.StatusCode != tc.status {
					t.Fatalf("import = %v, want *apim.Error with %d", err, tc.status)
				}
				return
			}
			if err != nil {
				t.Fatalf("import = %v, want nil", err)
			}
		})
	}
}

func TestAsyncWaitTimeoutError(t *testing.T) {
	for _, upsert := range hcAsyncUpserts {
		t.Run(upsert.name, func(t *testing.T) {
			withAsyncTiming(t, 40*time.Millisecond, 5*time.Millisecond)
			_, polls := hcAsyncFake(t, http.Header{"Location": {"/operations/slow"}},
				func(_ int, w http.ResponseWriter, _ *http.Request) {
					hcRespond(w, http.StatusAccepted, nil, ``)
				})
			start := time.Now()
			err := upsert.run(context.Background())
			elapsed := time.Since(start)

			if !errors.Is(err, ErrImportWaitTimeout) {
				t.Fatalf("%s = %v, want ErrImportWaitTimeout", upsert.name, err)
			}
			if !errors.Is(fmt.Errorf("controller: %w", err), ErrImportWaitTimeout) {
				t.Error("the timeout must survive wrapping")
			}
			var apimErr *Error
			if !errors.As(err, &apimErr) {
				t.Fatalf("timeout = %T, want *apim.Error", err)
			}
			if apimErr.Operation != upsert.operation || apimErr.Method != http.MethodGet || apimErr.StatusCode != 0 {
				t.Errorf("timeout = %+v, want operation %q, GET, no status", apimErr, upsert.operation)
			}
			if !strings.Contains(apimErr.Message, "40ms") {
				t.Errorf("Message = %q, want it to say how long it waited", apimErr.Message)
			}
			if errors.Is(err, ErrAsyncOperationFailed) {
				t.Error("a timeout is not a failed operation")
			}
			if elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
				t.Errorf("gave up after %s, want about 40ms", elapsed)
			}
			if polls.Load() < 2 {
				t.Errorf("polled %d times before giving up, want several", polls.Load())
			}
		})
	}
}

// TestAsyncZeroWaitPollsNothing: with no time to wait there is nothing to poll for; it
// times out at once.
func TestAsyncZeroWaitPollsNothing(t *testing.T) {
	withAsyncTiming(t, 0, time.Millisecond)
	_, polls := hcAsyncFake(t, http.Header{"Azure-Asyncoperation": {"/operations/zero"}},
		func(_ int, w http.ResponseWriter, _ *http.Request) {
			hcRespond(w, http.StatusOK, nil, `{"status":"Succeeded"}`)
		})
	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, ErrImportWaitTimeout) {
		t.Fatalf("import = %v, want ErrImportWaitTimeout", err)
	}
	if polls.Load() != 0 {
		t.Errorf("polled %d times, want 0", polls.Load())
	}
}

func TestAsyncWaitHonoursCancellation(t *testing.T) {
	withAsyncTiming(t, time.Minute, 5*time.Millisecond)
	_, polls := hcAsyncFake(t, http.Header{"Azure-Asyncoperation": {"/operations/cancel"}},
		func(_ int, w http.ResponseWriter, _ *http.Request) {
			hcRespond(w, http.StatusOK, nil, `{"status":"InProgress"}`)
		})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel once the wait is clearly under way.
		for polls.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	err := ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("import = %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("cancellation did not stop the wait")
	}
}

// ---------------------------------------------------------------------------
// Retry-After on the 202 and on poll answers.
// ---------------------------------------------------------------------------

func TestAsyncRetryAfterCases(t *testing.T) {
	future := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(http.TimeFormat) }
	// boundedWait is the wait of the cases where a Retry-After is honoured and bounded:
	// their one poll comes after a sleep to the deadline, so the count does not depend on
	// how long a localhost round trip takes. pollingWait is for the cases that poll more
	// than once; it leaves room for slow round trips (-race on a loaded CI runner) while
	// an ignored Retry-After would still show up as hundreds of polls.
	const (
		boundedWait = 60 * time.Millisecond
		pollingWait = 500 * time.Millisecond
	)
	cases := []struct {
		name string
		// onAccepted / onPoll are the Retry-After values on the 202 and on every poll answer.
		onAccepted string
		onPoll     string
		wait       time.Duration
		// minPolls and maxPolls bound the polls with a 1ms interval; a Retry-After that is
		// honoured and bounded leaves exactly one poll, at the end of the wait.
		minPolls, maxPolls int32
	}{
		{"seconds on the 202, bounded", "3600", "3600", boundedWait, 1, 1},
		// The first poll comes after the 1ms interval, the second at the deadline. A first
		// round trip slower than the whole wait leaves only the one.
		{"seconds only on the poll answer, bounded", "", "3600", pollingWait, 1, 2},
		{"HTTP date on the 202, bounded", future(time.Hour), future(time.Hour), boundedWait, 1, 1},
		{"HTTP date in a year, bounded", future(365 * 24 * time.Hour), future(365 * 24 * time.Hour), boundedWait, 1, 1},
		{"68 years in seconds, bounded", "2147483648", "2147483648", boundedWait, 1, 1},
		// seconds*time.Second would wrap negative here; unbounded, the wait busy-polled ARM
		// (over a thousand GETs in 60ms) instead of polling once at the end of the wait.
		{"seconds that overflow time.Duration, bounded", "9223372037", "9223372037", boundedWait, 1, 1},
		{"seconds far beyond int64 fall back to the interval", "99999999999999999999", "99999999999999999999", pollingWait, 3, 1 << 30},
		{"zero falls back to the interval", "0", "0", pollingWait, 3, 1 << 30},
		{"negative falls back to the interval", "-5", "-5", pollingWait, 3, 1 << 30},
		{"date in the past falls back to the interval", "Mon, 02 Jan 2006 15:04:05 GMT", "Mon, 02 Jan 2006 15:04:05 GMT", pollingWait, 3, 1 << 30},
		{"garbage falls back to the interval", "later", "later", pollingWait, 3, 1 << 30},
		{"fraction falls back to the interval", "1.5", "1.5", pollingWait, 3, 1 << 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withAsyncTiming(t, tc.wait, time.Millisecond)
			accepted := http.Header{"Azure-Asyncoperation": {"/operations/ra"}}
			if tc.onAccepted != "" {
				accepted.Set("Retry-After", tc.onAccepted)
			}
			_, polls := hcAsyncFake(t, accepted, func(_ int, w http.ResponseWriter, _ *http.Request) {
				h := http.Header{}
				if tc.onPoll != "" {
					h.Set("Retry-After", tc.onPoll)
				}
				hcRespond(w, http.StatusOK, h, `{"status":"InProgress"}`)
			})
			start := time.Now()
			err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
			elapsed := time.Since(start)
			if !errors.Is(err, ErrImportWaitTimeout) {
				t.Fatalf("import = %v, want ErrImportWaitTimeout", err)
			}
			// The bound is checked on time, not on poll counts: a Retry-After of an hour
			// that was not bounded would wait an hour.
			if elapsed < tc.wait {
				t.Errorf("gave up after %s, before the %s wait was over", elapsed, tc.wait)
			}
			if elapsed > tc.wait+2*time.Second {
				t.Errorf("waited %s; the wait must never outlast AsyncWaitTimeout (%s) by more than a round trip", elapsed, tc.wait)
			}
			if got := polls.Load(); got < tc.minPolls || got > tc.maxPolls {
				t.Errorf("polled %d times, want between %d and %d", got, tc.minPolls, tc.maxPolls)
			}
		})
	}
}

// TestRetryAfterOverflowIsBounded pins the parser directly: a number of seconds too
// large for time.Duration must not come back as a negative or tiny delay.
// time.Duration(seconds) * time.Second overflows int64 for anything above
// 9223372036 s; unchecked, that gave a negative or arbitrary short delay and the wait
// polled with no pause.
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
