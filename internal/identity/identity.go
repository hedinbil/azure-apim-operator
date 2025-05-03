package identity

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	ctrl "sigs.k8s.io/controller-runtime"
)

func GetManagementToken(ctx context.Context) (string, error) {
	logger := ctrl.Log.WithName("identity")

	// This will automatically use the token file and federated identity via service account annotation
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		// These paths are set by Azure workload identity in the pod
		// These defaults will usually work as-is in AKS with workload identity
		ClientID: os.Getenv("AZURE_CLIENT_ID"), // Optional — you can leave this out if your pod only has one UAMI
	})
	if err != nil {
		logger.Error(err, "❌ Failed to create workload identity credential")
		return "", fmt.Errorf("failed to create workload identity credential: %w", err)
	}

	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		logger.Error(err, "❌ Failed to get Azure token")
		return "", fmt.Errorf("failed to get Azure token: %w", err)
	}

	logger.Info("✅ Successfully acquired Azure token", "expires", token.ExpiresOn.Format(time.RFC3339))
	return token.Token, nil
}
