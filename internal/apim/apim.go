package apim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
)

var logger = ctrl.Log.WithName("apim")

func ImportSwaggerToAPIM(ctx context.Context, apimParams APIMRevisionConfig, swaggerYAML []byte) error {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, importURL, bytes.NewReader(swaggerYAML))
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
	snippet := string(swaggerYAML)
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

func PatchServiceURL(ctx context.Context, config APIMRevisionConfig) error {
	patchURL := fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s?api-version=2021-08-01",
		config.SubscriptionID,
		config.ResourceGroup,
		config.ServiceName,
		config.APIID,
	)

	body := fmt.Sprintf(`{"properties":{"serviceUrl":"%s"}}`, config.ServiceURL)

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

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("serviceUrl patch failed: %s\n%s", resp.Status, string(respBody))
	}

	return nil
}

type APIMRevisionConfig struct {
	SubscriptionID string
	ResourceGroup  string
	ServiceName    string
	APIID          string // unique identifier for the API in APIM
	RoutePrefix    string // base route in APIM (e.g. /bidme)
	ServiceURL     string // Backend URL (e.g. https://myapp.example.com)
	BearerToken    string // AAD token for the APIM management scope
	Revision       string // e.g. "2" → optional
}
