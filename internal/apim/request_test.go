package apim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeARM stands in for management.azure.com. Every request is recorded; handler
// decides the answer.
type fakeARM struct {
	mu       sync.Mutex
	requests []*http.Request
	server   *httptest.Server
	router   *hcRouter
}

func newFakeARM(t *testing.T, handler http.HandlerFunc) *fakeARM {
	t.Helper()
	f := &fakeARM{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(f.server.Close)
	f.router = routeToFake(t, f.server)
	return f
}

func (f *fakeARM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeARM) request(i int) *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func deploymentConfig() APIMDeploymentConfig {
	return APIMDeploymentConfig{
		SubscriptionID: "sub-1", ResourceGroup: "rg-1", ServiceName: "apim-1",
		APIID: "orders", RoutePrefix: "/orders", ServiceURL: "https://orders.internal",
		BearerToken: "tok", ProductIDs: []string{"p1", "p2"}, TagIDs: []string{"t1"},
		SubscriptionRequired: true,
	}
}

func tagConfig() APIMTagConfig {
	return APIMTagConfig{
		SubscriptionID: "sub-1", ResourceGroup: "rg-1", ServiceName: "apim-1",
		TagID: "t1", DisplayName: "Tag one", BearerToken: "tok",
	}
}

func policyConfig() APIMInboundPolicyConfig {
	return APIMInboundPolicyConfig{
		SubscriptionID: "sub-1", ResourceGroup: "rg-1", ServiceName: "apim-1",
		APIID: "orders", PolicyContent: "<policies/>", BearerToken: "tok",
	}
}

// writeJSON answers with status and a JSON body.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// TestEveryCallGoesThroughTheSharedClient runs each exported write and read against
// the fake ARM. armHost points at an unroutable host that only httpClient's transport
// re-addresses to the fake, so a call that used http.DefaultClient fails on DNS, and one
// with a hard-coded host would try the real management.azure.com; neither shows up here.
func TestEveryCallGoesThroughTheSharedClient(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context) error
		want int // requests the fake must see
	}{
		{"GetAPI", func(ctx context.Context) error { _, _, err := GetAPI(ctx, deploymentConfig()); return err }, 1},
		// GET for the etag, then the PUT
		{"ImportOpenAPIDefinitionToAPIM", hcDropResult(hcImport), 2},
		{"UpsertWebSocketAPI", hcDropResult(hcUpsertWebSocket), 2},
		{"AssignProductsToAPI", func(ctx context.Context) error { return AssignProductsToAPI(ctx, deploymentConfig()) }, 2},
		{"AssignTagsToAPI", func(ctx context.Context) error { return AssignTagsToAPI(ctx, deploymentConfig()) }, 1},
		{"UpsertProduct", func(ctx context.Context) error {
			cfg := productConfig()
			cfg.DisplayName = "P1"
			return UpsertProduct(ctx, cfg)
		}, 1},
		{"DeleteProduct", func(ctx context.Context) error { return DeleteProduct(ctx, productConfig()) }, 1},
		{"UpsertTag", func(ctx context.Context) error { return UpsertTag(ctx, tagConfig()) }, 1},
		{"UpsertInboundPolicy", func(ctx context.Context) error { return UpsertInboundPolicy(ctx, policyConfig()) }, 1},
		{"GetAPIMServiceDetails", func(ctx context.Context) error {
			_, _, err := GetAPIMServiceDetails(ctx, deploymentConfig())
			return err
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, `{"properties":{}}`)
			})
			if err := tc.call(context.Background()); err != nil {
				t.Fatalf("%s() = %v, want nil", tc.name, err)
			}
			if got := fake.count(); got != tc.want {
				t.Fatalf("fake ARM saw %d requests, want %d", got, tc.want)
			}
			if got := fake.router.sent.Load(); got != int64(tc.want) {
				t.Fatalf("httpClient sent %d requests, want %d: something reached the fake around it", got, tc.want)
			}
			for i := range tc.want {
				req := fake.request(i)
				if got := req.Header.Get("Authorization"); got != "Bearer tok" {
					t.Errorf("request %d Authorization = %q, want Bearer tok", i, got)
				}
				if !strings.HasPrefix(req.URL.Path, "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.ApiManagement/service/apim-1") {
					t.Errorf("request %d path = %s, want it under the APIM service", i, req.URL.Path)
				}
				if got := req.URL.Query().Get("api-version"); got != apiVersion {
					t.Errorf("request %d api-version = %q, want %s", i, got, apiVersion)
				}
			}
		})
	}
}

// TestNonSuccessStatusesAreTypedErrors pins what a controller gets back for the
// statuses it has to tell apart: the status, both Azure codes, the message and the
// method, all through errors.As.
func TestNonSuccessStatusesAreTypedErrors(t *testing.T) {
	cases := []struct {
		status     int
		body       string
		code       string
		detailCode string
		message    string
	}{
		{http.StatusBadRequest,
			`{"error":{"code":"ValidationError","message":"One or more fields contain incorrect values:","details":[{"code":"ValidationError","target":"displayName","message":"Display name is required."}]}}`,
			"ValidationError", "ValidationError", "Display name is required."},
		{http.StatusUnauthorized, `{"error":{"code":"InvalidAuthenticationToken","message":"The access token is invalid."}}`,
			"InvalidAuthenticationToken", "", "The access token is invalid."},
		{http.StatusForbidden, `{"error":{"code":"AuthorizationFailed","message":"The client does not have authorization"}}`,
			"AuthorizationFailed", "", "does not have authorization"},
		{http.StatusNotFound, `{"error":{"code":"ResourceNotFound","message":"Api not found."}}`,
			"ResourceNotFound", "", "Api not found."},
		{http.StatusConflict, `{"error":{"code":"Conflict","message":"Operation on the API is in progress"}}`,
			"Conflict", "", "in progress"},
		{http.StatusPreconditionFailed, `{"error":{"code":"PreconditionFailed","message":"The If-Match header did not match."}}`,
			"PreconditionFailed", "", "If-Match"},
		{http.StatusUnprocessableEntity, `{"error":{"code":"ManagementApiRequestFailed","message":"Management API timed out","details":[{"code":"Timeout"}]}}`,
			"ManagementApiRequestFailed", "Timeout", "Management API timed out"},
		{http.StatusTooManyRequests, `{"error":{"code":"TooManyRequests","message":"Rate limit exceeded"}}`,
			"TooManyRequests", "", "Rate limit exceeded"},
		{http.StatusInternalServerError, `{"error":{"code":"InternalServerError","message":"Something went wrong"}}`,
			"InternalServerError", "", "Something went wrong"},
		{http.StatusServiceUnavailable, `upstream connect error`, "", "", "upstream connect error"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, tc.status, tc.body) })
			err := UpsertTag(context.Background(), tagConfig())
			var apimErr *Error
			if !errors.As(err, &apimErr) {
				t.Fatalf("UpsertTag() = %v (%T), want *apim.Error", err, err)
			}
			if apimErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apimErr.StatusCode, tc.status)
			}
			if apimErr.Code != tc.code {
				t.Errorf("Code = %q, want %q", apimErr.Code, tc.code)
			}
			if apimErr.DetailCode != tc.detailCode {
				t.Errorf("DetailCode = %q, want %q", apimErr.DetailCode, tc.detailCode)
			}
			if !strings.Contains(apimErr.Message, tc.message) {
				t.Errorf("Message = %q, want it to contain %q", apimErr.Message, tc.message)
			}
			if apimErr.Method != http.MethodPut {
				t.Errorf("Method = %q, want PUT", apimErr.Method)
			}
			if apimErr.Operation != "upsert tag t1" {
				t.Errorf("Operation = %q, want %q", apimErr.Operation, "upsert tag t1")
			}
			if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
				t.Errorf("Error() = %q, want it to contain the status", err.Error())
			}
			if errors.Is(err, ErrImportWaitTimeout) || errors.Is(err, ErrAsyncOperationFailed) {
				t.Errorf("an HTTP failure must not look like an async one: %v", err)
			}
		})
	}
}

