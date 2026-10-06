// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file contains functions for managing tags in Azure APIM.
package apim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// UpsertTag creates or updates a tag in Azure APIM.
// Tags are used to categorize and organize APIs for easier management and discovery.
// If the tag already exists, it will be updated with the new display name.
func UpsertTag(ctx context.Context, config APIMTagConfig) error {
	tagURL := serviceURL(
		config, "tags", config.TagID)

	tagBody := map[string]interface{}{
		"properties": map[string]interface{}{
			"displayName": config.DisplayName,
		},
	}

	bodyBytes, err := json.Marshal(tagBody)
	if err != nil {
		return fmt.Errorf("failed to marshal tag body: %w", err)
	}

	logger.Info("🏷️ Upserting tag",
		"tagID", config.TagID,
		"url", tagURL,
	)

	resp, err := armRequest{
		operation:   "upsert tag " + config.TagID,
		method:      http.MethodPut,
		url:         tagURL,
		token:       config.BearerToken,
		body:        bodyBytes,
		contentType: contentTypeJSON,
		ifMatch:     "*",
	}.send(ctx)
	if err != nil {
		logger.Error(err, "❌ Failed to upsert tag", "tagID", config.TagID)
		return err
	}

	logger.Info("✅ Tag upserted",
		"tagID", config.TagID,
		"status", resp.status,
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
		tagAssignURL := serviceURL(
			config, "apis", config.APIID, "tags", tagID)

		logger.Info("🔖 Assigning tag to API",
			"apiID", config.APIID,
			"tagID", tagID,
			"url", tagAssignURL,
		)

		if _, err := (armRequest{
			operation: "assign tag " + tagID + " to API",
			method:    http.MethodPut,
			url:       tagAssignURL,
			token:     config.BearerToken,
			dependent: true,
		}).send(ctx); err != nil {
			logger.Error(err, "❌ Failed to assign tag to API",
				"apiID", config.APIID,
				"tagID", tagID,
			)
			return err
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
