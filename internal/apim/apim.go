package apim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

func ImportSwaggerToAPIM(ctx context.Context, apimParams APIMConfig, swaggerYAML []byte) error {
	importURL := fmt.Sprintf("https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ApiManagement/service/%s/apis/%s?api-version=2021-08-01",
		apimParams.SubscriptionID,
		apimParams.ResourceGroup,
		apimParams.ServiceName,
		apimParams.APIID,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, importURL, bytes.NewReader(swaggerYAML))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/vnd.oai.openapi.components+yaml") // for OpenAPI v3 YAML
	req.Header.Set("Authorization", "Bearer "+apimParams.BearerToken)

	// Optional query params to control import behavior
	q := req.URL.Query()
	q.Set("import", "true")
	q.Set("path", apimParams.RoutePrefix) // e.g. "/my-api"
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call APIM API: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("APIM API failed: %s\n%s", resp.Status, string(body))
	}

	fmt.Printf("API imported successfully into APIM: %s\n", apimParams.APIID)
	return nil
}

type APIMConfig struct {
	SubscriptionID string
	ResourceGroup  string
	ServiceName    string
	APIID          string // unique identifier for the API in APIM
	RoutePrefix    string // base route in APIM (e.g. /bidme)
	BearerToken    string // AAD token for the APIM management scope
}
