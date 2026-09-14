// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file creates WebSocket APIs, which APIM models as their own API type: no OpenAPI
// document, a single onHandshake operation that APIM adds itself, and a ws(s) backend.
package apim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// defaultWebSocketProtocols is what a websocket API is exposed on when the resource
// does not say. Plain ws is opt-in; APIM's own default when creating through the
// portal is wss as well.
var defaultWebSocketProtocols = []string{"wss"}

// UpsertWebSocketAPI creates or updates a WebSocket API in Azure APIM from the config
// alone. It is the websocket counterpart of ImportOpenAPIDefinitionToAPIM: same URL,
// same If-Match handling, same async completion, but a JSON body with type "websocket"
// instead of an OpenAPI import.
func UpsertWebSocketAPI(ctx context.Context, apimParams APIMDeploymentConfig) error {
	etag := ifMatchForUpsert(ctx, apimParams)

	body, err := webSocketAPIBody(apimParams)
	if err != nil {
		return err
	}

	upsertURL := revisionURL(apimParams, apimParams.APIID, apimParams.Revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, upsertURL, bytes.NewReader(body))
	if err != nil {
		logger.Error(err, "❌ Failed to build APIM request", "apiID", apimParams.APIID)
		return fmt.Errorf("failed to build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apimParams.BearerToken)
	req.Header.Set("If-Match", etag)

	logger.Info("📤 Sending WebSocket API request to APIM",
		"method", req.Method,
		"url", req.URL.String(),
		"apiID", apimParams.APIID,
		"routePrefix", apimParams.RoutePrefix,
		"serviceUrl", apimParams.ServiceURL,
		"protocols", webSocketProtocols(apimParams),
		"ifMatch", etag,
	)

	return doAPIUpsert(ctx, apimParams, req, "created WebSocket API in")
}

// webSocketAPIBody is the PUT body for a websocket API. Kept separate from the
// request so it can be tested without APIM.
func webSocketAPIBody(apimParams APIMDeploymentConfig) ([]byte, error) {
	displayName := apimParams.DisplayName
	if displayName == "" {
		displayName = apimParams.APIID
	}

	// Marshalled, not formatted, so a quote in a display name or URL cannot
	// change the shape of the request (same reasoning as AssignServiceUrlToApi).
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{
			"type":        "websocket",
			"displayName": displayName,
			// APIM stores the path without a leading slash; the import endpoint
			// tolerates one but the plain PUT rejects it.
			"path":                 strings.TrimPrefix(apimParams.RoutePrefix, "/"),
			"protocols":            webSocketProtocols(apimParams),
			"serviceUrl":           apimParams.ServiceURL,
			"subscriptionRequired": apimParams.SubscriptionRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal websocket API body: %w", err)
	}
	return body, nil
}

func webSocketProtocols(apimParams APIMDeploymentConfig) []string {
	if len(apimParams.Protocols) == 0 {
		return defaultWebSocketProtocols
	}
	return apimParams.Protocols
}
