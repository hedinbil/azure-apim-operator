package identity

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func GetManagementToken2(ctx context.Context, kubeClient client.Client) (string, error) {
	logger := ctrl.Log.WithName("identity")

	// Step 1: Get current pod and namespace from environment
	podName := os.Getenv("HOSTNAME") // Kubernetes sets this to the pod name
	namespaceBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", fmt.Errorf("failed to read pod namespace: %w", err)
	}
	namespace := string(namespaceBytes)

	// Step 2: Get Pod to find the ServiceAccount name
	var pod corev1.Pod
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return "", fmt.Errorf("failed to get pod %s/%s: %w", namespace, podName, err)
	}

	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}

	// Step 3: Get the ServiceAccount to extract the annotation
	var sa corev1.ServiceAccount
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: namespace}, &sa); err != nil {
		return "", fmt.Errorf("failed to get service account %s/%s: %w", namespace, saName, err)
	}

	clientID, ok := sa.Annotations["azure.workload.identity/client-id"]
	if !ok || clientID == "" {
		return "", fmt.Errorf("client ID annotation not found on service account %s/%s", namespace, saName)
	}

	// Step 4: Create credential using the client ID
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID: clientID,
	})
	if err != nil {
		logger.Error(err, "failed to create workload identity credential")
		return "", fmt.Errorf("failed to create credential: %w", err)
	}

	// Step 5: Get token
	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		logger.Error(err, "failed to get token")
		return "", fmt.Errorf("failed to get token: %w", err)
	}

	logger.Info("✅ Successfully acquired Azure token", "expires", token.ExpiresOn.Format(time.RFC3339))
	return token.Token, nil
}

func GetManagementToken3(ctx context.Context) (string, error) {
	logger := ctrl.Log.WithName("identity")

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		logger.Error(err, "❌ Failed to create default Azure credential")
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