// TestEachWriteReturnsTypedErrors makes sure no write slipped past the helper with an
// untyped fmt.Errorf of its own.
func TestEachWriteReturnsTypedErrors(t *testing.T) {
	writes := map[string]func(ctx context.Context) error{
		"AssignProductsToAPI":           func(ctx context.Context) error { return AssignProductsToAPI(ctx, deploymentConfig()) },
		"AssignTagsToAPI":               func(ctx context.Context) error { return AssignTagsToAPI(ctx, deploymentConfig()) },
		"UpsertProduct":                 func(ctx context.Context) error { return UpsertProduct(ctx, productConfig()) },
		"DeleteProduct":                 func(ctx context.Context) error { return DeleteProduct(ctx, productConfig()) },
		"UpsertTag":                     func(ctx context.Context) error { return UpsertTag(ctx, tagConfig()) },
		"UpsertInboundPolicy":           func(ctx context.Context) error { return UpsertInboundPolicy(ctx, policyConfig()) },
		"ImportOpenAPIDefinitionToAPIM": hcDropResult(hcImport),
		"UpsertWebSocketAPI":            hcDropResult(hcUpsertWebSocket),
		"GetAPIMServiceDetails": func(ctx context.Context) error {
			_, _, err := GetAPIMServiceDetails(ctx, deploymentConfig())
			return err
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusPreconditionFailed, `{"error":{"code":"PreconditionFailed","message":"etag"}}`)
			})
			var apimErr *Error
			if err := write(context.Background()); !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("%s() = %v, want *apim.Error with 412", name, err)
			}
		})
	}
}

// TestAssignmentErrorsNameTheTarget keeps the product or tag that failed in the error,
// since one reconcile assigns several.
func TestAssignmentErrorsNameTheTarget(t *testing.T) {
	newFakeARM(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/products/p2/") {
			writeJSON(w, http.StatusNotFound, `{"error":{"code":"ResourceNotFound","message":"Product not found."}}`)
			return
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
	err := AssignProductsToAPI(context.Background(), deploymentConfig())
	var apimErr *Error
	if !errors.As(err, &apimErr) {
		t.Fatalf("AssignProductsToAPI() = %v, want *apim.Error", err)
	}
	if apimErr.Operation != "assign API to product p2" || apimErr.StatusCode != http.StatusNotFound {
		t.Errorf("got %q %d, want the p2 assignment with 404", apimErr.Operation, apimErr.StatusCode)
	}
}

func TestTransportErrorsAreNotTyped(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // nothing listens there any more
	t.Cleanup(UseEndpoint(url, &http.Client{Timeout: 2 * time.Second}))

	err := UpsertTag(context.Background(), tagConfig())
	if err == nil {
		t.Fatal("UpsertTag() = nil, want a transport error")
	}
	var apimErr *Error
	if errors.As(err, &apimErr) {
		t.Errorf("a transport failure came back as *apim.Error: %v", err)
	}
	if !strings.Contains(err.Error(), "upsert tag t1") {
		t.Errorf("Error() = %q, want it to name the operation", err.Error())
	}
}

func TestGetAPINotFoundMeansAbsent(t *testing.T) {
	newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"error":{"code":"ResourceNotFound"}}`)
	})
	etag, exists, err := GetAPI(context.Background(), deploymentConfig())
	if err != nil || exists || etag != "" {
		t.Errorf("GetAPI() = %q, %v, %v; want \"\", false, nil", etag, exists, err)
	}
}

func TestGetAPIFormatsTheETag(t *testing.T) {
	newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `W/"abc"`)
		writeJSON(w, http.StatusOK, `{}`)
	})
	etag, exists, err := GetAPI(context.Background(), deploymentConfig())
	if err != nil || !exists || etag != `"abc"` {
		t.Errorf("GetAPI() = %q, %v, %v; want \"abc\" quoted, true, nil", etag, exists, err)
	}
}

// importBody is the JSON envelope the import PUT sends.
type importBody struct {
	Properties struct {
		Format               string `json:"format"`
		Value                string `json:"value"`
		Path                 string `json:"path"`
		ServiceURL           string `json:"serviceUrl"`
		SubscriptionRequired *bool  `json:"subscriptionRequired"`
	} `json:"properties"`
}

// importFakeARM answers every request with ok and records the body of each PUT.
func importFakeARM(t *testing.T, ok int) (*fakeARM, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var puts []string
	fake := newFakeARM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"e1"`)
		}
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			puts = append(puts, string(b))
			mu.Unlock()
		}
		writeJSON(w, ok, `{}`)
	})
	return fake, &puts
}

func decodeImportBody(t *testing.T, raw string) importBody {
	t.Helper()
	var b importBody
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("import body is not JSON: %v\n%s", err, raw)
	}
	return b
}

