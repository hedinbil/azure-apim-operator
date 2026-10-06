package apim

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// hcUnroutableHost is what armHost points at while a fake ARM runs. ".invalid" never
// resolves (RFC 2606), so only a request sent through httpClient, whose transport
// re-addresses it to the fake, gets anywhere. A call that bypassed httpClient (e.g.
// http.DefaultClient.Do, as several functions did before 0.31.0) fails on DNS instead of
// quietly reaching the fake through the same plain-HTTP address, and could not reach the
// real management.azure.com either.
const hcUnroutableHost = "http://arm.invalid"

// hcRouter is the transport of the client UseEndpoint installs for a fake ARM: it sends
// every request to the fake's address and counts it.
type hcRouter struct {
	target *url.URL
	base   http.RoundTripper
	sent   atomic.Int64
}

func (r *hcRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	r.sent.Add(1)
	out := req.Clone(req.Context())
	out.URL.Scheme = r.target.Scheme
	out.URL.Host = r.target.Host
	out.Host = ""
	return r.base.RoundTrip(out)
}

// routeToFake points the package at hcUnroutableHost through a client that delivers
// every request to server, for the rest of the test, and returns the router so a test
// can check that every request the fake saw went through httpClient.
func routeToFake(t *testing.T, server *httptest.Server) *hcRouter {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse fake ARM URL: %v", err)
	}
	router := &hcRouter{target: target, base: server.Client().Transport}
	t.Cleanup(UseEndpoint(hcUnroutableHost, &http.Client{Transport: router}))
	return router
}

// hcServicePath is the ARM path of the APIM instance every test config points at.
const hcServicePath = "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.ApiManagement/service/apim-1"

// hcRecorded is one request the fake ARM saw, with its body read before the handler
// returned (httptest closes it afterwards, so a cloned *http.Request is not enough).
type hcRecorded struct {
	method      string
	escapedPath string
	query       map[string][]string
	header      http.Header
	body        []byte
}

// hcFake is an httptest server standing in for management.azure.com that records
// every request, body included.
type hcFake struct {
	t      *testing.T
	mu     sync.Mutex
	seen   []hcRecorded
	server *httptest.Server
	router *hcRouter
}

// newHCFake starts the fake and points the package at it for the rest of the test.
func newHCFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) *hcFake {
	t.Helper()
	f := &hcFake{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake ARM: read request body: %v", err)
		}
		f.mu.Lock()
		f.seen = append(f.seen, hcRecorded{
			method:      r.Method,
			escapedPath: r.URL.EscapedPath(),
			query:       r.URL.Query(),
			header:      r.Header.Clone(),
			body:        body,
		})
		f.mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(f.server.Close)
	f.router = routeToFake(t, f.server)
	return f
}

func (f *hcFake) requests() []hcRecorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hcRecorded(nil), f.seen...)
}

// countPath counts requests whose escaped path starts with prefix.
func (f *hcFake) countPath(prefix string) int {
	n := 0
	for _, r := range f.requests() {
		if strings.HasPrefix(r.escapedPath, prefix) {
			n++
		}
	}
	return n
}

// hcRespond writes status, optional headers and body. Bodies on 204/304 are dropped,
// as net/http would refuse them anyway.
func hcRespond(w http.ResponseWriter, status int, headers http.Header, body string) {
	for k, v := range headers {
		w.Header()[k] = v
	}
	if body != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if body != "" && status != http.StatusNoContent && status != http.StatusNotModified {
		_, _ = io.WriteString(w, body)
	}
}

// hcDecode unmarshals a JSON request body, failing the test when it is not JSON.
func hcDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("request body %q is not JSON: %v", body, err)
	}
	return out
}

// hcProps returns body["properties"] as a map.
func hcProps(t *testing.T, body []byte) map[string]any {
	t.Helper()
	props, ok := hcDecode(t, body)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("request body %s has no properties object", body)
	}
	return props
}

// hcCall is one exported ARM call of the package, run with the shared test configs.
type hcCall struct {
	name string
	// method is the HTTP method of the request whose failure the call reports.
	method string
	// write is false for the two reads (GetAPI, GetAPIMServiceDetails).
	write bool
	// polls is true for the API upserts, which wait for a 202.
	polls bool
	run   func(ctx context.Context) error
}

// hcCalls lists every ARM call the package makes, reads included.
func hcCalls() []hcCall {
	return []hcCall{
		{"ImportOpenAPIDefinitionToAPIM", http.MethodPut, true, true, func(ctx context.Context) error {
			return ImportOpenAPIDefinitionToAPIM(ctx, deploymentConfig(), []byte(`{"openapi":"3.0.0"}`))
		}},
		{"UpsertWebSocketAPI", http.MethodPut, true, true, func(ctx context.Context) error {
			return UpsertWebSocketAPI(ctx, deploymentConfig())
		}},
		{"AssignServiceUrlToApi", http.MethodPatch, true, false, func(ctx context.Context) error {
			return AssignServiceUrlToApi(ctx, deploymentConfig())
		}},
		{"SetSubscriptionRequired", http.MethodPatch, true, false, func(ctx context.Context) error {
			return SetSubscriptionRequired(ctx, deploymentConfig())
		}},
		{"AssignProductsToAPI", http.MethodPut, true, false, func(ctx context.Context) error {
			return AssignProductsToAPI(ctx, deploymentConfig())
		}},
		{"AssignTagsToAPI", http.MethodPut, true, false, func(ctx context.Context) error {
			return AssignTagsToAPI(ctx, deploymentConfig())
		}},
		{"UpsertProduct", http.MethodPut, true, false, func(ctx context.Context) error {
			cfg := productConfig()
			cfg.DisplayName = "Product one"
			return UpsertProduct(ctx, cfg)
		}},
		{"DeleteProduct", http.MethodDelete, true, false, func(ctx context.Context) error {
			return DeleteProduct(ctx, productConfig())
		}},
		{"UpsertTag", http.MethodPut, true, false, func(ctx context.Context) error {
			return UpsertTag(ctx, tagConfig())
		}},
		{"UpsertInboundPolicy", http.MethodPut, true, false, func(ctx context.Context) error {
			return UpsertInboundPolicy(ctx, policyConfig())
		}},
		{"GetAPI", http.MethodGet, false, false, func(ctx context.Context) error {
			_, _, err := GetAPI(ctx, deploymentConfig())
			return err
		}},
		{"GetAPIMServiceDetails", http.MethodGet, false, false, func(ctx context.Context) error {
			_, _, err := GetAPIMServiceDetails(ctx, deploymentConfig())
			return err
		}},
	}
}
