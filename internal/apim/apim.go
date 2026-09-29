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

// GetAPI retrieves an existing API from Azure APIM to get its etag.
// This is used to properly update existing APIs with the correct If-Match header.
func GetAPI(ctx context.Context, config APIMDeploymentConfig) (etag string, exists bool, err error) {
	url := serviceURL(config, "apis", config.APIID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, fmt.Errorf("failed to build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.BearerToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("failed to call APIM API: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body", "apiID", config.APIID)
		}
	}()

	if resp.StatusCode == 404 {
		return "", false, nil // API doesn't exist
	}

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", false, fmt.Errorf("failed to get API: %s\n%s", resp.Status, string(body))
	}

	// Get etag from response header
	// Azure APIM returns etags in format: "W/\"etag-value\"" or "\"etag-value\""
	etag = resp.Header.Get("ETag")
	if etag != "" {
		// Remove W/ prefix if present (weak etag)
		etag = strings.TrimPrefix(etag, "W/")
		// Remove quotes if present
		etag = strings.Trim(etag, "\"")
		// Remove any remaining whitespace
		etag = strings.TrimSpace(etag)
		// Format etag with quotes for use in If-Match header (Azure APIM requirement)
		etag = fmt.Sprintf(`"%s"`, etag)
	}

	return etag, true, nil
}

// ImportOpenAPIDefinitionToAPIM imports an OpenAPI/Swagger definition into Azure API Management.
// It creates or updates an API in APIM with the provided OpenAPI content, route prefix, and optional revision.
// The function uses the Azure Management API to perform the import operation.
// For updates, it properly handles the If-Match header to ensure existing APIs are updated correctly.
// An import APIM is still running comes back as a pending UpsertResult; see doAPIUpsert.
func ImportOpenAPIDefinitionToAPIM(ctx context.Context, apimParams APIMDeploymentConfig, openApiContent []byte) (UpsertResult, error) {
	etag := ifMatchForUpsert(ctx, apimParams)

	// Build the Azure Management API URL for importing the API. APIM addresses
	// a revision as "apiId;rev=n".
	importURL := revisionURL(apimParams, apimParams.APIID, apimParams.Revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, importURL, bytes.NewReader(openApiContent))
	if err != nil {
		logger.Error(err, "❌ Failed to build APIM request", "apiID", apimParams.APIID)
		return UpsertResult{}, fmt.Errorf("failed to build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/vnd.oai.openapi+json")
	req.Header.Set("Authorization", "Bearer "+apimParams.BearerToken)
	// Set If-Match header for conditional updates (etag) or unconditional updates (*)
	// GetAPI already formats the etag with quotes, so we can use it directly
	req.Header.Set("If-Match", etag)

	q := req.URL.Query()
	q.Set("import", "true")
	q.Set("path", apimParams.RoutePrefix)
	if apimParams.Revision != "" {
		q.Set("createRevision", "true")
	}
	req.URL.RawQuery = q.Encode()

	logger.Info("📤 Sending request to APIM",
		"method", req.Method,
		"url", req.URL.String(),
		"apiID", apimParams.APIID,
		"routePrefix", apimParams.RoutePrefix,
		"ifMatch", etag,
		"contentType", "application/vnd.oai.openapi+json",
	)

	logger.Info("📄 OpenAPI document ready for import", "apiID", apimParams.APIID, "bytes", len(openApiContent))

	return doAPIUpsert(apimParams, req, "imported API into")
}

// ifMatchForUpsert picks the If-Match header for a PUT on an API: the current
// etag when the API exists (a conditional update), "*" when it does not or
// when a new revision is being created.
func ifMatchForUpsert(ctx context.Context, apimParams APIMDeploymentConfig) string {
	var etag string
	if apimParams.Revision == "" {
		existingEtag, exists, err := GetAPI(ctx, apimParams)
		if err != nil {
			logger.Error(err, "⚠️ Failed to check if API exists, will use If-Match: *", "apiID", apimParams.APIID)
			etag = "*"
		} else if exists {
			if existingEtag != "" {
				// Use the actual etag for conditional update
				etag = existingEtag
				logger.Info("🔍 Found existing API, will update with etag", "apiID", apimParams.APIID, "etag", etag)
			} else {
				// Fallback to unconditional update if no etag
				etag = "*"
				logger.Info("🔍 Found existing API but no etag, using If-Match: *", "apiID", apimParams.APIID)
			}
		} else {
			// API doesn't exist, use "*" for create
			etag = "*"
			logger.Info("🆕 API does not exist, will create", "apiID", apimParams.APIID)
		}
	} else {
		// Revisions are always new, use "*"
		etag = "*"
		logger.Info("📝 Creating new revision", "apiID", apimParams.APIID, "revision", apimParams.Revision)
	}
	return etag
}

// doAPIUpsert sends a prepared PUT for an API. verb is what the success log
// says was done, e.g. "imported API into".
//
// A large import comes back 202 and keeps running in APIM for minutes. The
// operation URL is returned instead of waited on: the caller records it and
// polls it on later reconciles (GetOperationState), so no worker is blocked for
// the length of an import and the API is not written again while it runs.
func doAPIUpsert(apimParams APIMDeploymentConfig, req *http.Request, verb string) (UpsertResult, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Error(err, "❌ Failed to send request to APIM", "apiID", apimParams.APIID)
		return UpsertResult{}, fmt.Errorf("failed to call APIM API: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error(closeErr, "⚠️ Failed to close response body", "apiID", apimParams.APIID)
		}
	}()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		logger.Error(fmt.Errorf("status code: %d", resp.StatusCode), "❌ APIM API returned error", "apiID", apimParams.APIID, "status", resp.Status, "body", string(body))
		return UpsertResult{}, fmt.Errorf("APIM API failed: %s\n%s", resp.Status, string(body))
	}

	if resp.StatusCode == http.StatusAccepted {
		if operationURL := asyncOperationURL(resp); operationURL != "" {
			logger.Info("⏳ APIM accepted the request and is still processing it", "apiID", apimParams.APIID, "operationURL", operationURL)
			return UpsertResult{OperationURL: operationURL}, nil
		}
		logger.Info("ℹ️ APIM returned 202 without a usable polling URL; cannot verify completion", "apiID", apimParams.APIID)
	}

	logger.Info("✅ Successfully "+verb+" APIM",
		"apiID", apimParams.APIID,
		"status", resp.Status,
		"statusCode", resp.StatusCode,
	)

	return UpsertResult{}, nil
}

