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

// TestDeploymentSpecChanged pins when a new desired hash clears a deployment's failures.
// A change in the spec always does; a change in the fetched OpenAPI document alone does
// unless the deployment is backing off, so a document that differs on every fetch can
// neither skip the backoff nor keep the count from reaching Stalled.
func TestDeploymentSpecChanged(t *testing.T) {
	spec := apimv1.APIMAPIDeploymentSpec{APIID: "orders", APIMService: "svc", RoutePrefix: "/orders", ServiceURL: "https://orders"}
	edited := spec
	edited.RoutePrefix = "/orders-v2"
	docA, docB := sha256Hex([]byte("A")), sha256Hex([]byte("B"))
	hash := func(s apimv1.APIMAPIDeploymentSpec, doc string) string {
		t.Helper()
		h, err := buildDesiredAPIMStateHash(&s, "sub", "rg", doc)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	failedWithA := func(failures int32, next string) apimv1.APIMAPIDeploymentStatus {
		return apimv1.APIMAPIDeploymentStatus{
			OpenAPIHash: docA, DesiredHash: hash(spec, docA),
			RetryStatus: apimv1.RetryStatus{ConsecutiveFailures: failures, NextAttemptAt: next},
		}
	}
	const next = "2026-09-25T10:01:00Z"

	cases := []struct {
		name   string
		spec   apimv1.APIMAPIDeploymentSpec
		status apimv1.APIMAPIDeploymentStatus
		doc    string
		want   bool
	}{
		{"same spec, same document", spec, failedWithA(2, next), docA, false},
		{"first attempt ever", spec, apimv1.APIMAPIDeploymentStatus{}, docA, true},
		{"new document, no failures", spec, failedWithA(0, ""), docB, true},
		{"new document while backing off", spec, failedWithA(2, next), docB, false},
		{"new document while Stalled", spec, failedWithA(5, ""), docB, true},
		{"new document while Invalid", spec, failedWithA(1, ""), docB, true},
		{"spec edit while backing off", edited, failedWithA(2, next), docA, true},
		{"spec edit and new document while backing off", edited, failedWithA(2, next), docB, true},
		{"back to the document the failures were recorded against", spec, failedWithA(3, next), docA, false},
		{"status hashes out of step (no document hash recorded)", spec,
			apimv1.APIMAPIDeploymentStatus{DesiredHash: hash(spec, docA), RetryStatus: apimv1.RetryStatus{ConsecutiveFailures: 2, NextAttemptAt: next}},
			docB, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			got := deploymentSpecChanged(&tc.spec, &status, "sub", "rg", hash(tc.spec, tc.doc))
			if got != tc.want {
				t.Errorf("deploymentSpecChanged = %v, want %v", got, tc.want)
			}
		})
	}
}
