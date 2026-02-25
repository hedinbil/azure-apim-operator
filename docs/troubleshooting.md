# Troubleshooting

This guide covers common errors and how to resolve them.

## Reading Operator Logs

The operator uses structured JSON logging. View logs with:

```bash
kubectl logs -n azure-apim-operator-system deployment/azure-apim-operator -f
```

Each log line includes a `logger` field indicating which controller produced it:

| Logger | Controller |
|--------|-----------|
| `replicasetwatcher_controller` | ReplicaSet watcher |
| `apimapideployment_controller` | API deployment |
| `apimapi_controller` | APIMAPI reconciler |
| `apimproduct_controller` | Product management |
| `apimtag_controller` | Tag management |
| `apiminboundpolicy_controller` | Policy management |
| `apim` | APIM REST API client |
| `identity` | Azure authentication |

## Checking CRD Status

```bash
# Check APIMAPI status
kubectl get apimapi -n <namespace> -o wide

# Detailed status
kubectl get apimapi <name> -n <namespace> -o yaml

# Check if APIMAPIDeployment exists (should be transient)
kubectl get apimapideployment -n <namespace>

# Check product/tag/policy status
kubectl get apimproduct -n azure-apim-operator-system -o yaml
kubectl get apimtag -n azure-apim-operator-system -o yaml
kubectl get apiminboundpolicy -n azure-apim-operator-system -o yaml
```

## Common Errors

### APIM ValidationError: Operation with the same method and URL template already exists

**Error message:**

```json
{
  "code": "ValidationError",
  "message": "Operation with the same method and URL template already exists: POST, /v1/payments/..."
}
```

**Cause:** Your OpenAPI spec is missing `operationId` on one or more operations. Without `operationId`, APIM auto-generates resource names on first import. On re-import, it generates different names, can't match them, and tries to create duplicate operations.

**Solution:**

Add `operationId` (via `.WithName()` in ASP.NET or equivalent in your framework) to every endpoint and re-deploy. See [OpenAPI Spec Requirements](openapi-spec-requirements.md) for detailed guidance.

---

### OpenAPI Fetch Failure

**Log message:**

```
"msg": "Failed to fetch OpenAPI definition after retries"
```

**Cause:** The operator could not reach the application's OpenAPI endpoint after 5 retries with exponential backoff (2s, 4s, 8s, 16s, 32s).

**Common causes:**

- The application pod is not ready yet (check pod status)
- The `openApiDefinitionUrl` in the APIMAPI resource is incorrect
- The application doesn't expose an OpenAPI endpoint at that URL
- Network policies are blocking in-cluster traffic
- The application's Swagger middleware is not configured for the expected path

**Diagnosis:**

```bash
# Check if the URL is reachable from inside the cluster
kubectl run curl-test --rm -it --image=curlimages/curl -- \
  curl -s -o /dev/null -w "%{http_code}" http://my-app.my-namespace.svc.cluster.local/swagger/v1/swagger.json

# Check the APIMAPI resource for the configured URL
kubectl get apimapi <name> -n <namespace> -o jsonpath='{.spec.openApiDefinitionUrl}'
```

---

### Authentication Failure: Missing Environment Variables

**Log message:**

```
"msg": "AZURE_CLIENT_ID or AZURE_TENANT_ID not set"
```

**Cause:** The Workload Identity environment variables are not injected into the operator pod.

**Solution:**

1. Verify Helm values:
   ```yaml
   serviceAccount:
     workloadIdentity:
       enabled: true
       clientID: <your-client-id>
       tenantID: <your-tenant-id>
   ```

2. Check the pod's environment:
   ```bash
   kubectl exec -n azure-apim-operator-system deployment/azure-apim-operator -- env | grep AZURE
   ```

3. Ensure Workload Identity is enabled on the AKS cluster:
   ```bash
   az aks show --name <aks-name> --resource-group <rg> --query "oidcIssuerProfile.enabled"
   ```

---

### Authentication Failure: Token Acquisition

**Log message:**

```
"msg": "Failed to create workload identity credential"
```

or

```
"msg": "Failed to get Azure access token"
```

**Cause:** The operator obtained the environment variables but could not exchange the Kubernetes ServiceAccount token for an Azure AD token.

**Common causes:**