// TestImportRequestShape pins the import PUT: a JSON envelope carrying the document, the
// path, the backend serviceUrl and the subscription requirement; no import/path query; the
// literal ";rev=" separator; and the etag from the GET.
func TestImportRequestShape(t *testing.T) {
	fake, puts := importFakeARM(t, http.StatusOK)
	cfg := deploymentConfig()
	doc := `{"openapi":"3.0.0","info":{"title":"Orders"}}`
	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), cfg, []byte(doc))
	if err != nil {
		t.Fatalf("import = %v", err)
	}
	if result.Accepted() {
		t.Errorf("a 200 import = %+v, want an empty WriteResult (APIM finished it)", result)
	}
	put := fake.request(1)
	if put.Method != http.MethodPut {
		t.Fatalf("second request = %s, want PUT", put.Method)
	}
	q := put.URL.Query()
	if q.Has("import") || q.Has("path") || q.Has("createRevision") {
		t.Errorf("query = %v, want only api-version: path and document travel in the body", q)
	}
	if got := put.Header.Get("If-Match"); got != `"e1"` {
		t.Errorf("If-Match = %q, want the etag from GET", got)
	}
	if got := put.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	b := decodeImportBody(t, (*puts)[0])
	if b.Properties.Format != "openapi+json" || b.Properties.Value != doc {
		t.Errorf("format/value = %q / %q, want openapi+json and the document unchanged", b.Properties.Format, b.Properties.Value)
	}
	if b.Properties.Path != cfg.RoutePrefix || b.Properties.ServiceURL != cfg.ServiceURL {
		t.Errorf("path/serviceUrl = %q / %q, want %q / %q", b.Properties.Path, b.Properties.ServiceURL, cfg.RoutePrefix, cfg.ServiceURL)
	}
	if b.Properties.SubscriptionRequired == nil || *b.Properties.SubscriptionRequired != cfg.SubscriptionRequired {
		t.Errorf("subscriptionRequired = %v, want %v", b.Properties.SubscriptionRequired, cfg.SubscriptionRequired)
	}

	// A revision skips the GET and addresses "orders;rev=3" with a literal separator.
	fake, _ = importFakeARM(t, http.StatusCreated)
	cfg.Revision = "3"
	if _, err := ImportOpenAPIDefinitionToAPIM(context.Background(), cfg, []byte(`{}`)); err != nil {
		t.Fatalf("revision import = %v", err)
	}
	put = fake.request(0)
	if !strings.HasSuffix(put.URL.EscapedPath(), "/apis/orders;rev=3") {
		t.Errorf("path = %s, want it to end in /apis/orders;rev=3", put.URL.EscapedPath())
	}
	if put.URL.Query().Get("createRevision") != "true" || put.Header.Get("If-Match") != "*" {
		t.Errorf("revision import must send createRevision=true and If-Match: *")
	}
}

// TestImportSetsServiceURLOverTheDocumentsServers is the bug this envelope fixes: a
// document whose servers point at the in-cluster address it was fetched from must still
// import with the configured backend, in the import itself.
func TestImportSetsServiceURLOverTheDocumentsServers(t *testing.T) {
	_, puts := importFakeARM(t, http.StatusOK)
	cfg := deploymentConfig()
	cfg.ServiceURL = "https://sharc-api.crm-dev.external.hedinit.io"
	doc := `{"openapi":"3.0.1","servers":[{"url":"http://crm-sharc-api.crm-sharc-dev.svc.cluster.local/"}]}`
	if _, err := ImportOpenAPIDefinitionToAPIM(context.Background(), cfg, []byte(doc)); err != nil {
		t.Fatalf("import = %v", err)
	}
	b := decodeImportBody(t, (*puts)[0])
	if b.Properties.ServiceURL != cfg.ServiceURL {
		t.Errorf("serviceUrl = %q, want the configured %q, not the document's servers", b.Properties.ServiceURL, cfg.ServiceURL)
	}
	if b.Properties.Value != doc {
		t.Errorf("the document must be sent unchanged; the envelope's serviceUrl wins in APIM")
	}
}

