// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// These functions handle importing APIs, updating service URLs, assigning products and tags,
// and retrieving API and service information from Azure APIM.
package apim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
)

// logger is the logger instance for APIM operations.
var logger = ctrl.Log.WithName("apim")

// ImportOpenAPIDefinitionToAPIM imports an OpenAPI/Swagger definition into Azure API Management.
// It creates or updates an API in APIM with the provided OpenAPI content, route prefix, and optional revision.
// The function uses the Azure Management API to perform the import operation.
func ImportOpenAPIDefinitionToAPIM(ctx context.Context, apimParams APIMDeploymentConfig, openApiContent []byte) error {
	// Construct the API ID, including revision if specified.
	// APIM uses the format "apiId;rev=revisionNumber" for revisions.
	apiID := apimParams.APIID
	if apimParams.Revision != "" {
		apiID = fmt.Sprintf("%s;rev=%s", apimParams.APIID, apimParams.Revision)
	}

	// Build the Azure Management API URL for importing the API.
	importURL := fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s?api-version=2021-08-01",
		apimParams.SubscriptionID,
		apimParams.ResourceGroup,
		apimParams.ServiceName,
		apiID,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, importURL, bytes.NewReader(openApiContent))
	if err != nil {
		logger.Error(err, "❌ Failed to build APIM request")
		return fmt.Errorf("failed to build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/vnd.oai.openapi+json") // or +json if needed
	req.Header.Set("Authorization", "Bearer "+apimParams.BearerToken)

	q := req.URL.Query()
	q.Set("import", "true")
	q.Set("path", apimParams.RoutePrefix)
	if apimParams.Revision != "" {
		q.Set("createRevision", "true")
	}
	req.Header.Set("If-Match", "*") // <-- Required to overwrite existing APIs
	req.URL.RawQuery = q.Encode()

	logger.Info("📤 Sending request to APIM",
		"method", req.Method,
		"url", req.URL.String(),
		"apiID", apimParams.APIID,
		"routePrefix", apimParams.RoutePrefix,
	)

	// Log beginning of the Swagger content for debug purposes
	snippet := string(openApiContent)
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	logger.Info("📄 Swagger snippet", "preview", strings.TrimSpace(snippet))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Error(err, "❌ Failed to send request to APIM")
		return fmt.Errorf("failed to call APIM API: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		logger.Error(fmt.Errorf("status code: %d", resp.StatusCode), "❌ APIM API returned error", "status", resp.Status, "body", string(body))
		return fmt.Errorf("APIM API failed: %s\n%s", resp.Status, string(body))
	}

	logger.Info("✅ Successfully imported API into APIM",
		"api", apimParams.APIID,
		"status", resp.Status,
		"statusCode", resp.StatusCode,
	)

	return nil
}

