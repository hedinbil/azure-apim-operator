// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// These functions handle importing APIs, updating service URLs, assigning products and tags,
// and retrieving API and service information from Azure APIM.
package apim

import (
	"bytes"
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

// ImportOpenAPIDefinitionToAPIM starts the import of an OpenAPI/Swagger definition into
// Azure API Management: it creates or updates the API with the document, route prefix and
// optional revision. It does not wait for APIM to finish. A WriteResult that is Accepted
// carries the operation APIM is still running; the caller records it and reads it later
// (see GetOperationState) and must not write the API again until it has ended.
//
// The document goes inside a JSON envelope together with the backend serviceUrl and the
// subscription requirement. Sent as a bare document, APIM takes the backend from the
// document's own servers (or host/basePath) field, which many frameworks fill with the host
// the document was fetched from: the in-cluster address the operator used.
func ImportOpenAPIDefinitionToAPIM(ctx context.Context, apimParams APIMDeploymentConfig, openApiContent []byte) (WriteResult, error) {
	body, format, err := importEnvelope(apimParams, openApiContent)
	if err != nil {
		logger.Error(err, "❌ Failed to build APIM import body", "apiID", apimParams.APIID)
		return WriteResult{}, err
	}

	etag, err := ifMatchForUpsert(ctx, apimParams)
	if err != nil {
		return WriteResult{}, err
	}

	// Build the Azure Management API URL for importing the API. APIM addresses
	// a revision as "apiId;rev=n".
	importURL, err := url.Parse(revisionURL(apimParams, apimParams.APIID, apimParams.Revision))
	if err != nil {
		logger.Error(err, "❌ Failed to build APIM request", "apiID", apimParams.APIID)
		return WriteResult{}, fmt.Errorf("failed to build request: %w", err)
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
		"serviceUrl", RedactURL(apimParams.ServiceURL),
		"format", format,
		"ifMatch", etag,
		"bytes", len(openApiContent),
	)

	return startAPIWrite(ctx, apimParams, armRequest{
		operation:   "import API",
		method:      http.MethodPut,
		url:         importURL.String(),
		token:       apimParams.BearerToken,
		body:        body,
		contentType: contentTypeJSON,
		ifMatch:     etag,
	})
}

// importEnvelope wraps an OpenAPI or Swagger document in the ARM body that imports it and
// sets the API's path, backend serviceUrl and subscription requirement in the same write.
// It returns the body and the import format it chose.
func importEnvelope(apimParams APIMDeploymentConfig, openApiContent []byte) ([]byte, string, error) {
	format, doc, err := importFormat(openApiContent)
	if err != nil {
		return nil, "", err
	}
	// An Encoder without HTML escaping: json.Marshal would turn every <, > and & of the
	// document into a six-byte escape, growing a large document by half for nothing.
	var body bytes.Buffer
	body.Grow(len(doc) + 512)
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{
		"properties": map[string]any{
			"format":               format,
			"value":                string(doc),
			"path":                 apimParams.RoutePrefix,
			serviceURLProperty:     apimParams.ServiceURL,
			"subscriptionRequired": apimParams.SubscriptionRequired,
		},
	}); err != nil {
		return nil, "", fmt.Errorf("marshal import body: %w", err)
	}
	return bytes.TrimSuffix(body.Bytes(), []byte("\n")), format, nil
}

// maxSwaggerYAMLConversion bounds the Swagger 2.0 YAML documents converted to JSON. The
// conversion builds the whole document in memory at roughly seventy times its size.
const maxSwaggerYAMLConversion = 2 << 20

