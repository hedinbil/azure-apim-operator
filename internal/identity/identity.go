// Package identity provides functions for obtaining Azure authentication tokens
// using various authentication methods, including workload identity and default credentials.
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

// GetManagementToken2 obtains an Azure AD access token by dynamically discovering
// the workload identity client ID from the Kubernetes ServiceAccount annotation.
// This method reads the current pod's service account and extracts the client ID
// from the "azure.workload.identity/client-id" annotation.
//
// This is an alternative to GetManagementToken that doesn't require the client ID
// to be passed as a parameter, but requires Kubernetes API access to read the ServiceAccount.
func GetManagementToken2(ctx context.Context, kubeClient client.Client) (string, error) {
	logger := ctrl.Log.WithName("identity")

	// Step 1: Get current pod and namespace from environment.
	// Kubernetes sets HOSTNAME to the pod name, and the namespace is available
	// in the service account mount.
	podName := os.Getenv("HOSTNAME") // Kubernetes sets this to the pod name
	namespaceBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", fmt.Errorf("failed to read pod namespace: %w", err)
	}
	namespace := string(namespaceBytes)

	// Step 2: Get Pod to find the ServiceAccount name.
	// The pod's service account name is needed to look up the ServiceAccount resource.
	var pod corev1.Pod
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return "", fmt.Errorf("failed to get pod %s/%s: %w", namespace, podName, err)
	}

	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}

	// Step 3: Get the ServiceAccount to extract the annotation.
	// The workload identity client ID is stored in the ServiceAccount annotation.
	var sa corev1.ServiceAccount
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: namespace}, &sa); err != nil {
		return "", fmt.Errorf("failed to get service account %s/%s: %w", namespace, saName, err)
	}

	clientID, ok := sa.Annotations["azure.workload.identity/client-id"]
	if !ok || clientID == "" {
		return "", fmt.Errorf("client ID annotation not found on service account %s/%s", namespace, saName)
	}

	// Step 4: Create credential using the client ID discovered from the ServiceAccount.
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID: clientID,
	})
	if err != nil {
		logger.Error(err, "failed to create workload identity credential")
		return "", fmt.Errorf("failed to create credential: %w", err)
	}

	// Step 5: Get token with Azure Management API scope.
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

// GetManagementToken3 obtains an Azure AD access token using DefaultAzureCredential.
// DefaultAzureCredential tries multiple authentication methods in order:
// 1. Environment variables (AZURE_CLIENT_ID, AZURE_CLIENT_SECRET, etc.)
// 2. Managed Identity (when running on Azure)
// 3. Azure CLI (when running locally with az login)
// 4. Other credential types
//
// This method is useful for local development and Azure-hosted environments
// where managed identity is available.
func GetManagementToken3(ctx context.Context) (string, error) {
	logger := ctrl.Log.WithName("identity")

	// Create a default Azure credential that will try multiple authentication methods.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		logger.Error(err, "❌ Failed to create default Azure credential")
		return "", err
	}

	// Request a token with the Azure Management API scope.
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
