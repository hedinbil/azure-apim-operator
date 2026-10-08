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
| `apim` | APIM REST API client |
| `identity` | Azure authentication |

The product, tag and policy controllers log through controller-runtime's reconcile logger instead; their lines carry a `controller` field (`apimproduct`, `apimtag`, `apiminboundpolicy`).

Every APIM write, in all four writing controllers, logs the same lines with `kind`, `namespace`, `name` and the APIM id:

| Message | Meaning |
|---------|---------|
| `▶️ APIM write starting` | A write is sent, with `attempt` (e.g. `2/5`) and the step |
| `💚 APIM write succeeded` | The write is done; failures are cleared |
| `💔 APIM write failed; backing off` | Transient failure; `nextAttemptAt` says when the next attempt is |
| `💔 APIM write rejected; not retrying` | Permanent failure; the resource is now `Invalid` |
| `⏸️ APIM write still backing off; skipping` | A reconcile came before `nextAttemptAt`; nothing was sent |
| `⏸️ APIM write stopped; waiting for a spec change or the retry annotation` | The resource is `Stalled` or `Invalid`; nothing was sent |
| `🛑 APIM write stalled` | Fifth transient failure in a row; the resource is now `Stalled` |
| `🔁 APIM write failures cleared` | A spec change or the retry annotation reset the failures |

## Checking CRD Status

```bash
# Check APIMAPI status
kubectl get apimapi -n <namespace> -o wide

# Detailed status
kubectl get apimapi <name> -n <namespace> -o yaml

# Check the APIMAPIDeployment (one per APIMAPI, same name; holds the import state)
kubectl get apimapideployment -n <namespace>
kubectl get apimapideployment <name> -n <namespace> -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}{.status.lastError}{"\n"}'

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
"msg": "❌ Failed to fetch OpenAPI definition"
```

The `APIMAPIDeployment` shows phase `Error`, message `Failed to fetch OpenAPI definition` and the reason in `status.lastError`.

**Cause:** The operator could not fetch a usable document from `openApiDefinitionUrl`. It makes one attempt per reconcile (15 second timeout) and tries again after 30 seconds. A fetch failure is not an APIM write failure and never makes the deployment `Stalled`.

**Common causes:**

- The application pod is not ready yet (check pod status)
- The `openApiDefinitionUrl` in the APIMAPI resource is incorrect
- The application doesn't expose an OpenAPI endpoint at that URL
- Network policies are blocking in-cluster traffic
- The application's Swagger middleware is not configured for the expected path
- The URL resolves to a refused address (loopback, link-local, multicast, the Azure host agent), uses a scheme other than http/https, or redirects more than 3 times
- The document is larger than 8 MiB, or is not JSON/YAML with a top-level `openapi` or `swagger` key

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

**Symptoms:** Application pods are running and ready, but the `APIMAPIDeployment` stays in phase `WaitingForMatch` or `WaitingForReadyPod`.

**Common causes:**

1. **Missing labels for matching:** In legacy mode the operator relies on `app.kubernetes.io/name`. In selector mode the ReplicaSet still needs whatever labels the `APIMAPI.spec.target.selector` expects.
   ```bash
   kubectl get replicaset -n <namespace> --show-labels
   ```

2. **Selector mismatch:** If the `APIMAPI` uses `spec.target.selector`, the ReplicaSet labels must satisfy that selector.
   ```bash
   kubectl get apimapi <name> -n <namespace> -o jsonpath='{.spec.target.selector}'
   ```

3. **No matching `APIMAPI` resource:** In legacy mode, the `APIMAPI` resource name must match the `app.kubernetes.io/name` label value.
   ```bash
   kubectl get apimapi -n <namespace>
   ```

4. **ReplicaSet scaled to 0:** The operator ignores ReplicaSets with `spec.replicas: 0` (old revisions during rolling updates).

5. **Rolling update not finished:** Phase `WaitingForRollout` means an older revision still has ready pods. The operator waits for them to go, rechecking every 30 seconds.

