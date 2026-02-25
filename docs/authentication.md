# Authentication

The operator authenticates to Azure using Azure AD tokens to call the Azure Management REST API. This document covers the supported authentication methods and how to configure them.

## Authentication Methods

The operator supports three authentication methods, tried in this order:

1. **Workload Identity with explicit credentials** (primary, recommended for production)
2. **Workload Identity with ServiceAccount discovery** (alternative)
3. **DefaultAzureCredential** (fallback, useful for local development)

### Method 1: Workload Identity (Primary)

This is the recommended method for production Kubernetes environments. The operator reads `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` from environment variables and exchanges a Kubernetes ServiceAccount token for an Azure AD token.

**Requirements:**

- Azure Workload Identity must be configured on the AKS cluster
- A User-Assigned Managed Identity (UAMI) must be created and federated with the operator's ServiceAccount
- The UAMI must have the required Azure RBAC permissions on the APIM instance

**Token file path:** `/var/run/secrets/azure/tokens/azure-identity-token`

**Token scope:** `https://management.azure.com/.default`

### Method 2: Workload Identity with ServiceAccount Discovery

An alternative that discovers the client ID from the ServiceAccount annotation `azure.workload.identity/client-id` instead of requiring it as an environment variable. This method reads the pod's ServiceAccount dynamically via the Kubernetes API.

This is useful when the client ID varies per namespace or per ServiceAccount.

### Method 3: DefaultAzureCredential (Fallback)

Uses the Azure SDK's `DefaultAzureCredential` which tries multiple authentication methods in order:

1. Environment variables (`AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, `AZURE_TENANT_ID`)
2. Managed Identity (when running on Azure VMs or App Service)
3. Azure CLI (`az login`)

This is primarily useful for **local development** when running the operator outside of Kubernetes.

## Azure RBAC Permissions

The managed identity used by the operator needs permissions to manage resources in your Azure APIM instance. The minimum required role assignment:

| Scope | Role | Purpose |
|-------|------|---------|
| APIM service instance | **API Management Service Contributor** | Import APIs, manage products, tags, and policies |

To assign the role:

```bash
az role assignment create \
  --assignee <uami-client-id> \
  --role "API Management Service Contributor" \
  --scope /subscriptions/<sub-id>/resourceGroups/<rg>/providers/Microsoft.ApiManagement/service/<apim-name>
```

For more restrictive permissions, you can create a custom role with only the specific actions the operator performs:

- `Microsoft.ApiManagement/service/apis/read`
- `Microsoft.ApiManagement/service/apis/write`
- `Microsoft.ApiManagement/service/apis/delete`
- `Microsoft.ApiManagement/service/apis/operations/policies/write`
- `Microsoft.ApiManagement/service/products/write`
- `Microsoft.ApiManagement/service/products/delete`
- `Microsoft.ApiManagement/service/products/apis/write`
- `Microsoft.ApiManagement/service/tags/write`
- `Microsoft.ApiManagement/service/apis/tags/write`
- `Microsoft.ApiManagement/service/read`

## Workload Identity Setup

### 1. Create a User-Assigned Managed Identity

```bash
az identity create \
  --name apim-operator-identity \
  --resource-group <your-resource-group>
```

Note the `clientId` and `tenantId` from the output.

### 2. Create the Federated Credential

```bash
az identity federated-credential create \
  --name apim-operator-federation \
  --identity-name apim-operator-identity \
  --resource-group <your-resource-group> \
  --issuer <aks-oidc-issuer-url> \
  --subject system:serviceaccount:azure-apim-operator-system:azure-apim-operator \
  --audiences api://AzureADTokenExchange
```

Get the AKS OIDC issuer URL:

```bash
az aks show --name <aks-name> --resource-group <aks-rg> --query "oidcIssuerProfile.issuerUrl" -o tsv
```

### 3. Assign RBAC Role

```bash
az role assignment create \
  --assignee <uami-client-id> \
  --role "API Management Service Contributor" \
  --scope /subscriptions/<sub-id>/resourceGroups/<rg>/providers/Microsoft.ApiManagement/service/<apim-name>
```

### 4. Configure Helm Values

```yaml
serviceAccount:
  create: true
  name: azure-apim-operator
  workloadIdentity:
    enabled: true
    clientID: <uami-client-id>
    tenantID: <tenant-id>
```

This configures the ServiceAccount with the required annotations and injects the `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` environment variables into the operator pod.

## Verifying Authentication

Check the operator logs for successful token acquisition:

```bash
kubectl logs -n azure-apim-operator-system deployment/azure-apim-operator -f
```

On success, you will see:

```
{"level":"info","ts":"...","logger":"identity","msg":"Successfully acquired Azure token","expires":"..."}
```

On failure, you will see:

```
{"level":"error","ts":"...","logger":"identity","msg":"Failed to create workload identity credential","error":"..."}
```

Common failure causes:

- `AZURE_CLIENT_ID` or `AZURE_TENANT_ID` not set -- check Helm values and pod environment
- Federated credential not configured -- verify the issuer URL and subject match
- Token file not mounted -- ensure Workload Identity is enabled on the AKS cluster
- RBAC not assigned -- the token works but APIM API calls return 403

See [Troubleshooting](troubleshooting.md) for more detailed diagnostics.