// importFormat returns APIM's import format for a document and the document to send:
//   - JSON: "swagger-json" for Swagger 2.0, otherwise "openapi+json", byte for byte.
//   - YAML OpenAPI 3: "openapi", byte for byte. Converting it would apply YAML 1.1 rules and
//     change the definition (version 1.0 becomes the number 1, an enum value yes becomes true).
//   - YAML Swagger 2.0: converted to JSON as "swagger-json", since APIM has no Swagger-YAML
//     format. That conversion has the YAML 1.1 caveat above.
//
// The fetcher has already checked that the document declares openapi or swagger; anything
// else is left to APIM to reject (400, which the controllers treat as Invalid).
func importFormat(openApiContent []byte) (string, []byte, error) {
	info, err := inspectDocument(openApiContent)
	if err != nil {
		return "", nil, fmt.Errorf("OpenAPI document: %w", err)
	}
	switch {
	case info.json && info.version == versionSwagger:
		return "swagger-json", openApiContent, nil
	case info.json:
		return "openapi+json", openApiContent, nil
	case info.version != versionSwagger:
		return "openapi", openApiContent, nil
	case len(openApiContent) > maxSwaggerYAMLConversion:
		return "", nil, fmt.Errorf("swagger 2.0 YAML of %d bytes is too large to convert to JSON (at most %d); serve the document as JSON: %w",
			len(openApiContent), maxSwaggerYAMLConversion, ErrUnsupportedDocument)
	}
	converted, err := yaml.YAMLToJSON(openApiContent)
	if err != nil {
		return "", nil, fmt.Errorf("convert Swagger 2.0 YAML to JSON: %w", err)
	}
	return "swagger-json", converted, nil
}

// ifMatchForUpsert picks the If-Match header for a PUT on an API: the current etag when the
// API exists (a conditional update), "*" when it does not or when a new revision is being
// created. Any failure of the existence check other than a 404 is returned: an APIM that
// cannot answer a GET is not sent the heaviest write it has.
func ifMatchForUpsert(ctx context.Context, apimParams APIMDeploymentConfig) (string, error) {
	if apimParams.Revision != "" {
		logger.Info("📝 Creating new revision", "apiID", apimParams.APIID, "revision", apimParams.Revision)
		return "*", nil
	}
	existingEtag, exists, err := GetAPI(ctx, apimParams)
	switch {
	case err != nil:
		logger.Error(err, "❌ Failed to check whether the API exists", "apiID", apimParams.APIID)
		return "", err
	case !exists:
		logger.Info("🆕 API does not exist, will create", "apiID", apimParams.APIID)
		return "*", nil
	case existingEtag == "":
		logger.Info("🔍 Found existing API but no etag, using If-Match: *", "apiID", apimParams.APIID)
		return "*", nil
	default:
		logger.Info("🔍 Found existing API, will update with etag", "apiID", apimParams.APIID, "etag", existingEtag)
		return existingEtag, nil
	}
}

// startAPIWrite sends a prepared PUT for an API. A 200 or 201 means APIM finished the write;
// a 202 means it accepted it and runs it in the background, and the WriteResult carries the
// operation to follow. It never waits.
//
// The PUT runs on a context detached from ctx, bounded by armRequestTimeout: once sent, it
// is allowed to come back with APIM's answer even when the reconcile is cancelled (the
// operator shutting down, the reconcile timing out). Cut off, it would leave APIM running
// an import nobody recorded, and the next attempt would import on top of it.
func startAPIWrite(ctx context.Context, apimParams APIMDeploymentConfig, request armRequest) (WriteResult, error) {
	// A reconcile already cancelled sends nothing; only a write already on its way is let finish.
	if err := ctx.Err(); err != nil {
		return WriteResult{}, fmt.Errorf("%s: %w", request.operation, err)
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), armRequestTimeout)
	defer cancel()
	resp, err := request.send(writeCtx)
	if err != nil {
		logger.Error(err, "❌ APIM API returned error", "apiID", apimParams.APIID, "operation", request.operation)
		if writeOutcomeUnknown(err) {
			return WriteResult{}, fmt.Errorf("%w: %w", ErrWriteOutcomeUnknown, err)
		}
		return WriteResult{}, err
	}
	if resp.statusCode != http.StatusAccepted {
		logger.Info("✅ APIM finished the write", "apiID", apimParams.APIID, "operation", request.operation, "statusCode", resp.statusCode)
		return WriteResult{}, nil
	}
	result, err := acceptedWrite(request.operation, request.method, resp.header)
	if err != nil {
		logger.Error(err, "❌ APIM accepted the write without an operation to follow", "apiID", apimParams.APIID)
		return WriteResult{}, err
	}
	logger.Info("⏳ APIM accepted the write and runs it in the background", "apiID", apimParams.APIID,
		"operation", request.operation, "operationURL", result.OperationURL)
	return result, nil
}

// RedactURL drops credentials and the query string from a URL before it is logged or
// written to a status: a backend or OpenAPI URL can carry a function key or basic-auth
// credentials.
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<unparseable URL>"
	}
	parsed.User = nil
	if parsed.RawQuery != "" {
		parsed.RawQuery = "<redacted>"
	}
	return parsed.String()
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
