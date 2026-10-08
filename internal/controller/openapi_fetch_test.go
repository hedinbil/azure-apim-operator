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

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hedinit/azure-apim-operator/internal/apim"
)

const openAPIJSON = `{"openapi":"3.0.0","info":{"title":"test","version":"1.0.0"},"paths":{}}`

// testOpenAPIFetcher allows loopback so tests can use httptest servers.
func testOpenAPIFetcher() *openAPIFetcher {
	return newOpenAPIFetcher(2*time.Second, maxOpenAPIBytes, true)
}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAPIFetcherAcceptsJSONAndYAML(t *testing.T) {
	json := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(openAPIJSON)) })
	yamlDoc := "swagger: \"2.0\"\ninfo:\n  title: t\n  version: \"1\"\npaths: {}\n"
	yml := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(yamlDoc)) })

	f := testOpenAPIFetcher()
	got, err := f.Fetch(context.Background(), json.URL)
	if err != nil || string(got) != openAPIJSON {
		t.Fatalf("json fetch: got %q, %v", got, err)
	}
	got, err = f.Fetch(context.Background(), yml.URL)
	if err != nil || string(got) != yamlDoc {
		t.Fatalf("yaml fetch: got %q, %v", got, err)
	}
}

func TestOpenAPIFetcherRejectsBadResponses(t *testing.T) {
	html := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>login</html>")) })
	notOpenAPI := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"hello":"world"}`)) })
	teapot := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("secret body"))
	})
	f := testOpenAPIFetcher()

	if _, err := f.Fetch(context.Background(), html.URL); err == nil || !strings.Contains(err.Error(), "neither JSON nor a YAML mapping") {
		t.Fatalf("html must be rejected, got %v", err)
	}
	if _, err := f.Fetch(context.Background(), notOpenAPI.URL); !errors.Is(err, apim.ErrNotOpenAPIDocument) {
		t.Fatalf("non-OpenAPI JSON must be rejected, got %v", err)
	}
	_, err := f.Fetch(context.Background(), teapot.URL)
	if err == nil || !strings.Contains(err.Error(), "418") {
		t.Fatalf("non-2xx must be rejected with the status, got %v", err)
	}
	if strings.Contains(err.Error(), "secret body") {
		t.Fatalf("error must not carry the response body: %v", err)
	}
}

func TestOpenAPIFetcherEnforcesSizeCap(t *testing.T) {
	big := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openapi":"3.0.0","info":{"description":"` + strings.Repeat("x", 200) + `"}}`))
	})
	f := newOpenAPIFetcher(2*time.Second, 64, true)
	if _, err := f.Fetch(context.Background(), big.URL); err == nil || !strings.Contains(err.Error(), "exceeds 64 bytes") {
		t.Fatalf("oversized document must be rejected, got %v", err)
	}
}

func TestOpenAPIFetcherRejectsSchemesAndCredentials(t *testing.T) {
	f := testOpenAPIFetcher()
	for _, raw := range []string{"ftp://example.com/openapi.json", "file:///etc/passwd", "http:///nohost", "http://user:pw@example.com/x"} {
		if _, err := f.Fetch(context.Background(), raw); err == nil {
			t.Fatalf("%s must be rejected", raw)
		}
	}
}

func TestOpenAPIFetcherTimesOut(t *testing.T) {
	slow := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(openAPIJSON))
	})
	f := newOpenAPIFetcher(100*time.Millisecond, maxOpenAPIBytes, true)
	start := time.Now()
	if _, err := f.Fetch(context.Background(), slow.URL); err == nil {
		t.Fatal("slow endpoint must time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
}

func TestOpenAPIFetcherRefusesLoopbackByDefault(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(openAPIJSON)) })
	f := newOpenAPIFetcher(2*time.Second, maxOpenAPIBytes, false)
	if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, errBlockedOpenAPIIP) {
		t.Fatalf("loopback must be refused unless allowed, got %v", err)
	}
}

