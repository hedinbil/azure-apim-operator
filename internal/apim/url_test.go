package apim

import "testing"

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
