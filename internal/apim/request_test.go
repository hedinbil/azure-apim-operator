package apim

import (
	"context"
	"errors"
	"fmt"
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

// withAsyncTiming shortens the async wait for one test.
func withAsyncTiming(t *testing.T, timeout, interval time.Duration) {
	t.Helper()
	previousTimeout, previousInterval := AsyncWaitTimeout, AsyncPollInterval
	AsyncWaitTimeout, AsyncPollInterval = timeout, interval
	t.Cleanup(func() { AsyncWaitTimeout, AsyncPollInterval = previousTimeout, previousInterval })
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
		{"ImportOpenAPIDefinitionToAPIM", func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), []byte(`{"openapi":"3.0.0"}`))
		}, 2}, // GET for the etag, then the PUT
		{"UpsertWebSocketAPI", func(ctx context.Context) error { return UpsertWebSocketAPI(ctx, deploymentConfig()) }, 2},
		{"AssignServiceUrlToApi", func(ctx context.Context) error { return AssignServiceUrlToApi(ctx, deploymentConfig()) }, 1},
		{"SetSubscriptionRequired", func(ctx context.Context) error { return SetSubscriptionRequired(ctx, deploymentConfig()) }, 1},
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
		"AssignServiceUrlToApi":   func(ctx context.Context) error { return AssignServiceUrlToApi(ctx, deploymentConfig()) },
		"SetSubscriptionRequired": func(ctx context.Context) error { return SetSubscriptionRequired(ctx, deploymentConfig()) },
		"AssignProductsToAPI":     func(ctx context.Context) error { return AssignProductsToAPI(ctx, deploymentConfig()) },
		"AssignTagsToAPI":         func(ctx context.Context) error { return AssignTagsToAPI(ctx, deploymentConfig()) },
		"UpsertProduct":           func(ctx context.Context) error { return UpsertProduct(ctx, productConfig()) },
		"DeleteProduct":           func(ctx context.Context) error { return DeleteProduct(ctx, productConfig()) },
		"UpsertTag":               func(ctx context.Context) error { return UpsertTag(ctx, tagConfig()) },
		"UpsertInboundPolicy":     func(ctx context.Context) error { return UpsertInboundPolicy(ctx, policyConfig()) },
		"ImportOpenAPIDefinitionToAPIM": func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), []byte(`{}`))
		},
		"UpsertWebSocketAPI": func(ctx context.Context) error { return UpsertWebSocketAPI(ctx, deploymentConfig()) },
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