The deployment rechecks the workload on its own every 2 minutes, so a missed ReplicaSet event does not need a restart.

---

### Import Waiting on APIM (`status.pendingImport`)

**Symptoms:** An `APIMAPIDeployment` stays in phase `Importing` with a message like `APIM is still importing the definition it accepted at ...`, and `status.pendingImport` is set.

**Cause:** APIM accepted the import (`202`) and is still running it. Large OpenAPI documents take minutes, much longer on a busy Developer-tier instance. The operator reads the operation (every 15 seconds at first, then every half of its age, at most every 15 minutes) and deliberately does not send another import until this one ends: APIM runs a second import alongside the first rather than refusing it, and piled-up imports can saturate a single-unit instance and slow every API on its gateway. A `nextAttemptAt` in the past next to it is left from a failure before this import; it is due, not a wait.

**Diagnosis:**

```bash
# When the import was accepted, and what the last reading said
kubectl get apimapideployment <name> -n <namespace> -o jsonpath='{.status.pendingImport}{"\n"}{.status.message}{"\n"}{.status.lastError}'

# Ask APIM directly (read-only)
az rest --method get --url "<status.pendingImport.operationUrl>"
```

A `lastError` mentioning `ManagementApiRequestFailed` or `Timeout` means APIM's management endpoint did not answer the poll, not that the import failed; the operator keeps waiting.

**Resolution:** Usually none: the operator continues as soon as APIM reports a result. An import that fails, or is still running after two hours, counts as a failed write (Backoff, then Stalled after five in a row). To start over sooner, first confirm in the APIM activity log that no import of the API is running, then clear the field:

```bash
kubectl patch apimapideployment <name> -n <namespace> --subresource=status --type=merge -p '{"status":{"pendingImport":null}}'
```

---

### Retries and recovery

`APIMAPIDeployment`, `APIMProduct`, `APIMTag` and `APIMInboundPolicy` share one retry policy for writes to APIM. Read the state from the resource's status:

```bash
kubectl get apimapideployment <name> -n <namespace> -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}{.status.lastError}{"\n"}{.status.consecutiveFailures}{" "}{.status.nextAttemptAt}{"\n"}'
kubectl get apimproduct <name> -n <namespace> -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}'
```

| Phase | Meaning | What to do |
|-------|---------|------------|
| `Backoff` | A write failed; `consecutiveFailures` counts them, `nextAttemptAt` is the next attempt (1, 2, 4, 8 minutes, +/-20 %, at most 30 minutes) | Usually nothing. Fix the cause shown in `message` (or `lastError` on a deployment) if it will not clear on its own |
| `Stalled` | Five transient failures in a row (throttling, conflicts, timeouts, 5xx, network). No more writes | Fix the cause, then reset |
| `Invalid` | APIM rejected the request: 400, 401, 403, 404 on the resource's own path, an import that failed with `ValidationError`, or a document the operator will not send (Swagger 2 YAML over 2 MiB). No more writes | Fix the spec, the document or the permissions, then reset |
| `Error` | A step before the write failed (missing `APIMService`, missing identity, token, OpenAPI fetch). Not counted | Fix the cause; retried after 30 or 60 seconds |

A `403` with the Azure code `AuthorizationFailed` (typically a role assignment that is missing or not yet propagated) counts as transient and ends in `Stalled`, not `Invalid`.

A write that fails only because something it depends on is not in APIM yet (a product or tag the API is assigned to, the API a policy is set on) keeps backing off, at most 30 minutes apart, and never becomes `Stalled`; it goes through once that resource exists. An API write whose outcome is unknown (no answer, 502, 504) waits at least 30 minutes, since APIM may still be running it.

For an API, the `APIMAPI` shows `status.status: Error` once its deployment is `Stalled` or `Invalid`, and ArgoCD shows it as Degraded.