// TestImportFormat pins the format chosen per document. Only Swagger 2.0 YAML is converted
// (APIM has no Swagger-YAML format); everything else is sent byte for byte, since a YAML 1.1
// conversion changes an OpenAPI 3 definition (version 1.0 becomes the number 1).
func TestImportFormat(t *testing.T) {
	cases := []struct {
		name, doc, format string
		wantJSON          string // empty: the document itself
	}{
		{"openapi 3 json", `{"openapi":"3.0.2","info":{}}`, "openapi+json", ""},
		{"openapi 3.1 json", `{"openapi":"3.1.0"}`, "openapi+json", ""},
		{"swagger 2 json", `{"swagger":"2.0","host":"svc.cluster.local"}`, "swagger-json", ""},
		{"openapi 3 yaml is sent as is", "openapi: 3.0.0\ninfo:\n  title: x\n", "openapi", ""},
		{"openapi 3 yaml keeps YAML 1.1 lookalikes", "openapi: 3.0.0\ninfo:\n  version: 1.0\n  x-flag: yes\n", "openapi", ""},
		{"openapi 3 yaml with a quoted key", "\"openapi\": \"3.1.0\"\npaths: {}\n", "openapi", ""},
		{"openapi 3 flow-mapping yaml", "{openapi: 3.0.0, paths: {}}\n", "openapi", ""},
		{"yaml without version is sent as openapi", "info:\n  title: x\n", "openapi", ""},
		{"swagger 2 yaml", "swagger: '2.0'\nbasePath: /v1\n", "swagger-json", `{"basePath":"/v1","swagger":"2.0"}`},
		{"swagger 2 yaml with a quoted key", "'swagger': '2.0'\nhost: svc\n", "swagger-json", `{"host":"svc","swagger":"2.0"}`},
		{"json without version falls back", `{}`, "openapi+json", ""},
		{"json keeps key order and spacing", "{ \"openapi\": \"3.0.0\",  \"a\": 1 }", "openapi+json", ""},
		{"json swagger keeps key order", `{"swagger":"2.0","info":{},"basePath":"/v1"}`, "swagger-json", ""},
		// Fuzz findings of 2026-10-08 (fuzz_test.go): none of these is Swagger 2.0.
		{"JSON-F1 Swagger in another case is not swagger", `{"Swagger":"2.0","openapi":"3.0.0"}`, "openapi+json", ""},
		{"YAML-F3 colon in a key is not swagger", "swagger:v2: true\nopenapi: 3.0.0\n", "openapi", ""},
		{"YAML-F2 second document is not read", "openapi: 3.0.0\n---\nswagger: '2.0'\n", "openapi", ""},
		{"YAML-F5 null swagger is not swagger", "swagger: ~\nopenapi: 3.0.0\n", "openapi", ""},
		{"YAML-F1 swagger after a lone CR", "info: x\rswagger: '2.0'\r", "swagger-json", `{"info":"x","swagger":"2.0"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			format, doc, err := importFormat([]byte(c.doc))
			if err != nil {
				t.Fatalf("importFormat = %v", err)
			}
			if format != c.format {
				t.Errorf("format = %q, want %q", format, c.format)
			}
			want := c.wantJSON
			if want == "" {
				want = c.doc
			}
			if string(doc) != want {
				t.Errorf("document = %s, want %s", doc, want)
			}
		})
	}

	for _, bad := range []string{"[1,2]", "- a\n- b\n", "key: [unclosed\n"} {
		if _, _, err := importFormat([]byte(bad)); err == nil {
			t.Errorf("importFormat(%q) = nil error, want one for a non-object document", bad)
		}
	}
}

// TestImportBadDocumentSendsNothing: a document the envelope cannot carry fails before any
// request reaches ARM.
func TestImportBadDocumentSendsNothing(t *testing.T) {
	fake, _ := importFakeARM(t, http.StatusOK)
	for _, bad := range []string{"[1,2]", "- a\n- b\n", "key: [unclosed\n"} {
		if _, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(bad)); err == nil {
			t.Errorf("import of %q succeeded, want an error", bad)
		}
	}
	if n := fake.count(); n != 0 {
		t.Errorf("ARM saw %d requests, want 0", n)
	}
}

// asyncARM answers the etag GET with 404 and the API PUT with 202 plus accepted, and
// counts every request to /operations/: none may come, since a write never waits.
func asyncARM(t *testing.T, accepted http.Header) (*fakeARM, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	fake := newFakeARM(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/operations/"):
			polls.Add(1)
			writeJSON(w, http.StatusOK, `{"status":"InProgress"}`)
		case r.Method == http.MethodGet:
			writeJSON(w, http.StatusNotFound, `{"error":{"code":"ResourceNotFound"}}`)
		default:
			for k, v := range accepted {
				w.Header()[k] = v
			}
			writeJSON(w, http.StatusAccepted, ``)
		}
	})
	return fake, &polls
}

// TestAsyncImportReturnsTheOperation: a 202 hands the operation to the caller at once; the
// import does not read it.
func TestAsyncImportReturnsTheOperation(t *testing.T) {
	// A relative operation URL is resolved against the ARM host.
	fake, polls := asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/op1"}})
	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if err != nil {
		t.Fatalf("import = %v, want nil", err)
	}
	if !result.Accepted() || result.OperationURL != hcUnroutableHost+"/operations/op1" {
		t.Errorf("WriteResult = %+v, want Accepted with %s/operations/op1", result, hcUnroutableHost)
	}
	if result.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0 without a Retry-After header", result.RetryAfter)
	}
	if got := polls.Load(); got != 0 {
		t.Errorf("read the operation %d times, want 0: the caller follows it", got)
	}
	if got := fake.count(); got != 2 {
		t.Errorf("requests = %d, want the etag GET and the PUT", got)
	}
}

func TestAsyncUpsertViaLocationHeader(t *testing.T) {
	_, polls := asyncARM(t, http.Header{"Location": {hcUnroutableHost + "/operations/op2"}})
	result, err := UpsertWebSocketAPI(context.Background(), deploymentConfig())
	if err != nil {
		t.Fatalf("websocket upsert = %v, want nil", err)
	}
	if result.OperationURL != hcUnroutableHost+"/operations/op2" {
		t.Errorf("OperationURL = %q, want the Location", result.OperationURL)
	}
	if got := polls.Load(); got != 0 {
		t.Errorf("read the operation %d times, want 0", got)
	}
}

// TestAsyncImportNeverWaits: neither a Retry-After of an hour nor a context without a
// deadline holds the write; the Retry-After goes back to the caller.
func TestAsyncImportNeverWaits(t *testing.T) {
	_, polls := asyncARM(t, http.Header{
		"Azure-Asyncoperation": {"/operations/op7"},
		"Retry-After":          {"3600"},
	})
	start := time.Now()
	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if err != nil {
		t.Fatalf("import = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("import took %s; a 202 must return at once", elapsed)
	}
	if result.RetryAfter != time.Hour {
		t.Errorf("RetryAfter = %s, want 1h from the 202", result.RetryAfter)
	}
	if got := polls.Load(); got != 0 {
		t.Errorf("read the operation %d times, want 0", got)
	}
}

// TestAsync202WithoutOperationURLIsAnError: a 202 that names nothing to follow leaves the
// write running in APIM with no way to tell when it ends; it is not reported as done.
func TestAsync202WithoutOperationURLIsAnError(t *testing.T) {
	asyncARM(t, nil)
	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, ErrNoOperationURL) {
		t.Fatalf("import = %+v, %v; want ErrNoOperationURL", result, err)
	}
	var apimErr *Error
	if !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusAccepted ||
		apimErr.Method != http.MethodPut || apimErr.Operation != "import API" {
		t.Errorf("err = %+v, want an *apim.Error for the import PUT with status 202", apimErr)
	}
	if result.Accepted() {
		t.Errorf("WriteResult = %+v, want empty with the error", result)
	}
}

// TestGetOperationStateFailedCarriesTheAzureCode: an operation that ended Failed reports the
// Azure codes of its result, not an HTTP status.
func TestGetOperationStateFailedCarriesTheAzureCode(t *testing.T) {
	newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK,
			`{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor","details":[{"code":"DeadOperationMonitor"}]}}`)
	})
	state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/op3")
	if err != nil {
		t.Fatalf("GetOperationState() error = %v, want the failure in the state", err)
	}
	if state.Status != OperationFailed {
		t.Fatalf("Status = %s, want Failed", state.Status)
	}
	var apimErr *Error
	if !errors.As(state.Err, &apimErr) {
		t.Fatalf("Err = %v (%T), want *apim.Error", state.Err, state.Err)
	}
	if !errors.Is(state.Err, ErrAsyncOperationFailed) {
		t.Errorf("errors.Is(Err, ErrAsyncOperationFailed) = false for %v", state.Err)
	}
	if apimErr.Code != "InternalServerError" || apimErr.DetailCode != "DeadOperationMonitor" {
		t.Errorf("codes = %q/%q, want InternalServerError/DeadOperationMonitor", apimErr.Code, apimErr.DetailCode)
	}
	if apimErr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 for an async result", apimErr.StatusCode)
	}
	if !strings.Contains(state.Err.Error(), "DeadOperationMonitor") {
		t.Errorf("Error() = %q, want the Azure message", state.Err.Error())
	}
}

func TestGetOperationStateHTTPErrorIsTyped(t *testing.T) {
	newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":{"code":"InternalServerError","message":"boom"}}`)
	})
	state, err := GetOperationState(context.Background(), "tok", hcUnroutableHost+"/operations/op5")
	var apimErr *Error
	if !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusInternalServerError || apimErr.Method != http.MethodGet {
		t.Fatalf("GetOperationState() = %+v, %v; want *apim.Error 500 from the GET", state, err)
	}
	if errors.Is(err, ErrAsyncOperationFailed) {
		t.Errorf("a failed read is not a failed operation: %v", err)
	}
}

func TestGetOperationStateStopsWithTheContext(t *testing.T) {
	fake := newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"InProgress"}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GetOperationState(ctx, "tok", hcUnroutableHost+"/operations/op9"); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetOperationState() = %v, want context.Canceled", err)
	}
	if got := fake.count(); got != 0 {
		t.Errorf("sent %d requests with a cancelled context", got)
	}
}

