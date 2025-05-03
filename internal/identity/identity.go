package identity

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// GetBearerToken fetches a token for ARM from default Azure credentials
func GetBearerToken() (string, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return "", err
	}

	token, err := cred.GetToken(context.Background(), azidentity.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", err
	}

	return token.Token, nil
}
