package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// GetManagementToken returns a Bearer token for Azure Resource Manager
func GetManagementToken(ctx context.Context) (string, error) {
	logger := logf.FromContext(ctx)

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		logger.Error(err, "❌ Failed to create Azure credential")
		return "", fmt.Errorf("failed to create Azure credential: %w", err)
	}

	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		return "", fmt.Errorf("failed to get Azure token: %w", err)
	}

	logger.Info("✅ Successfully acquired Azure token", "expires", token.ExpiresOn.Format(time.RFC3339), "length", len(token.Token))

	return token.Token, nil
}
