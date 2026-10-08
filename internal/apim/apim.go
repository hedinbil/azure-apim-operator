// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// These functions handle importing APIs, updating service URLs, assigning products and tags,
// and retrieving API and service information from Azure APIM.
package apim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"
)

// logger is the logger instance for APIM operations.
var logger = ctrl.Log.WithName("apim")

// serviceURLProperty is the API property that holds the backend address.
const serviceURLProperty = "serviceUrl"

// GetAPI retrieves an existing API from Azure APIM to get its etag.
// This is used to properly update existing APIs with the correct If-Match header.
func GetAPI(ctx context.Context, config APIMDeploymentConfig) (etag string, exists bool, err error) {
	resp, err := armRequest{
		operation: "get API",
		method:    http.MethodGet,
		url:       serviceURL(config, "apis", config.APIID),
		token:     config.BearerToken,
	}.send(ctx)
	if IsNotFound(err) {
		return "", false, nil // API doesn't exist
	}
	if err != nil {
		return "", false, err
	}

	// Get etag from response header
	// Azure APIM returns etags in format: "W/\"etag-value\"" or "\"etag-value\""
	etag = resp.header.Get("ETag")
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
//
// The document goes inside a JSON envelope together with the backend serviceUrl. Sent as a
// bare document, APIM takes the backend from the document's own servers (or host/basePath)
// field, which many frameworks fill with the host the document was fetched from: the
// in-cluster address the operator used. The API then pointed at that address, or at nothing,
// until the later serviceUrl PATCH, and kept doing so while that PATCH was backing off.
func ImportOpenAPIDefinitionToAPIM(ctx context.Context, apimParams APIMDeploymentConfig, openApiContent []byte) error {
	body, format, err := importEnvelope(apimParams, openApiContent)
	if err != nil {
		logger.Error(err, "❌ Failed to build APIM import body", "apiID", apimParams.APIID)
		return err
	}

	etag := ifMatchForUpsert(ctx, apimParams)

	// Build the Azure Management API URL for importing the API. APIM addresses
	// a revision as "apiId;rev=n".
	importURL, err := url.Parse(revisionURL(apimParams, apimParams.APIID, apimParams.Revision))
	if err != nil {
		logger.Error(err, "❌ Failed to build APIM request", "apiID", apimParams.APIID)
		return fmt.Errorf("failed to build request: %w", err)
	}
	if apimParams.Revision != "" {
		q := importURL.Query()
		q.Set("createRevision", "true")
		importURL.RawQuery = q.Encode()
	}

	logger.Info("📤 Sending request to APIM",
		"method", http.MethodPut,
		"url", importURL.String(),
		"apiID", apimParams.APIID,
		"routePrefix", apimParams.RoutePrefix,
		"serviceUrl", apimParams.ServiceURL,
		"format", format,
		"ifMatch", etag,
	)

	logger.Info("📄 OpenAPI document ready for import", "apiID", apimParams.APIID, "bytes", len(openApiContent))

	return doAPIUpsert(ctx, apimParams, armRequest{
		operation: "import API",
		method:    http.MethodPut,
		url:       importURL.String(),
		token:     apimParams.BearerToken,
		body:      body,
		// Set If-Match header for conditional updates (etag) or unconditional updates (*)
		// GetAPI already formats the etag with quotes, so we can use it directly
		contentType: contentTypeJSON,
		ifMatch:     etag,
	}, "imported API into")
}

// importEnvelope wraps an OpenAPI or Swagger document in the ARM body that imports it and
// sets the API's path, backend serviceUrl and subscription requirement in the same write.
// It returns the body and the import format it chose.
func importEnvelope(apimParams APIMDeploymentConfig, openApiContent []byte) ([]byte, string, error) {
	format, doc, err := importFormat(openApiContent)
	if err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{
			"format":               format,
			"value":                string(doc),
			"path":                 apimParams.RoutePrefix,
			serviceURLProperty:     apimParams.ServiceURL,
			"subscriptionRequired": apimParams.SubscriptionRequired,
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("marshal import body: %w", err)
	}
	return body, format, nil
}

// importFormat returns APIM's import format for a document and the document as JSON:
// "swagger-json" for Swagger 2.0, otherwise "openapi+json". A JSON document is passed
// through byte for byte; a YAML one is converted, since APIM has no Swagger-YAML format and
// one JSON path keeps the two versions alike. The fetcher has already checked that the
// document declares openapi or swagger; anything else is left to APIM to reject (400,
// which the controllers treat as Invalid).
func importFormat(openApiContent []byte) (string, []byte, error) {
	doc := openApiContent
	if !json.Valid(doc) {
		converted, err := yaml.YAMLToJSON(openApiContent)
		if err != nil {
			return "", nil, fmt.Errorf("OpenAPI document is neither JSON nor YAML: %w", err)
		}
		doc = converted
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return "", nil, fmt.Errorf("OpenAPI document is not a JSON object: %w", err)
	}
	if _, ok := top["swagger"]; ok {
		return "swagger-json", doc, nil
	}
	return "openapi+json", doc, nil
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

// doAPIUpsert sends a prepared PUT for an API and waits for APIM to finish
// it, including the asynchronous (202) case. verb is what the success log
// says was done, e.g. "imported API into".
func doAPIUpsert(ctx context.Context, apimParams APIMDeploymentConfig, request armRequest, verb string) error {
	resp, err := request.send(ctx)
	if err != nil {
		logger.Error(err, "❌ APIM API returned error", "apiID", apimParams.APIID)
		return err
	}

	// Azure APIM may return 202 (Accepted) for asynchronous import operations.
	// Poll completion explicitly so we don't report success while the import later fails.
	if resp.statusCode == http.StatusAccepted {
		if err := waitForAsyncImportCompletion(ctx, apimParams.BearerToken, apimParams.APIID, request.operation, resp); err != nil {
			logger.Error(err, "❌ APIM async import did not complete successfully", "apiID", apimParams.APIID)
			return err
		}
	}

	logger.Info("✅ Successfully "+verb+" APIM",
		"apiID", apimParams.APIID,
		"status", resp.status,
		"statusCode", resp.statusCode,
	)

	return nil
}

// AssignServiceUrlToApi updates the backend service URL for an existing API in Azure APIM.
// This is used to point an API to a different backend service without re-importing the OpenAPI definition.
func AssignServiceUrlToApi(ctx context.Context, config APIMDeploymentConfig) error {
	patchURL := serviceURL(config, "apis", config.APIID)

	// Marshalled, not formatted: a quote or backslash in serviceUrl used to
	// break out of the string and change the request body (APIM-16).
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{serviceURLProperty: config.ServiceURL},
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

	resp, err := armRequest{
		operation:   "patch serviceUrl",
		method:      http.MethodPatch,
		url:         patchURL,
		token:       config.BearerToken,
		body:        body,
		contentType: contentTypeJSON,
		dependent:   true,
	}.send(ctx)
	if err != nil {
		logger.Error(err, "❌ PATCH returned error", "apiID", config.APIID)
		return err
	}

	logger.Info("✅ Successfully patched serviceUrl",
		"apiID", config.APIID,
		"status", resp.status,
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

	resp, err := armRequest{
		operation:   "patch subscriptionRequired",
		method:      http.MethodPatch,
		url:         patchURL,
		token:       config.BearerToken,
		body:        body,
		contentType: contentTypeJSON,
		dependent:   true,
	}.send(ctx)
	if err != nil {
		logger.Error(err, "❌ PATCH returned error", "apiID", config.APIID)
		return err
	}

	logger.Info("✅ Successfully patched subscriptionRequired",
		"apiID", config.APIID,
		"status", resp.status,
		"subscriptionRequired", config.SubscriptionRequired,
	)

	return nil
}

// GetAPIMServiceDetails retrieves hostname information for an Azure APIM service instance.
// It returns the API gateway hostname (Proxy) and the developer portal hostname.
// This information is used to construct full URLs for accessing APIs through APIM.
func GetAPIMServiceDetails(ctx context.Context, config APIMDeploymentConfig) (apiHost, developerPortalHost string, err error) {
	resp, err := armRequest{
		operation: "get APIM service details",
		method:    http.MethodGet,
		url:       serviceURL(config),
		token:     config.BearerToken,
	}.send(ctx)
	if err != nil {
		return "", "", err
	}
	body := resp.body

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
