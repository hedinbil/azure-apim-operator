package identity

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	ctrl "sigs.k8s.io/controller-runtime"
)

func GetManagementToken(ctx context.Context, clientId string, tenantId string) (string, error) {
	logger := ctrl.Log.WithName("identity")

	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID:      clientId,
		TenantID:      tenantId,
		TokenFilePath: "/var/run/secrets/azure/tokens/azure-identity-token",
	})
	if err != nil {
		logger.Error(err, "❌ Failed to create workload identity credential")
		return "", err
	}

	const scope = "https://management.azure.com/.default"
	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{scope},
	})
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure access token")
		return "", err
	}

	logger.Info("✅ Successfully acquired Azure token", "expires", token.ExpiresOn.Format(time.RFC3339))
	return token.Token, nil
}