// AssignServiceUrlToApi updates the backend service URL for an existing API in Azure APIM.
// This is used to point an API to a different backend service without re-importing the OpenAPI definition.
func AssignServiceUrlToApi(ctx context.Context, config APIMDeploymentConfig) error {
	patchURL := fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s?api-version=2021-08-01",
		config.SubscriptionID,
		config.ResourceGroup,
		config.ServiceName,
		config.APIID,
	)

	body := fmt.Sprintf(`{"properties":{"serviceUrl":"%s"}}`, config.ServiceURL)

	// Log what we’re about to do
	logger.Info("🔧 Patching APIM service URL",
		"method", http.MethodPatch,
		"url", patchURL,
		"apiID", config.APIID,
		"serviceUrl", config.ServiceURL,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("building PATCH request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.BearerToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("patch request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		errMsg := fmt.Errorf("status code: %d", resp.StatusCode)
		logger.Error(errMsg, "❌ PATCH returned error",
			"apiID", config.APIID,
			"status", resp.Status,
			"body", string(respBody),
		)
		return fmt.Errorf("serviceUrl patch failed: %s\n%s", resp.Status, string(respBody))
	}

	logger.Info("✅ Successfully patched serviceUrl",
		"apiID", config.APIID,
		"status", resp.Status,
		"serviceUrl", config.ServiceURL,
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

// GetAPIRevisions retrieves all revisions for an API from Azure APIM.
// API revisions allow you to version APIs and test changes before making them current.
func GetAPIRevisions(ctx context.Context, config APIMDeploymentConfig) ([]APIRevision, error) {
	url := fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s/revisions?api-version=2021-08-01",
		config.SubscriptionID,
		config.ResourceGroup,
		config.ServiceName,
		config.APIID,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		logger.Error(err, "❌ Failed to build request for API revisions")
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.BearerToken)

	logger.Info("🔎 Requesting API revisions from APIM",
		"apiID", config.APIID,
		"url", url,
	)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Error(err, "❌ Failed to request API revisions")
		return nil, fmt.Errorf("failed to call APIM API: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		logger.Error(fmt.Errorf("status code: %d", resp.StatusCode), "❌ Failed to get API revisions",
			"status", resp.Status,
			"body", string(body),
		)
		return nil, fmt.Errorf("failed to get API revisions: %s\n%s", resp.Status, string(body))
	}

	var result APIRevisionListResponse
	if err := json.Unmarshal(body, &result); err != nil {
		logger.Error(err, "❌ Failed to parse API revisions response")
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	logger.Info("✅ Successfully retrieved API revisions",
		"apiID", config.APIID,
		"revisionCount", len(result.Value),
	)

	return result.Value, nil
}

// GetAPIMServiceDetails retrieves hostname information for an Azure APIM service instance.
// It returns the API gateway hostname (Proxy) and the developer portal hostname.
// This information is used to construct full URLs for accessing APIs through APIM.
func GetAPIMServiceDetails(ctx context.Context, config APIMDeploymentConfig) (apiHost, developerPortalHost string, err error) {
	url := fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s?api-version=2021-08-01",
		config.SubscriptionID,
		config.ResourceGroup,
		config.ServiceName,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("building request for APIM service details: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+config.BearerToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("request to get APIM service details failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body")
		}
	}()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("failed to get APIM service details: %s\n%s", resp.Status, string(body))
	}

	var serviceInfo struct {
		Properties struct {
			HostnameConfigurations []struct {
				Type     string `json:"type"`
				HostName string `json:"hostName"`
			} `json:"hostnameConfigurations"`
		} `json:"properties"`
	}

	if err := json.Unmarshal(body, &serviceInfo); err != nil {
		return "", "", fmt.Errorf("failed to parse service response: %w", err)
	}

	// Extract hostnames from the service configuration.
	// APIM services can have multiple hostname configurations for different purposes.
	for _, cfg := range serviceInfo.Properties.HostnameConfigurations {
		switch cfg.Type {
		case "Proxy":
			// Proxy hostname is used for API gateway access.
			apiHost = cfg.HostName
		case "DeveloperPortal":
			// Developer portal hostname is used for the developer portal UI.
			developerPortalHost = cfg.HostName
		}
	}

	return apiHost, developerPortalHost, nil
}

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

func UpsertInboundPolicy(ctx context.Context, config APIMInboundPolicyConfig) error {
	return nil
}

// APIRevision represents a single API revision in Azure APIM.
// Revisions allow versioning of APIs and testing changes before making them current.
type APIRevision struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Properties struct {
		// ApiRevision is the revision number (e.g., "1", "2").
		ApiRevision string `json:"apiRevision"`
		// IsCurrent indicates whether this revision is the current active revision.
		IsCurrent bool `json:"isCurrent"`
	} `json:"properties"`
}

// APIRevisionListResponse is the response structure from the Azure Management API
// when querying for API revisions.
type APIRevisionListResponse struct {
	// Value contains the list of API revisions.
	Value []APIRevision `json:"value"`
}

// APIMDeploymentConfig contains all the configuration needed to deploy an API to Azure APIM.
// This includes Azure subscription information, API details, and optional associations.
type APIMDeploymentConfig struct {
	// SubscriptionID is the Azure subscription ID where the APIM service is located.
	SubscriptionID string
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string
	// ServiceName is the name of the Azure API Management service instance.
	ServiceName string
	// APIID is the unique identifier for the API in APIM.
	APIID string
	// RoutePrefix is the base route path in APIM (e.g., "/myapi").
	RoutePrefix string
	// Product is a legacy field for a single product association (deprecated, use ProductIDs).
	Product string
	// ServiceURL is the backend service URL that APIM will proxy requests to.
	ServiceURL string
	// BearerToken is the Azure AD authentication token for the APIM management API.
	BearerToken string
	// Revision is an optional API revision number (e.g., "2"). If specified, a new revision will be created.
	Revision string
	// ProductIDs is a list of product IDs to associate this API with in APIM.
	ProductIDs []string
	// TagIDs is a list of tag IDs to apply to this API in APIM.
	TagIDs []string
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

// APIMInboundPolicyConfig contains the configuration needed to create or update an inbound policy in Azure APIM.
// Inbound policies are used to control the inbound traffic to an API.
type APIMInboundPolicyConfig struct {
	// SubscriptionID is the Azure subscription ID where the APIM service is located.
	SubscriptionID string
	// ResourceGroup is the Azure resource group where the APIM service is located.
	ResourceGroup string
	// PolicyID is the unique identifier for the policy in APIM.
	PolicyID string
	// ServiceName is the name of the Azure API Management service instance.
	ServiceName string
	// BearerToken is the Azure AD authentication token for the APIM management API.
	BearerToken string
}
