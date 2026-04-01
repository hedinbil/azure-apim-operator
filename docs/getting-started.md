# Getting Started

This guide walks through installing the Azure APIM Operator and deploying your first API to Azure API Management.

## Prerequisites

- A Kubernetes cluster (1.28+)
- `kubectl` configured to access the cluster
- `helm` v3 installed
- An Azure API Management service instance
- Azure Workload Identity configured on the cluster (see [Authentication](authentication.md))
- An application deployed in the cluster that exposes an OpenAPI/Swagger endpoint

## Installation

### 1. Add the Helm chart

```bash
helm install azure-apim-operator oci://ghcr.io/hedinit/azure-apim-operator/azure-apim-operator \
  --version 0.25.0 \
  --namespace azure-apim-operator-system \
  --create-namespace \
  --set serviceAccount.workloadIdentity.enabled=true \
  --set serviceAccount.workloadIdentity.clientID=<your-uami-client-id> \
  --set serviceAccount.workloadIdentity.tenantID=<your-tenant-id>
```

### 2. Configure APIM service references

The Helm chart can create `APIMService` resources automatically via the `apimServices` value:

```yaml
apimServices:
  - name: my-apim-instance
    resourceGroup: rg-my-apim
    subscription: 00000000-0000-0000-0000-000000000000
```

Or create them manually after installation:

```yaml
apiVersion: apim.operator.io/v1
kind: APIMService
metadata:
  name: my-apim-instance
  namespace: azure-apim-operator-system
spec:
  name: my-apim-instance
  resourceGroup: rg-my-apim
  subscription: 00000000-0000-0000-0000-000000000000
```

### 3. Verify the operator is running

```bash
kubectl get pods -n azure-apim-operator-system
```

You should see the operator pod in `Running` state.

## Register Your First API

### Step 1: Create an APIMAPI resource

The `APIMAPI` resource tells the operator which API to manage in APIM. Create it in the **same namespace** as your application:

```yaml
apiVersion: apim.operator.io/v1
kind: APIMAPI
metadata:
  name: my-public-api
  namespace: my-namespace
spec:
  APIID: my-api                             # Unique API identifier in APIM
  apimService: my-apim-instance             # References the APIMService CR
  routePrefix: /my-api                      # Base path in APIM
  serviceUrl: https://my-app.internal.example.com
  openApiDefinitionUrl: https://my-app.internal.example.com/swagger/v1/swagger.json
  target:
    selector:
      matchLabels:
        app.kubernetes.io/name: my-app
  subscriptionRequired: true
  productIds:                                # Optional
    - my-product
  tagIds:                                    # Optional
    - my-tag
```

`serviceUrl` and `openApiDefinitionUrl` remain explicit URLs. In many setups they point at an ingress or internal hostname rather than a Kubernetes Service DNS name.

**Matching behavior:**

- Preferred: set `spec.target.selector` to match the application's ReplicaSet labels.
- Legacy fallback: if `spec.target.selector` is omitted, `metadata.name` must match the workload's `app.kubernetes.io/name` label.

### Step 2: Deploy your application

When your application's ReplicaSet has ready pods, the operator automatically:

1. Detects the ready ReplicaSet
2. Matches it to one or more `APIMAPI` resources using `spec.target.selector`, or the legacy name-based fallback if no selector is set
3. Creates a transient `APIMAPIDeployment` resource for each match
4. Fetches the OpenAPI spec from `openApiDefinitionUrl`
5. Imports it into Azure APIM
6. Configures service URL, products, tags, and subscription settings
7. Updates the `APIMAPI` status with the APIM gateway URL
8. Cleans up the `APIMAPIDeployment` resource

### Step 3: Verify the import

Check the APIMAPI status:

```bash
kubectl get apimapi my-public-api -n my-namespace -o yaml
```

Look for the `status` section:

```yaml
status:
  apiHost: https://my-apim-instance.azure-api.net/my-api
  developerPortalHost: https://my-apim-instance.developer.azure-api.net
  importedAt: "2026-01-15T10:30:00Z"
  status: "OK"
```

You can also verify in the Azure portal by navigating to your APIM instance and checking the APIs section.

## What Happens on Redeployment

Every time your application is updated (a new ReplicaSet becomes ready), the operator re-imports the OpenAPI spec. This keeps APIM in sync with your latest API changes.

You can have multiple `APIMAPI` resources point at the same application by using selectors, while still giving each API its own `APIID`, route prefix, and backend/OpenAPI URLs.

For this to work reliably on repeated imports, your OpenAPI spec must include `operationId` on every operation. See [OpenAPI Spec Requirements](openapi-spec-requirements.md) for details.

## Optional: Products and Tags

If you want to organize APIs in APIM using products and tags, create the corresponding resources:

```yaml
apiVersion: apim.operator.io/v1
kind: APIMProduct
metadata:
  name: my-product
  namespace: azure-apim-operator-system
spec:
  productId: my-product
  displayName: My Product
  description: APIs for my platform
  published: true
  apimService: my-apim-instance
---
apiVersion: apim.operator.io/v1
kind: APIMTag
metadata:
  name: my-tag
  namespace: azure-apim-operator-system
spec:
  tagId: my-tag
  displayName: My Tag
  apimService: my-apim-instance
```

Then reference their IDs in the `APIMAPI` resource's `productIds` and `tagIds` fields.

## Next Steps

- [Custom Resources](custom-resources.md) -- full CRD reference
- [Authentication](authentication.md) -- setting up Azure Workload Identity
- [OpenAPI Spec Requirements](openapi-spec-requirements.md) -- making your API specs APIM-compatible
- [Troubleshooting](troubleshooting.md) -- when things go wrong
