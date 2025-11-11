// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file contains functions for managing products in Azure APIM.
package apim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// UpsertProduct creates or updates a product in Azure APIM.
// Products are used to group APIs and require subscriptions for access.
// If the product already exists, it will be updated with the new configuration.
func UpsertProduct(ctx context.Context, config APIMProductConfig) error {
	// Skip if no product ID is provided.
	if config.ProductID == "" {
		logger.Info("ℹ️ No product ID specified; skipping product creation")
		return nil
	}

	productURL := fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/products/%s?api-version=2021-08-01",
		config.SubscriptionID,
		config.ResourceGroup,
		config.ServiceName,
		config.ProductID,
	)

	// Determine the product state based on the Published flag.
	// Published products are visible in the developer portal and can be subscribed to.
	state := "notPublished"
	if config.Published {
		state = "published"
	}

	productBody := map[string]interface{}{
		"properties": map[string]interface{}{
			"displayName":          config.DisplayName,
			"description":          config.Description,
			"subscriptionRequired": true,
			"approvalRequired":     false,
			"subscriptionsLimit":   1000,
			"state":                state,
		},
	}

	bodyBytes, err := json.Marshal(productBody)
	if err != nil {
		return fmt.Errorf("failed to marshal product body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, productURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to build product creation request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.BearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "*")

	logger.Info("📦 Creating or updating product",
		"productId", config.ProductID,
		"url", productURL,
	)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("product creation request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		logger.Error(fmt.Errorf("status code: %d", resp.StatusCode), "❌ Failed to create product",
			"status", resp.Status,
			"body", string(body),
		)
		return fmt.Errorf("failed to create product: %s\n%s", resp.Status, string(body))
	}

	logger.Info("✅ Product created or already exists",
		"productId", config.ProductID,
		"status", resp.Status,
	)

	return nil
}

// AssignProductsToAPI associates an API with one or more products in Azure APIM.
// Products are used to group APIs and require subscriptions for access.
// This function assigns the API to all products specified in the config.
func AssignProductsToAPI(ctx context.Context, config APIMDeploymentConfig) error {
	// If no products are configured, skip the assignment.
	if len(config.ProductIDs) == 0 {
		logger.Info("ℹ️ No products configured for assignment; skipping")
		return nil
	}

	// Assign the API to each product in the list.
	for _, productID := range config.ProductIDs {
		productAssignURL := fmt.Sprintf(
			"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/products/%s/apis/%s?api-version=2021-08-01",
			config.SubscriptionID,
			config.ResourceGroup,
			config.ServiceName,
			productID,
			config.APIID,
		)

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, productAssignURL, nil)
		if err != nil {
			return fmt.Errorf("failed to build product assign request for %s: %w", productID, err)
		}

		req.Header.Set("Authorization", "Bearer "+config.BearerToken)

		logger.Info("📦 Assigning API to product",
			"apiID", config.APIID,
			"productID", productID,
			"url", productAssignURL,
		)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("product assign request failed for %s: %w", productID, err)
		}
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil {
				logger.Error(closeErr, "⚠️ Failed to close response body")
			}
		}()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return fmt.Errorf("assigning API to product %s failed: %s\n%s", productID, resp.Status, string(body))
		}

		logger.Info("✅ API successfully assigned to product",
			"apiID", config.APIID,
			"productID", productID,
		)
	}

	return nil
}

// APIMProductConfig contains the configuration needed to create or update a product in Azure APIM.
// Products are used to group APIs and require subscriptions for access.
type APIMProductConfig struct {
	// SubscriptionID is the Azure subscription ID where the APIM service is located.
	SubscriptionID string
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string
	// ServiceName is the name of the Azure API Management service instance.
	ServiceName string
	// ProductID is the unique identifier for the product in APIM.
	ProductID string
	// DisplayName is the friendly name shown in the APIM UI.
	DisplayName string
	// Description is an optional description of the product.
	Description string
	// BearerToken is the Azure AD authentication token for the APIM management API.
	BearerToken string
	// Published indicates whether the product should be published and visible in the developer portal.
	Published bool
}
