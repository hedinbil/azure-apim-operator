package identity

import (
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// GetBearerToken fetches a token for ARM from default Azure credentials
func GetBearerToken() (string, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return "", err
	}

	token, err := cred.GetToken(ctx, azidentity.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}

	fmt.Printf("✅ Got token of length: %d\n", len(token.Token))

	return token.Token, nil
}
