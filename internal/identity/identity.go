package identity

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	ctrl "sigs.k8s.io/controller-runtime"
)

func GetManagementToken(ctx context.Context, kubeClient client.Client, namespace string, serviceAccountName string, tenantID string) (string, error) {
	logger := ctrl.Log.WithName("identity")

	// Read the ServiceAccount to extract the workload identity client ID annotation
	// var sa corev1.ServiceAccount
	// if err := kubeClient.Get(ctx, types.NamespacedName{
	// 	Name:      serviceAccountName,
	// 	Namespace: namespace,
	// }, &sa); err != nil {
	// 	logger.Error(err, "❌ Failed to get ServiceAccount")
	// 	return "", err
	// }

	// clientID, ok := sa.Annotations["azure.workload.identity/client-id"]
	// if !ok || clientID == "" {
	// 	err := fmt.Errorf("service account missing 'azure.workload.identity/client-id' annotation")
	// 	logger.Error(err, "❌ Missing client ID annotation on service account")
	// 	return "", err
	// }

	// Build the WorkloadIdentityCredential manually
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID:      "d7e57310-e862-41e6-9a55-7c492327a69b",
		TenantID:      tenantID,
		TokenFilePath: "/var/run/secrets/azure/tokens/azure-identity-token",
	})
	if err != nil {
		logger.Error(err, "❌ Failed to create workload identity credential")
		return "", err
	}

	// cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
	// 	ClientID: "d7e57310-e862-41e6-9a55-7c492327a69b",
	// })
	// if err != nil {
	// 	logger.Error(err, "failed to create workload identity credential")
	// 	return "", fmt.Errorf("failed to create credential: %w", err)
	// }

	// Acquire token
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
