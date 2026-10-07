// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file contains functions for managing products in Azure APIM.
package apim

import (
	"context"
	"encoding/json"
	"fmt"
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

	productURL := serviceURL(
		config, "products", config.ProductID)

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

	logger.Info("📦 Creating or updating product",
		"productId", config.ProductID,
		"url", productURL,
	)

	resp, err := armRequest{
		operation:   "upsert product " + config.ProductID,
		method:      http.MethodPut,
		url:         productURL,
		token:       config.BearerToken,
		body:        bodyBytes,
		contentType: contentTypeJSON,
		ifMatch:     "*",
	}.send(ctx)
	if err != nil {
		logger.Error(err, "❌ Failed to create product", "productId", config.ProductID)
		return err
	}

	logger.Info("✅ Product created or already exists",
		"productId", config.ProductID,
		"status", resp.status,
	)

	return nil
}

// DeleteProduct deletes a product from Azure APIM.
// Products are used to group APIs and require subscriptions for access.
// This function removes the product from the APIM service.
func DeleteProduct(ctx context.Context, config APIMProductConfig) error {
	// Skip if no product ID is provided.
	if config.ProductID == "" {
		logger.Info("ℹ️ No product ID specified; skipping product deletion")
		return nil
	}

	// APIM refuses to delete a product that still has subscriptions unless they are
	// deleted with it; without the flag the call is a 400 ValidationError for any
	// product actually in use. A Delete policy means the product and its keys go.
	productURL := withQuery(serviceURL(
		config, "products", config.ProductID), "deleteSubscriptions", "true")

	logger.Info("🗑️ Deleting product",
		"productId", config.ProductID,
		"url", productURL,
	)

	resp, err := armRequest{
		operation: "delete product " + config.ProductID,
		method:    http.MethodDelete,
		url:       productURL,
		token:     config.BearerToken,
		ifMatch:   "*",
	}.send(ctx)
	if IsNotFound(err) {
		logger.Info("ℹ️ Product not found, already deleted",
			"productId", config.ProductID,
		)
		return nil // Product doesn't exist, consider deletion successful
	}
	if err != nil {
		logger.Error(err, "❌ Failed to delete product", "productId", config.ProductID)
		return err
	}

	logger.Info("✅ Product deleted successfully",
		"productId", config.ProductID,
		"status", resp.status,
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
		productAssignURL := serviceURL(
			config, "products", productID, "apis", config.APIID)

		logger.Info("📦 Assigning API to product",
			"apiID", config.APIID,
			"productID", productID,
			"url", productAssignURL,
		)

		if _, err := (armRequest{
			operation: "assign API to product " + productID,
			method:    http.MethodPut,
			url:       productAssignURL,
			token:     config.BearerToken,
			dependent: true,
		}).send(ctx); err != nil {
			return err
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
