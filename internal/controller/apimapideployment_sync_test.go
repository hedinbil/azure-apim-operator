package controller

import (
	"testing"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

// TestBuildDesiredAPIMStateHashCoversTheAPIType makes sure a change of type,
// display name or protocols re-applies the API instead of being read as
// "already in sync", which is what would happen if the hash left them out.
func TestBuildDesiredAPIMStateHashCoversTheAPIType(t *testing.T) {
	base := apimv1.APIMAPIDeploymentSpec{
		Type:        apimv1.APITypeWebSocket,
		APIID:       "orders",
		APIMService: "apim",
		RoutePrefix: "/orders",
		ServiceURL:  "wss://orders.internal/hub",
	}
	hashOf := func(mutate func(*apimv1.APIMAPIDeploymentSpec)) string {
		spec := *base.DeepCopy()
		mutate(&spec)
		hash, err := buildDesiredAPIMStateHash(&spec, "sub", "rg", "")
		if err != nil {
			t.Fatalf("buildDesiredAPIMStateHash() error = %v", err)
		}
		return hash
	}

	baseline := hashOf(func(*apimv1.APIMAPIDeploymentSpec) {})
	if again := hashOf(func(*apimv1.APIMAPIDeploymentSpec) {}); again != baseline {
		t.Fatalf("hash is not stable: %s vs %s", baseline, again)
	}

	cases := map[string]func(*apimv1.APIMAPIDeploymentSpec){
		"type": func(s *apimv1.APIMAPIDeploymentSpec) { s.Type = apimv1.APITypeHTTP },
		"displayName": func(s *apimv1.APIMAPIDeploymentSpec) {
			s.WebSocket = &apimv1.APIMAPIWebSocket{DisplayName: "Orders hub"}
		},
		"protocols": func(s *apimv1.APIMAPIDeploymentSpec) {
			s.WebSocket = &apimv1.APIMAPIWebSocket{Protocols: []string{"ws", "wss"}}
		},
	}
	for name, mutate := range cases {
		if hashOf(mutate) == baseline {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

// TestBuildDesiredAPIMStateHashIsUnchangedForHTTPAPIs guards the upgrade path:
// every APIMAPI that predates the type field is served with the default "http",
// and its hash must equal the one the operator applied before the field existed.
// Otherwise the upgrade would re-import every API in the fleet once.
func TestBuildDesiredAPIMStateHashIsUnchangedForHTTPAPIs(t *testing.T) {
	legacy := apimv1.APIMAPIDeploymentSpec{
		APIID:                "orders",
		APIMService:          "apim",
		RoutePrefix:          "/orders",
		ServiceURL:           "https://orders.internal",
		OpenAPIDefinitionURL: "https://orders.internal/swagger.json",
	}
	defaulted := *legacy.DeepCopy()
	defaulted.Type = apimv1.APITypeHTTP

	legacyHash, err := buildDesiredAPIMStateHash(&legacy, "sub", "rg", "doc")
	if err != nil {
		t.Fatalf("buildDesiredAPIMStateHash() error = %v", err)
	}
	defaultedHash, err := buildDesiredAPIMStateHash(&defaulted, "sub", "rg", "doc")
	if err != nil {
		t.Fatalf("buildDesiredAPIMStateHash() error = %v", err)
	}
	if legacyHash != defaultedHash {
		t.Errorf("type=http changed the hash: %s vs %s", legacyHash, defaultedHash)
	}
}