// TestIfMatchForUpsert pins the existence check before an API upsert: a 404 means a new API
// (If-Match: *), an existing one is updated on its etag, and any other failure of the GET
// stops the write before it is sent.
func TestIfMatchForUpsert(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		etag    string
		want    string
		wantErr int // the status of the returned *Error; 0 for none
	}{
		{name: "absent", status: http.StatusNotFound, want: "*"},
		{name: "existing", status: http.StatusOK, etag: `W/"e1"`, want: `"e1"`},
		{name: "existing without etag", status: http.StatusOK, want: "*"},
		{name: "server error", status: http.StatusInternalServerError, wantErr: http.StatusInternalServerError},
		{name: "throttled", status: http.StatusTooManyRequests, wantErr: http.StatusTooManyRequests},
		{name: "forbidden", status: http.StatusForbidden, wantErr: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.etag != "" {
					w.Header().Set("ETag", tc.etag)
				}
				writeJSON(w, tc.status, `{"error":{"code":"Whatever"}}`)
			})
			got, err := ifMatchForUpsert(context.Background(), deploymentConfig())
			if tc.wantErr != 0 {
				var apimErr *Error
				if !errors.As(err, &apimErr) || apimErr.StatusCode != tc.wantErr || apimErr.Method != http.MethodGet {
					t.Fatalf("ifMatchForUpsert() = %q, %v; want the GET's %d", got, err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("If-Match = %q alongside an error, want \"\"", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ifMatchForUpsert() = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

// TestFailedExistenceCheckSendsNoUpsert: an APIM that cannot answer the GET is not sent the
// import (or websocket upsert) after it. Before, the GET failure was logged and the PUT went
// out with If-Match: *, overwriting whatever was there.
func TestFailedExistenceCheckSendsNoUpsert(t *testing.T) {
	upserts := map[string]func(ctx context.Context) (WriteResult, error){
		"import":    hcImport,
		"websocket": hcUpsertWebSocket,
	}
	for name, upsert := range upserts {
		t.Run(name, func(t *testing.T) {
			fake := newFakeARM(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					writeJSON(w, http.StatusInternalServerError, `{"error":{"code":"InternalServerError"}}`)
					return
				}
				writeJSON(w, http.StatusOK, `{}`)
			})
			result, err := upsert(context.Background())
			var apimErr *Error
			if !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusInternalServerError || apimErr.Operation != "get API" {
				t.Fatalf("%s = %+v, %v; want the GET's 500", name, result, err)
			}
			for i := range fake.count() {
				if m := fake.request(i).Method; m != http.MethodGet {
					t.Errorf("request %d = %s, want only the GET", i, m)
				}
			}
			if got := fake.count(); got != 1 {
				t.Errorf("requests = %d, want the GET alone", got)
			}
		})
	}
}

func TestRetryAfterParsing(t *testing.T) {
	def := 10 * time.Second
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	cases := []struct {
		value   string
		min     time.Duration
		max     time.Duration
		comment string
	}{
		{"", def, def, "absent"},
		{"5", 5 * time.Second, 5 * time.Second, "seconds"},
		{" 7 ", 7 * time.Second, 7 * time.Second, "whitespace"},
		{"0", def, def, "zero falls back"},
		{"-3", def, def, "negative falls back"},
		{"soon", def, def, "garbage falls back"},
		{future, 28 * time.Second, 30 * time.Second, "HTTP date"},
		{past, def, def, "date in the past falls back"},
	}
	for _, tc := range cases {
		t.Run(tc.comment, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			if got := retryAfter(h, def); got < tc.min || got > tc.max {
				t.Errorf("retryAfter(%q) = %s, want between %s and %s", tc.value, got, tc.min, tc.max)
			}
		})
	}
}

func TestParseARMError(t *testing.T) {
	long := strings.Repeat("x", 3000)
	cases := []struct {
		name, body, code, detail, message string
	}{
		{"wrapped", `{"error":{"code":"Conflict","message":"busy"}}`, "Conflict", "", "busy"},
		{"details", `{"error":{"code":"ValidationError","message":"bad:","details":[{"code":"InvalidFormat","message":"field x"},{"code":"Other"}]}}`,
			"ValidationError", "InvalidFormat", "bad: field x"},
		{"top level", `{"code":"PreconditionFailed","message":"etag mismatch"}`, "PreconditionFailed", "", "etag mismatch"},
		{"async result", `{"status":"Failed","error":{"code":"Timeout","message":"slow"}}`, "Timeout", "", "slow"},
		{"plain text", `Bad Gateway`, "", "", "Bad Gateway"},
		{"empty", ``, "", "", ""},
		{"json without error", `{"status":"Failed"}`, "", "", `{"status":"Failed"}`},
		{"long text is truncated", long, "", "", strings.Repeat("x", maxErrorBodyInMessage) + "…"},
		{"long JSON message is truncated", `{"error":{"code":"ValidationError","message":"` + long + `"}}`,
			"ValidationError", "", strings.Repeat("x", maxErrorBodyInMessage) + "…"},
		{"long detail message is truncated", `{"error":{"code":"ValidationError","message":"bad:","details":[{"code":"InvalidFormat","message":"` + long + `"}]}}`,
			"ValidationError", "InvalidFormat", "bad: " + strings.Repeat("x", maxErrorBodyInMessage-len("bad: ")) + "…"},
		{"top-level long JSON message is truncated", `{"code":"PreconditionFailed","message":"` + long + `"}`,
			"PreconditionFailed", "", strings.Repeat("x", maxErrorBodyInMessage) + "…"},
		{"JSON message at the limit is kept", `{"error":{"code":"C","message":"` + long[:maxErrorBodyInMessage] + `"}}`,
			"C", "", long[:maxErrorBodyInMessage]},
		{"long JSON message is cut on a rune boundary", `{"error":{"code":"C","message":"x` + strings.Repeat("é", 1000) + `"}}`,
			"C", "", "x" + strings.Repeat("é", (maxErrorBodyInMessage-1)/2) + "…"},
		// ARM-F1 (fuzz finding of 2026-10-08): a short body keeps no invalid UTF-8.
		{"ARM-F1 invalid UTF-8 in a short body", "\xff\xfe not UTF-8", "", "", "\uFFFD not UTF-8"},
		{"ARM-F1 invalid UTF-8 in the middle", "a\xc3b", "", "", "a\uFFFDb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, detail, message := parseARMError([]byte(tc.body))
			if code != tc.code || detail != tc.detail || message != tc.message {
				t.Errorf("parseARMError() = %q, %q, %q; want %q, %q, %q", code, detail, message, tc.code, tc.detail, tc.message)
			}
		})
	}
}

