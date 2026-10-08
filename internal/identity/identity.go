// Package identity obtains Azure authentication tokens through Azure Workload
// Identity, the only method the operator uses. Two further helpers that
// discovered the client id from the pod's ServiceAccount, or fell back to
// DefaultAzureCredential, had no callers and were removed (APIM-17); the
// fallback chain in docs/authentication.md described them, not the code.
package identity

import (
	"context"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	ctrl "sigs.k8s.io/controller-runtime"
)

// tokenFilePath is where Kubernetes projects the service account token that workload
// identity exchanges for an Azure AD token.
const tokenFilePath = "/var/run/secrets/azure/tokens/azure-identity-token"

// managementScope is the Azure Resource Manager scope.
const managementScope = "https://management.azure.com/.default"

// credentialKey identifies one workload identity.
type credentialKey struct{ clientID, tenantID string }

// credentials holds one credential per identity for the life of the process. A credential
// caches the token it obtained and renews it shortly before it expires; building a new one
// per call, as this package used to, exchanged the service account token with Azure AD on
// every reconcile of every resource.
var (
	credentialsMu sync.Mutex
	credentials   = map[credentialKey]*azidentity.WorkloadIdentityCredential{}
)

// credential returns the cached credential for clientID and tenantID, creating it on first use.
func credential(clientID, tenantID string) (*azidentity.WorkloadIdentityCredential, error) {
	key := credentialKey{clientID, tenantID}
	credentialsMu.Lock()
	defer credentialsMu.Unlock()
	if cred, ok := credentials[key]; ok {
		return cred, nil
	}
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID:      clientID,
		TenantID:      tenantID,
		TokenFilePath: tokenFilePath,
	})
	if err != nil {
		return nil, err
	}
	credentials[key] = cred
	return cred, nil
}

// GetManagementToken obtains an Azure AD access token for Azure Resource Manager through
// Azure Workload Identity. The token comes from the credential's cache while it is valid,
// so calling this on every reconcile costs a round trip to Azure AD only when the token is
// due for renewal.
func GetManagementToken(ctx context.Context, clientID string, tenantID string) (string, error) {
	cred, err := credential(clientID, tenantID)
	if err != nil {
		ctrl.Log.WithName("identity").Error(err, "❌ Failed to create workload identity credential")
		return "", err
	}
	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{managementScope}})
	if err != nil {
		ctrl.Log.WithName("identity").Error(err, "❌ Failed to get Azure access token")
		return "", err
	}
	return token.Token, nil
}