- Federated credential not configured on the managed identity
- OIDC issuer URL mismatch between AKS and the federated credential
- ServiceAccount subject mismatch (namespace or name doesn't match)
- Token file not mounted at `/var/run/secrets/azure/tokens/azure-identity-token`

**Diagnosis:**

```bash
# Check if the token file is mounted
kubectl exec -n azure-apim-operator-system deployment/azure-apim-operator -- \
  ls -la /var/run/secrets/azure/tokens/

# Verify the federated credential
az identity federated-credential list \
  --identity-name <identity-name> \
  --resource-group <rg> \
  --output table

# Check the OIDC issuer
az aks show --name <aks-name> --resource-group <rg> --query "oidcIssuerProfile.issuerUrl" -o tsv
```

---

### APIM API Error: 403 Forbidden

**Log message:**

```
"msg": "APIM API returned error", "status": "403 Forbidden"
```

**Cause:** The Azure token was obtained successfully, but the managed identity does not have permission to manage the APIM instance.

**Solution:**

Assign the **API Management Service Contributor** role (or a custom role) to the managed identity:

```bash
az role assignment create \
  --assignee <uami-client-id> \
  --role "API Management Service Contributor" \
  --scope /subscriptions/<sub-id>/resourceGroups/<rg>/providers/Microsoft.ApiManagement/service/<apim-name>
```

---

### APIM API Error: 404 Not Found

**Log message:**

```
"msg": "Failed to get APIMService"
```

**Cause:** The `APIMService` resource referenced in the `APIMAPI` spec does not exist in the operator namespace.

**Solution:**

1. Check that the `APIMService` resource exists:
   ```bash
   kubectl get apimservice -n azure-apim-operator-system
   ```

2. Verify the name matches what's referenced in the `APIMAPI`:
   ```bash
   kubectl get apimapi <name> -n <namespace> -o jsonpath='{.spec.apimService}'
   ```

3. Ensure the `APIMService` is in the operator's namespace, not the application namespace.

---

### ReplicaSet Not Triggering Deployment

**Symptoms:** Application pods are running and ready, but no `APIMAPIDeployment` is created.

**Common causes:**

1. **Missing `app.kubernetes.io/name` label:** The operator matches ReplicaSets to `APIMAPI` resources using this label.
   ```bash
   kubectl get replicaset -n <namespace> --show-labels
   ```

2. **No matching `APIMAPI` resource:** The `APIMAPI` resource name must match the `app.kubernetes.io/name` label value.
   ```bash
   kubectl get apimapi -n <namespace>
   ```

3. **ReplicaSet scaled to 0:** The operator ignores ReplicaSets with `spec.replicas: 0` (old revisions during rolling updates).

4. **ReadyReplicas transition not detected:** The operator only triggers when `ReadyReplicas` transitions from 0 to > 0. If the ReplicaSet was already ready when the operator started watching, it may have been missed.

**Workaround:** Restart the deployment to trigger a new ReplicaSet:

```bash
kubectl rollout restart deployment/<name> -n <namespace>
```

---

### APIMAPIDeployment Stuck (Not Cleaning Up)

**Symptoms:** An `APIMAPIDeployment` resource persists instead of being deleted after import.

**Cause:** The import workflow failed at a step after creation but before the cleanup step. The controller will retry on requeue.

**Diagnosis:**

```bash
# Check if the deployment resource exists
kubectl get apimapideployment -n <namespace>

# Check operator logs for errors
kubectl logs -n azure-apim-operator-system deployment/azure-apim-operator | grep "apimapideployment_controller"
```

**Manual cleanup:**

```bash
kubectl delete apimapideployment <name> -n <namespace>
```

The next time the application is redeployed, the operator will create a fresh `APIMAPIDeployment`.

---

### Product or Tag Assignment Failure

**Log message:**

```
"msg": "Failed to assign API to products"
```

**Cause:** The product or tag does not exist in APIM, or the operator doesn't have permissions to create the association.

**Solution:**

1. Verify the product/tag exists in APIM:
   ```bash
   kubectl get apimproduct -n azure-apim-operator-system
   kubectl get apimtag -n azure-apim-operator-system
   ```

2. Check that the `APIMProduct` or `APIMTag` resource has `status.phase: Created`:
   ```bash
   kubectl get apimproduct <name> -n azure-apim-operator-system -o yaml
   ```

3. Ensure the product/tag was created before the API import (products and tags must exist in APIM before they can be assigned to an API).

---

### Policy Application Failure

**Log message:**

```
"msg": "Failed to upsert inbound policy"
```

**Common causes:**

- Invalid XML in `policyContent` -- APIM validates the policy XML structure
- Referenced `operationId` does not exist in APIM -- the API must be imported first, and the operation must have a matching `operationId` in the OpenAPI spec
- Missing `<base />` elements -- APIM requires base policy references in each section

**Diagnosis:**

Validate your policy XML against the APIM policy reference. Ensure all four sections (`inbound`, `backend`, `outbound`, `on-error`) are present with `<base />` elements.

## Getting Help

If the issue is not covered here:

1. Check operator logs for the full error message and stack trace
2. Verify the Azure APIM REST API response body in the logs (logged at error level)
3. Test the APIM REST API directly using `az rest` or `curl` to isolate whether the issue is in the operator or in APIM