func TestErrorRendering(t *testing.T) {
	cases := []struct {
		err  *Error
		want string
	}{
		{&Error{Operation: "upsert tag t1", StatusCode: 412, Code: "PreconditionFailed", Message: "etag"},
			"upsert tag t1 failed: 412 Precondition Failed: PreconditionFailed: etag"},
		{&Error{Operation: "import API", Code: "InternalServerError", DetailCode: "DeadOperationMonitor", Message: "m", Err: ErrAsyncOperationFailed},
			"import API failed: APIM async operation failed: InternalServerError/DeadOperationMonitor: m"},
		{&Error{Operation: "import API", Err: ErrImportWaitTimeout, Message: "operation still running after 5m0s"},
			"import API failed: timed out waiting for APIM async operation: operation still running after 5m0s"},
		{&Error{Operation: "delete product p1", StatusCode: 403},
			"delete product p1 failed: 403 Forbidden"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() =\n  %s\nwant\n  %s", got, tc.want)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(fmt.Errorf("wrapped: %w", &Error{StatusCode: 404})) {
		t.Error("a wrapped 404 must count")
	}
	if IsNotFound(&Error{StatusCode: 409}) || IsNotFound(errors.New("404")) || IsNotFound(nil) {
		t.Error("only an *apim.Error with 404 counts")
	}
}

// TestDependentWritesMarkA404 pins which calls report a 404 as "something this write
// hangs off is not in APIM yet" (ErrDependencyNotFound), which the controllers retry,
// and which report it as a plain 404, which they give up on. The marked ones write under
// a resource another custom resource creates: the product or tag an API is assigned to,
// the API (or operation) a policy is set on.
func TestDependentWritesMarkA404(t *testing.T) {
	dependent := map[string]bool{
		"AssignProductsToAPI": true,
		"AssignTagsToAPI":     true,
		"UpsertInboundPolicy": true,
	}
	operationPolicy := hcCall{"UpsertInboundPolicy (operation)", http.MethodPut, true, false, func(ctx context.Context) error {
		cfg := policyConfig()
		cfg.OperationID = "get-order"
		return UpsertInboundPolicy(ctx, cfg)
	}}
	dependent[operationPolicy.name] = true

	for _, call := range append(hcCalls(), operationPolicy) {
		if call.name == "GetAPI" || call.name == "DeleteProduct" {
			continue // a 404 is an answer for these, not an error
		}
		t.Run(call.name, func(t *testing.T) {
			newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if r.Method == http.MethodGet && call.method != http.MethodGet {
					// The etag lookup before an API upsert: the API is new.
					hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound"}}`)
					return
				}
				hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound","message":"Api not found."}}`)
			})
			err := call.run(context.Background())

			var apimErr *Error
			if !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusNotFound {
				t.Fatalf("%s() = %v, want an *apim.Error with status 404", call.name, err)
			}
			if !IsNotFound(err) {
				t.Errorf("IsNotFound(%v) = false, want true either way", err)
			}
			if apimErr.Code != "ResourceNotFound" {
				t.Errorf("Code = %q, want ResourceNotFound either way", apimErr.Code)
			}
			if got := errors.Is(err, ErrDependencyNotFound); got != dependent[call.name] {
				t.Errorf("errors.Is(err, ErrDependencyNotFound) = %v, want %v", got, dependent[call.name])
			}
		})
	}
}

// TestDependencyNotFoundOnlyOn404: other failures of a dependent write keep their own
// meaning; only the 404 says the parent is missing.
func TestDependencyNotFoundOnlyOn404(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusGone, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				hcRespond(w, status, nil, `{"error":{"code":"Whatever"}}`)
			})
			err := AssignProductsToAPI(context.Background(), deploymentConfig())
			if errors.Is(err, ErrDependencyNotFound) {
				t.Errorf("a %d was marked ErrDependencyNotFound: %v", status, err)
			}
		})
	}
}

// TestAPIWriteSurvivesTheCallersContext: once the PUT of an API is sent it comes back with
// APIM's answer, even when the reconcile's context is cancelled or runs out meanwhile. Cut
// off, it would leave APIM running a write nobody recorded, and the next attempt would write
// on top of it.
func TestAPIWriteSurvivesTheCallersContext(t *testing.T) {
	stops := map[string]func() (context.Context, context.CancelFunc, func()){
		"cancelled": func() (context.Context, context.CancelFunc, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel, cancel
		},
		"deadline": func() (context.Context, context.CancelFunc, func()) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			return ctx, cancel, func() { <-ctx.Done() }
		},
	}
	for _, upsert := range hcAsyncUpserts {
		for stopName, newContext := range stops {
			t.Run(upsert.name+"/"+stopName, func(t *testing.T) {
				putSeen := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
				var cutOff atomic.Bool
				newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
					if r.Method == http.MethodGet {
						hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound"}}`)
						return
					}
					close(putSeen)
					select {
					case <-release:
					case <-time.After(10 * time.Second):
					}
					// The client hanging up shows here as a cancelled request context.
					cutOff.Store(r.Context().Err() != nil)
					hcRespond(w, http.StatusAccepted, http.Header{"Azure-Asyncoperation": {"/operations/survivor"}}, ``)
				})

				ctx, cancel, stop := newContext()
				defer cancel()
				type outcome struct {
					result WriteResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := upsert.run(ctx)
					done <- outcome{result, err}
				}()

				select {
				case <-putSeen:
				case o := <-done:
					t.Fatalf("%s returned %+v, %v before sending the PUT", upsert.name, o.result, o.err)
				case <-time.After(10 * time.Second):
					t.Fatal("the PUT never reached the fake ARM")
				}
				stop()
				// Time for a write bound to the caller's context to give up.
				select {
				case o := <-done:
					t.Fatalf("%s returned %+v, %v before APIM answered", upsert.name, o.result, o.err)
				case <-time.After(150 * time.Millisecond):
				}
				releaseOnce.Do(func() { close(release) })

				var o outcome
				select {
				case o = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("the write never returned after APIM answered")
				}
				if o.err != nil {
					t.Fatalf("%s = %v, want APIM's answer", upsert.name, o.err)
				}
				if want := hcUnroutableHost + "/operations/survivor"; o.result.OperationURL != want {
					t.Errorf("OperationURL = %q, want %q", o.result.OperationURL, want)
				}
				if cutOff.Load() {
					t.Error("the PUT was cut off when the caller's context ended")
				}
				if ctx.Err() == nil {
					t.Error("the caller's context is still live; the test did not stop it")
				}
			})
		}
	}
}

