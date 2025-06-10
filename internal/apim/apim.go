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

var logger = ctrl.Log.WithName("apim")

func ImportOpenAPIDefinitionToAPIM(ctx context.Context, apimParams APIMDeploymentConfig, openApiContent []byte) error {
	apiID := apimParams.APIID
	if apimParams.Revision != "" {
		apiID = fmt.Sprintf("%s;rev=%s", apimParams.APIID, apimParams.Revision)
	}

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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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

func AssignProductToAPI(ctx context.Context, config APIMDeploymentConfig) error {
	if len(config.ProductIDs) == 0 {
		logger.Info("ℹ️ No products configured for assignment; skipping")
		return nil
	}

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
		defer resp.Body.Close()

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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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

	for _, cfg := range serviceInfo.Properties.HostnameConfigurations {
		switch cfg.Type {
		case "Proxy":
			apiHost = cfg.HostName
		case "DeveloperPortal":
			developerPortalHost = cfg.HostName
		}
	}

	return apiHost, developerPortalHost, nil
}

func UpsertProduct(ctx context.Context, config APIMProductConfig) error {
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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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

type APIRevision struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Properties struct {
		ApiRevision string `json:"apiRevision"`
		IsCurrent   bool   `json:"isCurrent"`
	} `json:"properties"`
}

type APIRevisionListResponse struct {
	Value []APIRevision `json:"value"`
}

type APIMDeploymentConfig struct {
	SubscriptionID string
	ResourceGroup  string
	ServiceName    string
	APIID          string   // unique identifier for the API in APIM
	RoutePrefix    string   // base route in APIM (e.g. /bidme)
	Product        string   // e.g. "my-product" → optional
	ServiceURL     string   // Backend URL (e.g. https://myapp.example.com)
	BearerToken    string   // AAD token for the APIM management scope
	Revision       string   // e.g. "2" → optional
	ProductIDs     []string // e.g. "my-product" → optional
}

type APIMProductConfig struct {
	SubscriptionID string // Azure subscription
	ResourceGroup  string // Resource group where APIM lives
	ServiceName    string // APIM instance name
	ProductID      string // Unique product ID in APIM
	DisplayName    string // UI display name
	Description    string // Optional product description
	BearerToken    string // Authorization token
	Published      bool   // Whether product should be published
}

type APIMTagConfig struct {
	SubscriptionID string
	ResourceGroup  string
	ServiceName    string
	BearerToken    string
	TagID          string
	DisplayName    string
}
