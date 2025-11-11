// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file contains functions for managing inbound policies in Azure APIM.
package apim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// UpsertInboundPolicy creates or updates an inbound policy for an API or a specific operation in Azure APIM.
// Policies are XML-based configurations that control how requests are processed.
// The policy content should be a complete policy XML document including all sections.
// If OperationID is provided, the policy will be applied to that specific operation (endpoint).
// If OperationID is not provided, the policy will be applied to the entire API.
func UpsertInboundPolicy(ctx context.Context, config APIMInboundPolicyConfig) error {
	// Skip if no API ID is provided.
	if config.APIID == "" {
		logger.Info("ℹ️ No API ID specified; skipping policy creation")
		return nil
	}

	// Skip if no policy content is provided.
	if config.PolicyContent == "" {
		logger.Info("ℹ️ No policy content specified; skipping policy creation")
		return nil
	}

	// Build the Azure Management API URL for setting the policy.
	// If OperationID is provided, apply to the specific operation.
	// Otherwise, apply to the entire API.
	var policyURL string
	if config.OperationID != "" {
		// Operation-level policy: /apis/{apiId}/operations/{operationId}/policies/policy
		policyURL = fmt.Sprintf(
			"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s/operations/%s/policies/policy?api-version=2021-08-01",
			config.SubscriptionID,
			config.ResourceGroup,
			config.ServiceName,
			config.APIID,
			config.OperationID,
		)
	} else {
		// API-level policy: /apis/{apiId}/policies/policy
		policyURL = fmt.Sprintf(
			"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s/policies/policy?api-version=2021-08-01",
			config.SubscriptionID,
			config.ResourceGroup,
			config.ServiceName,
			config.APIID,
		)
	}

	// Construct the request body with the policy XML.
	// Azure APIM expects the policy in a JSON structure with format and value.
	policyBody := map[string]interface{}{
		"properties": map[string]interface{}{
			"format": "xml",
			"value":  config.PolicyContent,
		},
	}

	bodyBytes, err := json.Marshal(policyBody)
	if err != nil {
		return fmt.Errorf("failed to marshal policy body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, policyURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to build policy request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.BearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "*")

	// Log the appropriate scope
	if config.OperationID != "" {
		logger.Info("📋 Upserting inbound policy for operation",
			"apiID", config.APIID,
			"operationID", config.OperationID,
			"url", policyURL,
		)
	} else {
		logger.Info("📋 Upserting inbound policy for API",
			"apiID", config.APIID,
			"url", policyURL,
		)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("policy request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		logger.Error(fmt.Errorf("status code: %d", resp.StatusCode), "❌ Failed to upsert inbound policy",
			"status", resp.Status,
			"body", string(respBody),
		)
		return fmt.Errorf("failed to upsert inbound policy: %s\n%s", resp.Status, string(respBody))
	}

	// Log success with appropriate scope
	if config.OperationID != "" {
		logger.Info("✅ Inbound policy upserted for operation",
			"apiID", config.APIID,
			"operationID", config.OperationID,
			"status", resp.Status,
		)
	} else {
		logger.Info("✅ Inbound policy upserted for API",
			"apiID", config.APIID,
			"status", resp.Status,
		)
	}

	return nil
}

// APIMInboundPolicyConfig contains the configuration needed to create or update an inbound policy in Azure APIM.
// Inbound policies are used to control the inbound traffic to an API.
type APIMInboundPolicyConfig struct {
	// SubscriptionID is the Azure subscription ID where the APIM service is located.
	SubscriptionID string
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string
	// APIID is the unique identifier for the API in APIM where the policy will be applied.
	APIID string
	// OperationID is the unique identifier for the operation (endpoint) within the API.
	// If specified, the policy will be applied to this specific operation.
	// If not specified, the policy will be applied to the entire API.
	OperationID string
	// ServiceName is the name of the Azure API Management service instance.
	ServiceName string
	// BearerToken is the Azure AD authentication token for the APIM management API.
	BearerToken string
	// PolicyContent is the XML content of the policy to be applied.
	PolicyContent string
}