func TestBlockedDestination(t *testing.T) {
	blocked := []string{"169.254.169.254", "169.254.0.7", "127.0.0.1", "::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "fe80::1"}
	allowed := []string{"10.1.2.3", "172.16.0.10", "192.168.1.1", "fd00::1", "1.2.3.4", "2001:db8::1"}
	for _, s := range blocked {
		if !blockedDestination(net.ParseIP(s), false) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range allowed {
		if blockedDestination(net.ParseIP(s), false) {
			t.Errorf("%s must be allowed", s)
		}
	}
	if blockedDestination(net.ParseIP("127.0.0.1"), true) {
		t.Error("loopback must pass when explicitly allowed")
	}
	if !blockedDestination(nil, true) {
		t.Error("an unparseable address must be blocked")
	}
}

// TestBlockedDestinationAzureWireServer: the Azure host agent is not link-local, so it is
// refused by address, even with loopback allowed.
func TestBlockedDestinationAzureWireServer(t *testing.T) {
	for _, allowLoopback := range []bool{false, true} {
		if !blockedDestination(net.ParseIP("168.63.129.16"), allowLoopback) {
			t.Errorf("168.63.129.16 must be blocked (allowLoopback %t)", allowLoopback)
		}
	}
	if blockedDestination(net.ParseIP("168.63.129.17"), false) {
		t.Error("only the wire server itself is blocked, not its neighbours")
	}
}

// TestOpenAPIFetcherFollowsAtMostThreeRedirects: up to maxOpenAPIRedirects hops are
// followed, one more fails.
func TestOpenAPIFetcherFollowsAtMostThreeRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = serve(t, func(w http.ResponseWriter, r *http.Request) {
		var hops int
		_, _ = fmt.Sscanf(r.URL.Path, "/hop/%d", &hops)
		if hops > 0 {
			http.Redirect(w, r, fmt.Sprintf("%s/hop/%d", srv.URL, hops-1), http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(openAPIJSON))
	})
	f := testOpenAPIFetcher()
	for hops := 0; hops <= maxOpenAPIRedirects; hops++ {
		if got, err := f.Fetch(context.Background(), fmt.Sprintf("%s/hop/%d", srv.URL, hops)); err != nil || string(got) != openAPIJSON {
			t.Errorf("%d redirects: got %q, %v; want the document", hops, got, err)
		}
	}
	_, err := f.Fetch(context.Background(), fmt.Sprintf("%s/hop/%d", srv.URL, maxOpenAPIRedirects+1))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("stopped after %d redirects", maxOpenAPIRedirects)) {
		t.Fatalf("%d redirects: got %v, want the redirect limit", maxOpenAPIRedirects+1, err)
	}
}

// TestOpenAPIFetcherRefusesRedirectsToOtherSchemesAndBlockedHosts: every hop is checked
// like the first URL.
func TestOpenAPIFetcherRefusesRedirectsToOtherSchemesAndBlockedHosts(t *testing.T) {
	for name, target := range map[string]string{
		"file scheme":     "file:///etc/passwd",
		"credentials":     "http://user:pw@example.com/x",
		"IMDS":            "http://169.254.169.254/metadata/instance",
		"Azure wire host": "http://168.63.129.16/machine",
	} {
		t.Run(name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, http.StatusFound) })
			if _, err := testOpenAPIFetcher().Fetch(context.Background(), srv.URL); err == nil {
				t.Fatalf("a redirect to %s must be refused", target)
			}
		})
	}
}

// TestOpenAPIFetcherRedactsTheURLInErrors: a key in the query string never reaches an
// error (and with it the status), neither for a bad status nor for a transport failure,
// whose *url.Error would repeat the full URL.
func TestOpenAPIFetcherRedactsTheURLInErrors(t *testing.T) {
	const secret = "code=s3cr3t-function-key"
	teapot := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	f := newOpenAPIFetcher(500*time.Millisecond, maxOpenAPIBytes, true)
	for name, raw := range map[string]string{
		"bad status":         teapot.URL + "/openapi.json?" + secret,
		"connection refused": closedURL + "/openapi.json?" + secret,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.Fetch(context.Background(), raw)
			if err == nil {
				t.Fatal("the fetch must fail")
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error leaks the query string: %v", err)
			}
			if !strings.Contains(err.Error(), apim.RedactURL(raw)) {
				t.Errorf("error %q does not name the redacted URL %q", err, apim.RedactURL(raw))
			}
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				t.Errorf("error still wraps a *url.Error that carries the full URL: %v", urlErr)
			}
		})
	}
}
