package apim

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const prefix = "https://management.azure.com/subscriptions/sub-1/resourceGroups/rg-1" +
	"/providers/Microsoft.ApiManagement/service/apim-1"

func testConfig() APIMDeploymentConfig {
	return APIMDeploymentConfig{SubscriptionID: "sub-1", ResourceGroup: "rg-1", ServiceName: "apim-1"}
}

func TestServiceURLBuildsTheExpectedPath(t *testing.T) {
	cases := []struct {
		name     string
		segments []string
		want     string
	}{
		{"service itself", nil, prefix + "?api-version=" + apiVersion},
		{"one api", []string{"apis", "orders"}, prefix + "/apis/orders?api-version=" + apiVersion},
		{"a product assignment", []string{"products", "p1", "apis", "orders"},
			prefix + "/products/p1/apis/orders?api-version=" + apiVersion},
		{"an operation policy", []string{"apis", "orders", "operations", "getOrder", "policies", "policy"},
			prefix + "/apis/orders/operations/getOrder/policies/policy?api-version=" + apiVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceURL(testConfig(), tc.segments...); got != tc.want {
				t.Errorf("serviceURL() =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// TestServiceURLEscapesInjectedSegments is the APIM-16 regression. Ids come
// straight from a custom resource, and the URL used to be assembled with
// fmt.Sprintf: a value containing "/", "?", "#" or ".." changed which resource
// the request targeted, because http.NewRequestWithContext parses whatever
// string it is handed.
func TestServiceURLEscapesInjectedSegments(t *testing.T) {
	cases := []struct {
		name, apiID, wantSegment string
	}{
		{"path traversal", "../../../subscriptions/other", "..%2F..%2F..%2Fsubscriptions%2Fother"},
		{"query injection", "orders?api-version=2018-01-01&x=", "orders%3Fapi-version=2018-01-01&x="},
		{"fragment", "orders#frag", "orders%23frag"},
		{"slash", "orders/policies/policy", "orders%2Fpolicies%2Fpolicy"},
		{"space", "my orders", "my%20orders"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := prefix + "/apis/" + tc.wantSegment + "?api-version=" + apiVersion
			got := serviceURL(testConfig(), "apis", tc.apiID)
			if got != want {
				t.Errorf("serviceURL() =\n  %s\nwant\n  %s", got, want)
			}
		})
	}
}

// TestServiceURLEscapesTheScope covers the scope fields, which come from an
// APIMService resource rather than being hard-coded.
func TestServiceURLEscapesTheScope(t *testing.T) {
	cfg := APIMDeploymentConfig{
		SubscriptionID: "sub/../evil",
		ResourceGroup:  "rg?x=1",
		ServiceName:    "svc#frag",
	}
	want := "https://management.azure.com/subscriptions/sub%2F..%2Fevil/resourceGroups/rg%3Fx=1" +
		"/providers/Microsoft.ApiManagement/service/svc%23frag?api-version=" + apiVersion
	if got := serviceURL(cfg); got != want {
		t.Errorf("serviceURL() =\n  %s\nwant\n  %s", got, want)
	}
}

// TestRevisionURLKeepsTheRevisionSeparator guards the one place that must NOT
// be fully escaped: APIM addresses a revision as "{apiId};rev={n}" and ARM
// needs that ";" literally, so escaping it to %3B would break every revision
// import.
func TestRevisionURLKeepsTheRevisionSeparator(t *testing.T) {
	got := revisionURL(testConfig(), "orders", "3")
	want := prefix + "/apis/orders;rev=3?api-version=" + apiVersion
	if got != want {
		t.Errorf("revisionURL() =\n  %s\nwant\n  %s", got, want)
	}
	// Both halves are still escaped.
	got = revisionURL(testConfig(), "ord/ers", "3/4")
	want = prefix + "/apis/ord%2Fers;rev=3%2F4?api-version=" + apiVersion
	if got != want {
		t.Errorf("revisionURL() with hostile input =\n  %s\nwant\n  %s", got, want)
	}
	// No revision means no separator at all.
	got = revisionURL(testConfig(), "orders", "")
	want = prefix + "/apis/orders?api-version=" + apiVersion
	if got != want {
		t.Errorf("revisionURL() without a revision =\n  %s\nwant\n  %s", got, want)
	}
}

// TestEveryConfigProvidesItsScope keeps the serviceScope implementations in
// step with the config types, so a new one cannot silently miss out on the
// escaping builder.
func TestEveryConfigProvidesItsScope(t *testing.T) {
	configs := []serviceScope{
		APIMDeploymentConfig{SubscriptionID: "s", ResourceGroup: "r", ServiceName: "n"},
		APIMProductConfig{SubscriptionID: "s", ResourceGroup: "r", ServiceName: "n"},
		APIMTagConfig{SubscriptionID: "s", ResourceGroup: "r", ServiceName: "n"},
		APIMInboundPolicyConfig{SubscriptionID: "s", ResourceGroup: "r", ServiceName: "n"},
	}
	for _, cfg := range configs {
		sub, rg, name := cfg.scope()
		if sub != "s" || rg != "r" || name != "n" {
			t.Errorf("%T.scope() = %q, %q, %q", cfg, sub, rg, name)
		}
	}
}

// TestWithQueryAppendsAfterAPIVersion pins the shape of a URL with an extra
// parameter: serviceURL always ends in ?api-version=, so the helper must join
// with "&" and escape both halves.
func TestWithQueryAppendsAfterAPIVersion(t *testing.T) {
	got := withQuery(serviceURL(testConfig(), "products", "p1"), "deleteSubscriptions", "true")
	want := prefix + "/products/p1?api-version=" + apiVersion + "&deleteSubscriptions=true"
	if got != want {
		t.Errorf("withQuery() =\n  %s\nwant\n  %s", got, want)
	}
	got = withQuery(prefix+"?api-version="+apiVersion, "a b", "c&d")
	want = prefix + "?api-version=" + apiVersion + "&a+b=c%26d"
	if got != want {
		t.Errorf("withQuery() with hostile input =\n  %s\nwant\n  %s", got, want)
	}
}

// TestARMClientHasTimeouts: without them a half-open connection to ARM blocks a reconcile
// worker for good. The transport is a clone, so the settings stay off
// http.DefaultTransport, which other clients in the process share.
func TestARMClientHasTimeouts(t *testing.T) {
	for name, client := range map[string]*http.Client{
		"newARMClient": newARMClient(),
		"httpClient":   httpClient,
	} {
		t.Run(name, func(t *testing.T) {
			if client.Timeout != 2*time.Minute {
				t.Errorf("Timeout = %s, want 2m", client.Timeout)
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
			}
			if transport.ResponseHeaderTimeout != 90*time.Second {
				t.Errorf("ResponseHeaderTimeout = %s, want 90s", transport.ResponseHeaderTimeout)
			}
			if transport == http.DefaultTransport {
				t.Error("the ARM client must not change http.DefaultTransport")
			}
			if transport.MaxIdleConnsPerHost != 16 {
				t.Errorf("MaxIdleConnsPerHost = %d, want 16: every worker talks to the same host", transport.MaxIdleConnsPerHost)
			}
			if client.CheckRedirect == nil {
				t.Fatal("CheckRedirect is nil: the client follows redirects")
			}
			if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
				t.Errorf("CheckRedirect() = %v, want http.ErrUseLastResponse", err)
			}
			// Still the default transport otherwise: proxy from the environment and TLS
			// handshake bounds.
			if transport.Proxy == nil || transport.TLSHandshakeTimeout == 0 {
				t.Errorf("transport lost the defaults: Proxy set = %v, TLSHandshakeTimeout = %s",
					transport.Proxy != nil, transport.TLSHandshakeTimeout)
			}
		})
	}
	if def := http.DefaultTransport.(*http.Transport); def.ResponseHeaderTimeout != 0 || def.MaxIdleConnsPerHost == 16 {
		t.Errorf("http.DefaultTransport changed: ResponseHeaderTimeout = %s, MaxIdleConnsPerHost = %d",
			def.ResponseHeaderTimeout, def.MaxIdleConnsPerHost)
	}
	if newARMClient().Transport == newARMClient().Transport {
		t.Error("each ARM client must get its own transport")
	}
}

// TestRedactURL: a backend URL can carry basic-auth credentials or a function key in its
// query; neither may reach a log line.
func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://orders.internal/api":                            "https://orders.internal/api",
		"https://user:secret@orders.internal/api":                "https://orders.internal/api",
		"https://user@orders.internal/api":                       "https://orders.internal/api",
		"https://fn.azurewebsites.net/api/x?code=secretkey":      "https://fn.azurewebsites.net/api/x?<redacted>",
		"https://u:p@fn.azurewebsites.net/api/x?code=k&a=b#frag": "https://fn.azurewebsites.net/api/x?<redacted>#frag",
		"wss://bidme.example/hub":                                "wss://bidme.example/hub",
		"":                                                       "",
		"http://[::1":                                            "<unparseable URL>",
	}
	for in, want := range cases {
		got := RedactURL(in)
		if got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
		for _, secret := range []string{"secret", "secretkey", "user:", "u:p@", "code="} {
			if in != "" && strings.Contains(got, secret) {
				t.Errorf("RedactURL(%q) = %q still carries %q", in, got, secret)
			}
		}
	}
}

// TestARMClientNeverFollowsARedirect runs the production client against a fake ARM that
// redirects every request. ARM does not redirect; a client that followed would send the
// request, and with it the bearer token, past the IsOperationURL check: Go forwards the
// Authorization header to the same host name on another port. The 3xx comes back as an
// *Error, and the redirect target, on the same host and another port, sees nothing.
func TestARMClientNeverFollowsARedirect(t *testing.T) {
	var targetHits atomic.Int32
	var targetAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		targetAuth.Store(r.Header.Get("Authorization"))
		writeJSON(w, http.StatusOK, `{"status":"Succeeded","properties":{}}`)
	}))
	t.Cleanup(target.Close)

	calls := map[string]func(ctx context.Context) error{
		"UpsertTag (PUT)": func(ctx context.Context) error { return UpsertTag(ctx, tagConfig()) },
		"GetAPIMServiceDetails (GET)": func(ctx context.Context) error {
			_, _, err := GetAPIMServiceDetails(ctx, deploymentConfig())
			return err
		},
		"GetOperationState (GET)": func(ctx context.Context) error {
			_, err := GetOperationState(ctx, "tok", armHost+"/operations/redirected")
			return err
		},
	}
	locations := map[string]func(r *http.Request) string{
		"another port":  func(r *http.Request) string { return target.URL + r.URL.RequestURI() },
		"relative path": func(*http.Request) string { return "/elsewhere" },
	}
	statuses := []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	}
	for callName, call := range calls {
		for locationName, location := range locations {
			for _, status := range statuses {
				t.Run(fmt.Sprintf("%s/%s/%d", callName, locationName, status), func(t *testing.T) {
					var seen atomic.Int32
					var elsewhere atomic.Int32
					var auth atomic.Value
					arm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/elsewhere" {
							elsewhere.Add(1)
							writeJSON(w, http.StatusOK, `{}`)
							return
						}
						seen.Add(1)
						auth.Store(r.Header.Get("Authorization"))
						w.Header().Set("Location", location(r))
						w.WriteHeader(status)
					}))
					t.Cleanup(arm.Close)
					client := newARMClient()
					t.Cleanup(client.CloseIdleConnections)
					t.Cleanup(UseEndpoint(arm.URL, client))

					err := call(context.Background())
					var apimErr *Error
					if !errors.As(err, &apimErr) || apimErr.StatusCode != status {
						t.Fatalf("%s = %v; want the %d as an *apim.Error", callName, err, status)
					}
					if seen.Load() != 1 {
						t.Errorf("fake ARM saw %d requests, want 1", seen.Load())
					}
					if got, _ := auth.Load().(string); got != "Bearer tok" {
						t.Errorf("ARM got Authorization %q, want the bearer token", got)
					}
					if elsewhere.Load() != 0 {
						t.Errorf("the relative redirect was followed %d times", elsewhere.Load())
					}
				})
			}
		}
	}
	if n := targetHits.Load(); n != 0 {
		got, _ := targetAuth.Load().(string)
		t.Errorf("the redirect target got %d requests (last Authorization %q), want none", n, got)
	}
}
