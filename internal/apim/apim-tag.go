// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file contains functions for managing tags in Azure APIM.
package apim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// UpsertTag creates or updates a tag in Azure APIM.
// Tags are used to categorize and organize APIs for easier management and discovery.
// If the tag already exists, it will be updated with the new display name.
func UpsertTag(ctx context.Context, config APIMTagConfig) error {
	tagURL := fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/tags/%s?api-version=2021-08-01",
		config.SubscriptionID,
		config.ResourceGroup,
		config.ServiceName,
		config.TagID,
	)

	tagBody := map[string]interface{}{
		"properties": map[string]interface{}{
			"displayName": config.DisplayName,
		},
	}

	bodyBytes, err := json.Marshal(tagBody)
	if err != nil {
		return fmt.Errorf("failed to marshal tag body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, tagURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to build tag request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.BearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "*")

	logger.Info("🏷️ Upserting tag",
		"tagID", config.TagID,
		"url", tagURL,
	)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("tag request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		logger.Error(fmt.Errorf("status code: %d", resp.StatusCode), "❌ Failed to upsert tag",
			"status", resp.Status,
			"body", string(respBody),
		)
		return fmt.Errorf("failed to upsert tag: %s\n%s", resp.Status, string(respBody))
	}

	logger.Info("✅ Tag upserted",
		"tagID", config.TagID,
		"status", resp.Status,
	)

	return nil
}

// AssignTagsToAPI applies one or more tags to an API in Azure APIM.
// Tags help organize and categorize APIs for better management and discovery.
// This function assigns all tags specified in the config to the API.
func AssignTagsToAPI(ctx context.Context, config APIMDeploymentConfig) error {
	// If no tags are configured, skip the assignment.
	if len(config.TagIDs) == 0 {
		logger.Info("ℹ️ No tags configured for assignment; skipping")
		return nil
	}

	// Assign each tag to the API.
	for _, tagID := range config.TagIDs {
		tagAssignURL := fmt.Sprintf(
			"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s/tags/%s?api-version=2021-08-01",
			config.SubscriptionID,
			config.ResourceGroup,
			config.ServiceName,
			config.APIID,
			tagID,
		)

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, tagAssignURL, nil)
		if err != nil {
			return fmt.Errorf("failed to build tag assign request for %s: %w", tagID, err)
		}

		req.Header.Set("Authorization", "Bearer "+config.BearerToken)

		logger.Info("🔖 Assigning tag to API",
			"apiID", config.APIID,
			"tagID", tagID,
			"url", tagAssignURL,
		)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("tag assign request failed for %s: %w", tagID, err)
		}
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil {
				logger.Error(closeErr, "⚠️ Failed to close response body")
			}
		}()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			logger.Error(fmt.Errorf("status code: %d", resp.StatusCode), "❌ Failed to assign tag to API",
				"status", resp.Status,
				"body", string(body),
			)
			return fmt.Errorf("assigning tag to API %s failed: %s\n%s", tagID, resp.Status, string(body))
		}

		logger.Info("✅ Tag successfully assigned to API",
			"apiID", config.APIID,
			"tagID", tagID,
		)
	}

	return nil
}

// APIMTagConfig contains the configuration needed to create or update a tag in Azure APIM.
// Tags are used to categorize and organize APIs.
type APIMTagConfig struct {
	// SubscriptionID is the Azure subscription ID where the APIM service is located.
	SubscriptionID string
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string
	// ServiceName is the name of the Azure API Management service instance.
	ServiceName string
	// BearerToken is the Azure AD authentication token for the APIM management API.
	BearerToken string
	// TagID is the unique identifier for the tag in APIM.
	TagID string
	// DisplayName is the friendly name shown in the APIM UI.
	DisplayName string
}