**Reset:** change the spec, or set the `apim.operator.io/retry` annotation to a new value. That clears `consecutiveFailures` and writes at once. For an API, annotate the `APIMAPIDeployment` (same name as the `APIMAPI`), not the `APIMAPI`:

```bash
kubectl annotate apimapideployment <name> -n <namespace> apim.operator.io/retry="$(date +%s)" --overwrite
kubectl annotate apimproduct <name> -n <namespace> apim.operator.io/retry="$(date +%s)" --overwrite
```

A new OpenAPI document also lifts a `Stalled` or `Invalid` deployment the next time it reconciles (a rollout of the application), but does not cut short a running backoff.

**Pending import:** a deployment in phase `Importing` with `status.pendingImport` set is waiting for an import APIM accepted; nothing is written until it ends. The annotation does not clear it. See [Import Waiting on APIM](#import-waiting-on-apim-statuspendingimport).

Do not delete the `APIMAPIDeployment` to force a new import: it is recreated on the next `APIMAPI` change or ReplicaSet signal, and the lost status means that attempt can import on top of an import APIM is still running.

---

### Product or Tag Assignment Failure

**Log message:**

```
"msg": "Failed to assign API to products"
```

**Cause:** The product or tag does not exist in APIM, or the operator doesn't have permissions to create the association. A product or tag that does not exist yet is not a reason to stop: the deployment keeps backing off, at most 30 minutes apart, and the assignment goes through once the `APIMProduct` or `APIMTag` is created. The API itself is not imported again for this (`status.importedHash`).

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

3. To skip the wait once the product or tag exists, set the retry annotation on the `APIMAPIDeployment` (see [Retries and recovery](#retries-and-recovery)).

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

### Namespace Stuck Terminating on apimproducts

**Symptom:**

```
kubectl describe namespace <ns>
  NamespaceContentRemaining     SomeResourcesRemain   apimproducts.apim.operator.io has N resource instances
  NamespaceFinalizersRemaining  SomeFinalizersRemain  apim.operator.io/product in N resource instances
```

**Cause:** One or more `APIMProduct` resources have `spec.deletionPolicy: Delete`, so they carry the `apim.operator.io/product` finalizer, and the operator cannot complete the delete in APIM. The product's `status.message` and the operator log show the reason, for example `Failed to delete product ... 403 Forbidden`, or the product is still referenced by an API. The delete is retried on the backoff schedule until it is `Stalled` or `Invalid`; then it waits for a spec change or the retry annotation.

If the `APIMService` or the operator's Azure identity is gone, the operator cannot remove the product: it releases the finalizer, leaves the product in APIM and logs an error. Such resources do not block the namespace.

Resources with the default `Retain` policy carry no finalizer and can never block a namespace.

**Solution:**

1. Read the reason from the operator log:
   ```bash
   kubectl logs -n azure-apim-operator deploy/azure-apim-operator | grep "Failed to delete product"
   ```

2. If the product must stay in APIM - typically because the same `productId` is now managed from another cluster - switch the stuck resources to `Retain`. The operator releases the finalizer on its next reconcile without touching APIM, and the namespace finalizes within about 30 seconds:
   ```bash
   kubectl patch apimproducts.apim.operator.io <name> -n <ns> --type=merge \
     -p '{"spec":{"deletionPolicy":"Retain"}}'
   ```
   Use the fully qualified resource name: some clusters still carry legacy `apim.hedinit.io` CRDs that shadow the short name.

3. If the product should go, fix what the log reports (permissions, references) and let the operator retry.

**Moving an application between clusters:** the same `productId` in the same APIM instance is shared, not copied. Before deleting the source namespace, make sure every `APIMProduct` there is `Retain` (the default), otherwise the delete from the old cluster removes the product the new cluster is serving.

---

## Getting Help

If the issue is not covered here:

1. Check operator logs for the full error message and stack trace
2. Verify the Azure APIM REST API response body in the logs (logged at error level)
3. Test the APIM REST API directly using `az rest` or `curl` to isolate whether the issue is in the operator or in APIM
