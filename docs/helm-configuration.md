# Helm Configuration Reference

The operator is distributed as a Helm chart. This document covers all configurable values.

**Chart:** `oci://ghcr.io/hedinbil/charts/azure-apim-operator`
**Chart version:** `0.33.0` (`version` and `appVersion` in `Chart.yaml` are kept equal)

## Installation

```bash
helm install azure-apim-operator oci://ghcr.io/hedinbil/charts/azure-apim-operator \
  --version 0.33.0 \
  --namespace azure-apim-operator-system \
  --create-namespace \
  -f values.yaml
```

## Values Reference

All keys below are in `charts/azure-apim-operator/values.yaml`; the chart has no others.

### Image

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `image.repository` | string | `hedinit.azurecr.io/azure-apim-operator` | Container image repository |
| `image.tag` | string | `""` | Image tag; empty means `v<appVersion>` |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | list | `[]` | Secrets for private registries |
| `nameOverride` | string | `""` | Override chart name |
| `fullnameOverride` | string | `""` | Override full name |

### Replicas and Leader Election

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `replicaCount` | int | `1` | Number of operator replicas |
| `leaderElect` | bool | `true` | Passes `--leader-elect` to the manager, so only one replica reconciles at a time |

### ServiceAccount and Workload Identity

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `serviceAccount.create` | bool | `true` | Create a ServiceAccount |
| `serviceAccount.automount` | bool | `true` | Automount API credentials |
| `serviceAccount.name` | string | `azure-apim-operator` | ServiceAccount name |
| `serviceAccount.annotations` | map | `{}` | Additional annotations |
| `serviceAccount.workloadIdentity.enabled` | bool | `true` | Enable Azure Workload Identity |
| `serviceAccount.workloadIdentity.clientID` | string | placeholder | Client ID of the Azure UAMI |
| `serviceAccount.workloadIdentity.tenantID` | string | placeholder | Azure AD tenant ID |

When `workloadIdentity.enabled` is `true`, the chart:

- Adds the `azure.workload.identity/client-id` (and, when set, `azure.workload.identity/tenant-id`) annotation to the ServiceAccount
- Adds the `azure.workload.identity/use: "true"` label to the pod

The Workload Identity webhook then injects `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` into the pod.

### APIM Service Configuration

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `apimServices` | list | `[]` | List of APIM service instances to create as `APIMService` CRs |

Each entry in `apimServices`:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Azure APIM service instance name |
| `resourceGroup` | string | Azure resource group |
| `subscription` | string | Azure subscription ID |

Example:

```yaml
apimServices:
  - name: apim-prod
    resourceGroup: rg-apim-prod
    subscription: 00000000-0000-0000-0000-000000000000
  - name: apim-dev
    resourceGroup: rg-apim-dev
    subscription: 00000000-0000-0000-0000-000000000000
```

### Metrics

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `metrics.enabled` | bool | `true` | Serve the manager's Prometheus endpoint as plain HTTP on `metrics.port` (`--metrics-bind-address`, `--metrics-secure=false`) and expose it as container port `metrics` |
| `metrics.port` | int | `8080` | Metrics port |
| `metrics.datadog.enabled` | bool | `true` | Add the Datadog openmetrics autodiscovery annotation to the pod, so the node's agent scrapes it |
| `metrics.datadog.metrics` | list | see `values.yaml` | Metric families Datadog collects (controller-runtime reconcile, workqueue and REST client series) |

The chart has no Service, so only node-local scrapers such as the Datadog agent reach the endpoint.

### Shutdown and Memory

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `terminationGracePeriodSeconds` | int | `180` | Pod termination grace period. Must exceed the manager's 150 second graceful shutdown, which lets a write already sent to APIM come back and be recorded |
| `goMemLimit` | string | `200MiB` | Sets `GOMEMLIMIT`, the Go runtime's soft memory limit. Keep it below `resources.limits.memory`; empty leaves it unset |

### Resources and Scheduling

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `resources` | map | requests `20m`/`64Mi`, limits `500m`/`256Mi` | CPU/memory requests and limits |
| `nodeSelector` | map | `{}` | Node selector constraints |
| `tolerations` | list | `[]` | Pod tolerations |
| `affinity` | map | `{}` | Pod affinity rules |

### Pod Configuration

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `podAnnotations` | map | `{}` | Additional pod annotations |
| `podLabels` | map | `{}` | Additional pod labels |
| `podSecurityContext` | map | `runAsNonRoot: true`, `seccompProfile: RuntimeDefault` | Pod security context |
| `securityContext` | map | `allowPrivilegeEscalation: false`, drop `ALL` capabilities | Container security context |

### Health Probes

The manager serves its probes on port 8081.

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `livenessProbe` | map | `GET /healthz` on 8081, initial delay 15 s, period 20 s | Liveness probe |
| `readinessProbe` | map | `GET /readyz` on 8081, initial delay 5 s, period 10 s | Readiness probe |

### Volumes

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `volumes` | list | `[]` | Additional volumes |
| `volumeMounts` | list | `[]` | Additional volume mounts |

### Telemetry

The operator exposes Prometheus metrics; see the `metrics` values block. There
is no tracing: the OpenTelemetry initialiser this section used to document was
never called, and the `env:` block it described was never rendered by the
deployment template, so both were removed in 0.28.0 (APIM-17). The `metrics`
block is the supported observability surface.

## Minimal Production Example

```yaml
replicaCount: 1

serviceAccount:
  create: true
  name: azure-apim-operator
  workloadIdentity:
    enabled: true
    clientID: 12345678-1234-1234-1234-123456789abc
    tenantID: abcdefab-abcd-abcd-abcd-abcdefabcdef

apimServices:
  - name: apim-prod
    resourceGroup: rg-apim-prod
    subscription: 00000000-0000-0000-0000-000000000000

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 200m
    memory: 256Mi
```