// TestImportRequestShape pins the import PUT: query parameters, the literal ";rev="
// separator, content type and the etag from the GET.
func TestImportRequestShape(t *testing.T) {
	fake := newFakeARM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"e1"`)
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
	cfg := deploymentConfig()
	if err := ImportOpenAPIDefinitionToAPIM(context.Background(), cfg, []byte(`{"openapi":"3.0.0"}`)); err != nil {
		t.Fatalf("import = %v", err)
	}
	put := fake.request(1)
	if put.Method != http.MethodPut {
		t.Fatalf("second request = %s, want PUT", put.Method)
	}
	q := put.URL.Query()
	if q.Get("import") != "true" || q.Get("path") != "/orders" || q.Has("createRevision") {
		t.Errorf("query = %v, want import=true, path=/orders, no createRevision", q)
	}
	if got := put.Header.Get("If-Match"); got != `"e1"` {
		t.Errorf("If-Match = %q, want the etag from GET", got)
	}
	if got := put.Header.Get("Content-Type"); got != "application/vnd.oai.openapi+json" {
		t.Errorf("Content-Type = %q", got)
	}

	// A revision skips the GET and addresses "orders;rev=3" with a literal separator.
	fake = newFakeARM(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, `{}`) })
	cfg.Revision = "3"
	if err := ImportOpenAPIDefinitionToAPIM(context.Background(), cfg, []byte(`{}`)); err != nil {
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

// asyncARM answers the import PUT with 202 and an operation URL, then the operation
// polls with whatever poll returns.
func asyncARM(t *testing.T, accepted http.Header, poll func(n int, w http.ResponseWriter)) *atomic.Int32 {
	t.Helper()
	var polls atomic.Int32
	newFakeARM(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/operations/"):
			poll(int(polls.Add(1)), w)
		case r.Method == http.MethodGet:
			writeJSON(w, http.StatusNotFound, `{"error":{"code":"ResourceNotFound"}}`)
		default:
			for k, v := range accepted {
				w.Header()[k] = v
			}
			writeJSON(w, http.StatusAccepted, ``)
		}
	})
	return &polls
}

func TestAsyncImportSucceeds(t *testing.T) {
	withAsyncTiming(t, 5*time.Second, 5*time.Millisecond)
	// A relative operation URL is resolved against the ARM host.
	polls := asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/op1"}}, func(n int, w http.ResponseWriter) {
		if n < 3 {
			writeJSON(w, http.StatusOK, `{"status":"InProgress"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
	})
	if err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`)); err != nil {
		t.Fatalf("import = %v, want nil", err)
	}
	if got := polls.Load(); got != 3 {
		t.Errorf("polled %d times, want 3", got)
	}
}

func TestAsyncImportViaLocationHeader(t *testing.T) {
	withAsyncTiming(t, 5*time.Second, 5*time.Millisecond)
	var location atomic.Value // the fake's own absolute URL, known only once it runs
	var polls atomic.Int32
	fake := newFakeARM(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/operations/"):
			if polls.Add(1) < 2 {
				writeJSON(w, http.StatusAccepted, ``)
				return
			}
			// A terminal status without a status field counts as done.
			writeJSON(w, http.StatusOK, `{}`)
		case r.Method == http.MethodGet:
			writeJSON(w, http.StatusNotFound, `{"error":{"code":"ResourceNotFound"}}`)
		default:
			w.Header().Set("Location", location.Load().(string))
			writeJSON(w, http.StatusAccepted, ``)
		}
	})
	location.Store(fake.server.URL + "/operations/op2")

	if err := UpsertWebSocketAPI(context.Background(), deploymentConfig()); err != nil {
		t.Fatalf("websocket upsert = %v, want nil", err)
	}
	if got := polls.Load(); got != 2 {
		t.Errorf("polled %d times, want 2", got)
	}
}

func TestAsyncImportFailedCarriesTheAzureCode(t *testing.T) {
	withAsyncTiming(t, 5*time.Second, 5*time.Millisecond)
	asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/op3"}}, func(_ int, w http.ResponseWriter) {
		writeJSON(w, http.StatusOK,
			`{"status":"Failed","error":{"code":"InternalServerError","message":"DeadOperationMonitor","details":[{"code":"DeadOperationMonitor"}]}}`)
	})
	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	var apimErr *Error
	if !errors.As(err, &apimErr) {
		t.Fatalf("import = %v (%T), want *apim.Error", err, err)
	}
	if !errors.Is(err, ErrAsyncOperationFailed) {
		t.Errorf("errors.Is(err, ErrAsyncOperationFailed) = false for %v", err)
	}
	if apimErr.Code != "InternalServerError" || apimErr.DetailCode != "DeadOperationMonitor" {
		t.Errorf("codes = %q/%q, want InternalServerError/DeadOperationMonitor", apimErr.Code, apimErr.DetailCode)
	}
	if apimErr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 for an async result", apimErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "DeadOperationMonitor") {
		t.Errorf("Error() = %q, want the Azure message", err.Error())
	}
}

func TestAsyncImportCanceled(t *testing.T) {
	withAsyncTiming(t, 5*time.Second, 5*time.Millisecond)
	asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/op4"}}, func(_ int, w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, `{"status":"Canceled"}`)
	})
	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, ErrAsyncOperationFailed) {
		t.Fatalf("import = %v, want ErrAsyncOperationFailed", err)
	}
}

func TestAsyncPollHTTPErrorIsTyped(t *testing.T) {
	withAsyncTiming(t, 5*time.Second, 5*time.Millisecond)
	asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/op5"}}, func(_ int, w http.ResponseWriter) {
		writeJSON(w, http.StatusInternalServerError, `{"error":{"code":"InternalServerError","message":"boom"}}`)
	})
	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	var apimErr *Error
	if !errors.As(err, &apimErr) || apimErr.StatusCode != http.StatusInternalServerError || apimErr.Method != http.MethodGet {
		t.Fatalf("import = %v, want *apim.Error 500 from the GET poll", err)
	}
}

func TestAsyncImportTimesOut(t *testing.T) {
	withAsyncTiming(t, 80*time.Millisecond, 10*time.Millisecond)
	polls := asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/op6"}}, func(_ int, w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, `{"status":"InProgress"}`)
	})
	start := time.Now()
	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, ErrImportWaitTimeout) {
		t.Fatalf("import = %v, want ErrImportWaitTimeout", err)
	}
	var apimErr *Error
	if !errors.As(err, &apimErr) || apimErr.Operation != "import API" {
		t.Errorf("timeout = %v, want *apim.Error for the import", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("gave up after %s, want about the 80ms wait", elapsed)
	}
	if polls.Load() < 2 {
		t.Errorf("polled %d times, want several before giving up", polls.Load())
	}
}

// TestRetryAfterIsHonouredAndBounded: a Retry-After far beyond the remaining wait must
// not hold the reconcile past AsyncWaitTimeout; the operation is polled once at the end.
func TestRetryAfterIsHonouredAndBounded(t *testing.T) {
	withAsyncTiming(t, 150*time.Millisecond, time.Millisecond)
	polls := asyncARM(t, http.Header{
		"Azure-Asyncoperation": {"/operations/op7"},
		"Retry-After":          {"3600"},
	}, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "3600")
		writeJSON(w, http.StatusOK, `{"status":"InProgress"}`)
	})
	start := time.Now()
	err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, ErrImportWaitTimeout) {
		t.Fatalf("import = %v, want ErrImportWaitTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %s; Retry-After must be bounded by the remaining wait", elapsed)
	}
	// With a 1 ms poll interval and no Retry-After this would have polled ~150 times.
	if got := polls.Load(); got != 1 {
		t.Errorf("polled %d times, want exactly 1 (Retry-After replaces the interval)", got)
	}
}

// TestRetryAfterShortensTheInterval: a Retry-After of one second beats an interval of
// an hour.
func TestRetryAfterShortensTheInterval(t *testing.T) {
	withAsyncTiming(t, 10*time.Second, time.Hour)
	asyncARM(t, http.Header{
		"Azure-Asyncoperation": {"/operations/op8"},
		"Retry-After":          {"1"},
	}, func(_ int, w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, `{"status":"Succeeded"}`)
	})
	start := time.Now()
	if err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`)); err != nil {
		t.Fatalf("import = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("finished after %s, want about the 1s Retry-After", elapsed)
	}
}

func TestAsyncWaitStopsWithTheContext(t *testing.T) {
	withAsyncTiming(t, time.Minute, time.Minute)
	asyncARM(t, http.Header{"Azure-Asyncoperation": {"/operations/op9"}}, func(_ int, w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, `{"status":"InProgress"}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), []byte(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("import = %v, want context.DeadlineExceeded", err)
	}
}

func TestAsync202WithoutPollURLIsAccepted(t *testing.T) {
	asyncARM(t, nil, func(_ int, w http.ResponseWriter) {})
	if err := ImportOpenAPIDefinitionToAPIM(context.Background(), deploymentConfig(), []byte(`{}`)); err != nil {
		t.Fatalf("import = %v, want nil (nothing to poll)", err)
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
// the API (or operation) a policy is set on, the imported API the patches change.
func TestDependentWritesMarkA404(t *testing.T) {
	dependent := map[string]bool{
		"AssignServiceUrlToApi":   true,
		"SetSubscriptionRequired": true,
		"AssignProductsToAPI":     true,
		"AssignTagsToAPI":         true,
		"UpsertInboundPolicy":     true,
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
