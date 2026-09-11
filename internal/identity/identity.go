// Package identity obtains Azure authentication tokens through Azure Workload
// Identity, the only method the operator uses. Two further helpers that
// discovered the client id from the pod's ServiceAccount, or fell back to
// DefaultAzureCredential, had no callers and were removed (APIM-17); the
// fallback chain in docs/authentication.md described them, not the code.
package identity

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	ctrl "sigs.k8s.io/controller-runtime"
)

// GetManagementToken obtains an Azure AD access token for the Azure Management API
// using Azure Workload Identity. This method requires the client ID and tenant ID
// to be provided, and reads the service account token from the standard Kubernetes
// service account token path.
//
// This is the primary authentication method used in Kubernetes environments with
// workload identity configured.
func GetManagementToken(ctx context.Context, clientId string, tenantId string) (string, error) {
	logger := ctrl.Log.WithName("identity")

	// Create a workload identity credential using the provided client ID and tenant ID.
	// The token file path is the standard location where Kubernetes injects the
	// service account token for workload identity authentication.
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID:      clientId,
		TenantID:      tenantId,
		TokenFilePath: "/var/run/secrets/azure/tokens/azure-identity-token",
	})
	if err != nil {
		logger.Error(err, "❌ Failed to create workload identity credential")
		return "", err
	}

	// Request a token with the Azure Management API scope.
	// This scope provides access to Azure Resource Manager APIs.
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