// resetConnection hijacks the connection of a request and closes it with a TCP reset, as a
// load balancer dropping the connection does.
func resetConnection(t *testing.T, w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("fake ARM cannot hijack")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// TestAPIWriteOutcomeUnknown: a write of an API that got no answer saying how it ended (the
// connection failed or timed out after the PUT went out, or a gateway in front of ARM
// answered 502 or 504) is marked ErrWriteOutcomeUnknown, so the controller does not take it
// for a write APIM refused. A 502 or 504 is still the *Error with its status. Any other
// answer is APIM's and is returned unmarked.
func TestAPIWriteOutcomeUnknown(t *testing.T) {
	cases := []struct {
		name string
		// answer answers the PUT.
		answer func(t *testing.T, w http.ResponseWriter, r *http.Request)
		// clientTimeout, when set, is the timeout of the client the package uses.
		clientTimeout time.Duration
		unknown       bool
		status        int // the *Error's status; 0 for a transport failure
	}{
		{name: "connection reset", unknown: true, answer: func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
			resetConnection(t, w)
		}},
		{name: "connection closed without an answer", unknown: true, answer: func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("fake ARM cannot hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}},
		{name: "timeout", unknown: true, clientTimeout: 100 * time.Millisecond, answer: func(_ *testing.T, w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			hcRespond(w, http.StatusOK, nil, `{}`)
		}},
		{name: "502", unknown: true, status: http.StatusBadGateway},
		{name: "504", unknown: true, status: http.StatusGatewayTimeout},
		{name: "400", status: http.StatusBadRequest},
		{name: "409", status: http.StatusConflict},
		{name: "412", status: http.StatusPreconditionFailed},
		{name: "429", status: http.StatusTooManyRequests},
		{name: "500", status: http.StatusInternalServerError},
		{name: "503", status: http.StatusServiceUnavailable},
	}
	for _, upsert := range hcAsyncUpserts {
		for _, tc := range cases {
			t.Run(upsert.name+"/"+tc.name, func(t *testing.T) {
				fake := newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
					switch {
					case r.Method == http.MethodGet:
						hcRespond(w, http.StatusNotFound, nil, `{"error":{"code":"ResourceNotFound"}}`)
					case tc.answer != nil:
						tc.answer(t, w, r)
					default:
						hcRespond(w, tc.status, nil, fmt.Sprintf(`{"error":{"code":"Code%d","message":"m"}}`, tc.status))
					}
				})
				if tc.clientTimeout != 0 {
					t.Cleanup(UseEndpoint(hcUnroutableHost, &http.Client{Transport: fake.router, Timeout: tc.clientTimeout}))
				}

				result, err := upsert.run(context.Background())
				if err == nil {
					t.Fatalf("%s = %+v, nil; want an error", upsert.name, result)
				}
				if result != (WriteResult{}) {
					t.Errorf("WriteResult = %+v alongside the error, want empty", result)
				}
				if puts := len(fake.requests()) - 1; puts != 1 {
					t.Errorf("sent %d PUTs, want 1", puts)
				}
				if got := errors.Is(err, ErrWriteOutcomeUnknown); got != tc.unknown {
					t.Errorf("errors.Is(%v, ErrWriteOutcomeUnknown) = %v, want %v", err, got, tc.unknown)
				}
				if !strings.Contains(err.Error(), upsert.operation) {
					t.Errorf("Error() = %q, want it to name %q", err.Error(), upsert.operation)
				}
				var apimErr *Error
				if tc.status == 0 {
					if errors.As(err, &apimErr) {
						t.Errorf("a transport failure came back as *apim.Error: %v", err)
					}
					if tc.clientTimeout != 0 {
						var netErr net.Error
						if !errors.As(err, &netErr) || !netErr.Timeout() {
							t.Errorf("err = %v, want the timeout itself to stay visible", err)
						}
					}
					return
				}
				if !errors.As(err, &apimErr) {
					t.Fatalf("err = %v (%T), want an *apim.Error", err, err)
				}
				if apimErr.StatusCode != tc.status || apimErr.Code != fmt.Sprintf("Code%d", tc.status) ||
					apimErr.Method != http.MethodPut || apimErr.Operation != upsert.operation {
					t.Errorf("*Error = %+v, want the PUT's %d", apimErr, tc.status)
				}
			})
		}
	}
}

// TestNoWriteNoUnknownOutcome: ErrWriteOutcomeUnknown is for a write that went out. A failed
// existence check stops the upsert before its PUT, and the other writes are not API writes.
func TestNoWriteNoUnknownOutcome(t *testing.T) {
	newHCFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet {
			resetConnection(t, w)
			return
		}
		hcRespond(w, http.StatusBadGateway, nil, ``)
	})
	for _, upsert := range hcAsyncUpserts {
		if _, err := upsert.run(context.Background()); err == nil || errors.Is(err, ErrWriteOutcomeUnknown) {
			t.Errorf("%s with a failed existence check = %v, want an error without ErrWriteOutcomeUnknown", upsert.name, err)
		}
	}
	if err := UpsertTag(context.Background(), tagConfig()); err == nil || errors.Is(err, ErrWriteOutcomeUnknown) {
		t.Errorf("UpsertTag with a 502 = %v, want an *apim.Error without ErrWriteOutcomeUnknown", err)
	}
}

// TestResponseBodyIsBounded: a response body is read up to maxResponseBody; a larger one is
// an error, and the read stops there instead of taking in whatever the endpoint sends.
func TestResponseBodyIsBounded(t *testing.T) {
	const endless = 64 << 20 // what the fake offers when asked for "more than the limit"
	cases := []struct {
		name    string
		status  int
		size    int
		wantErr bool
	}{
		{"at the limit", http.StatusOK, maxResponseBody, false},
		{"one byte over", http.StatusOK, maxResponseBody + 1, true},
		{"far over", http.StatusOK, endless, true},
		{"far over on an error status", http.StatusInternalServerError, endless, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var written atomic.Int64
			newHCFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				chunk := bytes.Repeat([]byte(" "), 32<<10)
				for left := tc.size; left > 0; {
					n := min(left, len(chunk))
					if _, err := w.Write(chunk[:n]); err != nil {
						return // the client hung up
					}
					written.Add(int64(n))
					left -= n
				}
			})
			err := UpsertTag(context.Background(), tagConfig())
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("UpsertTag() with a %d byte body = %v, want nil", tc.size, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("UpsertTag() with a %d byte body = nil, want an error", tc.size)
			}
			if !strings.Contains(err.Error(), "upsert tag t1: read response body") ||
				!strings.Contains(err.Error(), fmt.Sprint(maxResponseBody)) {
				t.Errorf("Error() = %q, want it to name the operation and the limit", err.Error())
			}
			var apimErr *Error
			if errors.As(err, &apimErr) {
				t.Errorf("an oversized body came back as *apim.Error: %v", err)
			}
			if tc.size == endless {
				if got := written.Load(); got >= endless/2 {
					t.Errorf("the fake wrote %d bytes before the client hung up; the read is not bounded", got)
				}
			}
		})
	}
}