func extractAsyncStatus(body []byte) string {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	if status, ok := payload["status"].(string); ok {
		return status
	}
	if props, ok := payload["properties"].(map[string]interface{}); ok {
		if status, ok := props["provisioningState"].(string); ok {
			return status
		}
	}
	return ""
}

// AssignServiceUrlToApi updates the backend service URL for an existing API in Azure APIM.
// This is used to point an API to a different backend service without re-importing the OpenAPI definition.
func AssignServiceUrlToApi(ctx context.Context, config APIMDeploymentConfig) error {
	patchURL := serviceURL(config, "apis", config.APIID)

	// Marshalled, not formatted: a quote or backslash in serviceUrl used to
	// break out of the string and change the request body (APIM-16).
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{"serviceUrl": config.ServiceURL},
	})
	if err != nil {
		return fmt.Errorf("marshal serviceUrl patch body: %w", err)
	}

	// Log what we're about to do
	logger.Info("🔧 Patching APIM service URL",
		"method", http.MethodPatch,
		"url", patchURL,
		"apiID", config.APIID,
		"serviceUrl", config.ServiceURL,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, bytes.NewReader(body))
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
			logger.Error(closeErr, "⚠️ Failed to close response body", "apiID", config.APIID)
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

// SetSubscriptionRequired updates the subscription requirement setting for an existing API in Azure APIM.
// This controls whether a subscription key is required to access the API.
func SetSubscriptionRequired(ctx context.Context, config APIMDeploymentConfig) error {
	logger.Info("🔍 SetSubscriptionRequired called",
		"apiID", config.APIID,
		"subscriptionRequired", config.SubscriptionRequired,
	)

	patchURL := serviceURL(config, "apis", config.APIID)

	// Build the JSON body with the subscriptionRequired property.
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{"subscriptionRequired": config.SubscriptionRequired},
	})
	if err != nil {
		return fmt.Errorf("marshal subscriptionRequired patch body: %w", err)
	}

	// Log what we're about to do
	logger.Info("🔧 Patching APIM subscription requirement",
		"method", http.MethodPatch,
		"url", patchURL,
		"apiID", config.APIID,
		"subscriptionRequired", config.SubscriptionRequired,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, bytes.NewReader(body))
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
			logger.Error(closeErr, "⚠️ Failed to close response body", "apiID", config.APIID)
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
		return fmt.Errorf("subscriptionRequired patch failed: %s\n%s", resp.Status, string(respBody))
	}

	logger.Info("✅ Successfully patched subscriptionRequired",
		"apiID", config.APIID,
		"status", resp.Status,
		"subscriptionRequired", config.SubscriptionRequired,
	)

	return nil
}

// GetAPIMServiceDetails retrieves hostname information for an Azure APIM service instance.
// It returns the API gateway hostname (Proxy) and the developer portal hostname.
// This information is used to construct full URLs for accessing APIs through APIM.
func GetAPIMServiceDetails(ctx context.Context, config APIMDeploymentConfig) (apiHost, developerPortalHost string, err error) {
	url := serviceURL(config)

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

// APIMDeploymentConfig contains all the configuration needed to deploy an API to Azure APIM.
// This includes Azure subscription information, API details, and optional associations.
type APIMDeploymentConfig struct {
	// Type is the APIM API type, "http" or "websocket". Empty means http.
	Type string
	// DisplayName is the display name for a websocket API. Empty means APIID.
	DisplayName string
	// Protocols are the gateway protocols for a websocket API. Empty means ["wss"].
	Protocols []string
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
	// SubscriptionRequired controls whether a subscription key is required to access the API.
	// Defaults to true (subscription required). If set to false, subscription is disabled.
	SubscriptionRequired bool
}
