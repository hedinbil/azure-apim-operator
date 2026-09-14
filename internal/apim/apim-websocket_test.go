package apim

import (
	"encoding/json"
	"testing"
)

func decodeWebSocketBody(t *testing.T, cfg APIMDeploymentConfig) map[string]any {
	t.Helper()
	body, err := webSocketAPIBody(cfg)
	if err != nil {
		t.Fatalf("webSocketAPIBody() error = %v", err)
	}
	var payload struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	return payload.Properties
}

func TestWebSocketAPIBodyDefaults(t *testing.T) {
	props := decodeWebSocketBody(t, APIMDeploymentConfig{
		APIID:                "bidme-signalr-connect",
		RoutePrefix:          "/bidme/auctionhub",
		ServiceURL:           "wss://bidme.retail-prod.external.hedinit.io/auctionhub",
		SubscriptionRequired: true,
	})

	if got := props["type"]; got != "websocket" {
		t.Errorf("type = %v, want websocket", got)
	}
	if got := props["displayName"]; got != "bidme-signalr-connect" {
		t.Errorf("displayName = %v, want the APIID as fallback", got)
	}
	if got := props["path"]; got != "bidme/auctionhub" {
		t.Errorf("path = %v, want the route prefix without its leading slash", got)
	}
	if got := props["serviceUrl"]; got != "wss://bidme.retail-prod.external.hedinit.io/auctionhub" {
		t.Errorf("serviceUrl = %v", got)
	}
	if got := props["subscriptionRequired"]; got != true {
		t.Errorf("subscriptionRequired = %v, want true", got)
	}
	protocols, _ := props["protocols"].([]any)
	if len(protocols) != 1 || protocols[0] != "wss" {
		t.Errorf("protocols = %v, want [wss] by default", props["protocols"])
	}
}

func TestWebSocketAPIBodyHonoursExplicitValues(t *testing.T) {
	props := decodeWebSocketBody(t, APIMDeploymentConfig{
		APIID:       "connect",
		DisplayName: "Distribution - BidMe - SignalR connect",
		Protocols:   []string{"ws", "wss"},
		RoutePrefix: "bidme/auctionhub",
		ServiceURL:  "ws://example.internal/hub",
	})

	if got := props["displayName"]; got != "Distribution - BidMe - SignalR connect" {
		t.Errorf("displayName = %v", got)
	}
	if got := props["path"]; got != "bidme/auctionhub" {
		t.Errorf("path = %v, a prefix without a slash must pass through unchanged", got)
	}
	protocols, _ := props["protocols"].([]any)
	if len(protocols) != 2 || protocols[0] != "ws" || protocols[1] != "wss" {
		t.Errorf("protocols = %v, want [ws wss]", props["protocols"])
	}
	if got := props["subscriptionRequired"]; got != false {
		t.Errorf("subscriptionRequired = %v, want false when unset", got)
	}
}

// TestWebSocketAPIBodyIsMarshalledNotFormatted is the APIM-16 pattern: values
// come from a custom resource, so a quote in one must end up escaped inside a
// JSON string instead of terminating it.
func TestWebSocketAPIBodyIsMarshalledNotFormatted(t *testing.T) {
	props := decodeWebSocketBody(t, APIMDeploymentConfig{
		APIID:       "x",
		DisplayName: `He said "hi", "path": "/evil`,
		ServiceURL:  "wss://example.internal/hub",
	})
	if got := props["displayName"]; got != `He said "hi", "path": "/evil` {
		t.Errorf("displayName = %v, quotes must survive as data", got)
	}
	if got := props["path"]; got != "" {
		t.Errorf("path = %v, an injected key must not become the path", got)
	}
}