// TestImportEnvelopeKeepsTheDocument: the document travels in the envelope as is. <, > and &
// are not escaped (json.Marshal would grow a document full of them by half), the body is
// valid JSON without a trailing newline, and properties.value decodes back to the document
// byte for byte, whatever it holds.
func TestImportEnvelopeKeepsTheDocument(t *testing.T) {
	cfg := deploymentConfig()
	cfg.ServiceURL = "https://orders.internal/api?a=1&b=<2>"
	docs := map[string]string{
		"html in json": `{"openapi":"3.0.0","info":{"title":"A & B","description":"<b>bold</b> -> x < y && y > z"},"paths":{}}`,
		"html in yaml": "openapi: 3.0.0\ninfo:\n  title: A & B\n  description: <b>bold</b> -> x < y && y > z\n",
		"escapes in yaml": "openapi: 3.0.0\ninfo:\n  title: \"quote \\\" backslash \\\\ tab\\t\"\n  " +
			"description: 'line\u2028separator, é, \U0001F600, \x7f'\n",
		"swagger json": `{"swagger":"2.0","info":{"description":"<script>alert('&')</script>"}}`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			body, format, err := importEnvelope(cfg, []byte(doc))
			if err != nil {
				t.Fatalf("importEnvelope() error = %v", err)
			}
			if !json.Valid(body) {
				t.Fatalf("envelope is not valid JSON: %s", body)
			}
			if bytes.HasSuffix(body, []byte("\n")) {
				t.Error("envelope ends in a newline")
			}
			for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
				if bytes.Contains(body, []byte(escape)) {
					t.Errorf("envelope escapes HTML characters as %s: %s", escape, body)
				}
			}
			var decoded importBody
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if decoded.Properties.Value != doc {
				t.Errorf("properties.value = %q, want the document %q", decoded.Properties.Value, doc)
			}
			if decoded.Properties.Format != format || decoded.Properties.ServiceURL != cfg.ServiceURL ||
				decoded.Properties.Path != cfg.RoutePrefix {
				t.Errorf("properties = %+v, want format %s and the config's path and serviceUrl", decoded.Properties, format)
			}
			if !bytes.Contains(body, []byte(cfg.ServiceURL)) {
				t.Errorf("serviceUrl is escaped in %s", body)
			}
		})
	}

	// A document of nothing but HTML characters costs no more than the document itself.
	doc := `{"openapi":"3.0.0","info":{"description":"` + strings.Repeat("<&>", 10000) + `"}}`
	body, _, err := importEnvelope(cfg, []byte(doc))
	if err != nil {
		t.Fatalf("importEnvelope() error = %v", err)
	}
	// Room for the other properties and the escaped quotes of the document; with \u003c
	// escapes the body would be 30000 bytes larger.
	if limit := len(doc) + 512; len(body) > limit {
		t.Errorf("envelope is %d bytes for a %d byte document, want at most %d", len(body), len(doc), limit)
	}
}

// TestImportSendsTheEnvelopeUnescaped: what reaches ARM is the envelope itself.
func TestImportSendsTheEnvelopeUnescaped(t *testing.T) {
	_, puts := importFakeARM(t, http.StatusOK)
	cfg := deploymentConfig()
	doc := `{"openapi":"3.0.0","info":{"description":"<b>a & b</b>"}}`
	if _, err := ImportOpenAPIDefinitionToAPIM(context.Background(), cfg, []byte(doc)); err != nil {
		t.Fatalf("import = %v", err)
	}
	want, _, err := importEnvelope(cfg, []byte(doc))
	if err != nil {
		t.Fatalf("importEnvelope() error = %v", err)
	}
	if len(*puts) != 1 || (*puts)[0] != string(want) {
		t.Fatalf("PUT bodies = %q, want exactly %s", *puts, want)
	}
	if !strings.Contains((*puts)[0], `<b>a & b</b>`) {
		t.Errorf("PUT body %s escapes the document", (*puts)[0])
	}
}

// bigSwaggerYAML builds a block-style Swagger 2.0 YAML document of at least size bytes.
func bigSwaggerYAML(size int, version string) []byte {
	var b bytes.Buffer
	b.Grow(size + 256)
	b.WriteString(version + "\ninfo:\n  title: big\n  version: '1'\npaths:\n")
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "  /items/%d:\n    get:\n      responses:\n        '200':\n          description: ok %d\n", i, i)
	}
	return b.Bytes()
}

// TestImportRefusesLargeSwaggerYAML: converting Swagger 2.0 YAML to JSON builds the whole
// document in memory at many times its size, so one over maxSwaggerYAMLConversion is
// refused with ErrUnsupportedDocument, before any request. The fetcher still accepts it as a
// document; only its conversion is refused. OpenAPI 3 YAML and JSON of that size are sent
// as they are.
func TestImportRefusesLargeSwaggerYAML(t *testing.T) {
	doc := bigSwaggerYAML(maxSwaggerYAMLConversion+1, "swagger: '2.0'")
	if len(doc) <= maxSwaggerYAMLConversion {
		t.Fatalf("document is %d bytes, want over %d", len(doc), maxSwaggerYAMLConversion)
	}
	if err := ValidateOpenAPIDocument(doc); err != nil {
		t.Errorf("ValidateOpenAPIDocument() = %v, want nil: the document itself is fine", err)
	}

	fake, _ := importFakeARM(t, http.StatusOK)
	result, err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), doc)
	if !errors.Is(err, ErrUnsupportedDocument) {
		t.Fatalf("import = %+v, %v; want ErrUnsupportedDocument", result, err)
	}
	if errors.Is(err, ErrNotOpenAPIDocument) {
		t.Errorf("err = %v, want it not to claim the document is not OpenAPI", err)
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("Error() = %q, want it to say to serve the document as JSON", err.Error())
	}
	if n := fake.count(); n != 0 {
		t.Errorf("ARM saw %d requests, want 0", n)
	}
	if result.Accepted() {
		t.Errorf("WriteResult = %+v, want empty", result)
	}

	// Same size, OpenAPI 3: sent as is, nothing converted.
	openAPI := bigSwaggerYAML(maxSwaggerYAMLConversion+1, "openapi: 3.0.0")
	if format, out, err := importFormat(openAPI); err != nil || format != "openapi" || len(out) != len(openAPI) {
		t.Errorf("importFormat(large OpenAPI 3 YAML) = %q, %d bytes, %v; want openapi, unchanged", format, len(out), err)
	}
	// Swagger 2.0 as JSON is never converted, whatever its size.
	swaggerJSON := append([]byte(`{"swagger":"2.0","x":"`), bytes.Repeat([]byte("a"), maxSwaggerYAMLConversion)...)
	swaggerJSON = append(swaggerJSON, `"}`...)
	if format, _, err := importFormat(swaggerJSON); err != nil || format != "swagger-json" {
		t.Errorf("importFormat(large Swagger JSON) = %q, %v; want swagger-json", format, err)
	}
}
