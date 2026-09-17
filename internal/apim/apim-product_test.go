package apim

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTrip lets a test stand in for ARM: it records the request and answers
// with a canned status and body.
type roundTrip struct {
	status int
	body   string
	got    *http.Request
}

func (rt *roundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.got = req
	return &http.Response{
		StatusCode: rt.status,
		Status:     fmt.Sprintf("%d %s", rt.status, http.StatusText(rt.status)), // as net/http renders it
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func withTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	previous := httpClient
	httpClient = &http.Client{Transport: rt}
	t.Cleanup(func() { httpClient = previous })
}

func productConfig() APIMProductConfig {
	return APIMProductConfig{
		SubscriptionID: "sub-1", ResourceGroup: "rg-1", ServiceName: "apim-1",
		ProductID: "p1", BearerToken: "tok",
	}
}

// TestDeleteProductSendsTheRequestAPIMAccepts pins the one request shape that
// works: a DELETE with deleteSubscriptions=true, since APIM answers 400 for any
// product that still has a subscription otherwise, and If-Match: * so no ETag
// round-trip is needed.
func TestDeleteProductSendsTheRequestAPIMAccepts(t *testing.T) {
	rt := &roundTrip{status: http.StatusOK}
	withTransport(t, rt)

	if err := DeleteProduct(context.Background(), productConfig()); err != nil {
		t.Fatalf("DeleteProduct() = %v, want nil", err)
	}
	if rt.got == nil {
		t.Fatal("no request was sent")
	}
	if rt.got.Method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", rt.got.Method)
	}
	want := prefix + "/products/p1?api-version=" + apiVersion + "&deleteSubscriptions=true"
	if got := rt.got.URL.String(); got != want {
		t.Errorf("url =\n  %s\nwant\n  %s", got, want)
	}
	if got := rt.got.Header.Get("If-Match"); got != "*" {
		t.Errorf("If-Match = %q, want *", got)
	}
	if got := rt.got.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", got)
	}
}

// TestDeleteProductStatusHandling covers the three outcomes the controller
// distinguishes: done, already gone, refused.
func TestDeleteProductStatusHandling(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string // empty means success
	}{
		{"200 deleted", http.StatusOK, "", ""},
		{"204 deleted", http.StatusNoContent, "", ""},
		{"404 already gone is success", http.StatusNotFound, `{"error":{"code":"ResourceNotFound"}}`, ""},
		{"400 refused surfaces the body", http.StatusBadRequest,
			`{"error":{"code":"ValidationError","message":"Product cannot be deleted since it has existing subscriptions."}}`,
			"existing subscriptions"},
		{"403 surfaces the status", http.StatusForbidden, "", "403"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTransport(t, &roundTrip{status: tc.status, body: tc.body})
			err := DeleteProduct(context.Background(), productConfig())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("DeleteProduct() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("DeleteProduct() = nil, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("DeleteProduct() = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestDeleteProductSkipsWithoutAnID keeps the guard that an empty productId
// never turns into a DELETE on the products collection.
func TestDeleteProductSkipsWithoutAnID(t *testing.T) {
	rt := &roundTrip{status: http.StatusOK}
	withTransport(t, rt)
	cfg := productConfig()
	cfg.ProductID = ""
	if err := DeleteProduct(context.Background(), cfg); err != nil {
		t.Fatalf("DeleteProduct() = %v, want nil", err)
	}
	if rt.got != nil {
		t.Errorf("a request was sent to %s; want none", rt.got.URL)
	}
}
