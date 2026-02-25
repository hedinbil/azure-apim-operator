# Helm Configuration Reference

The operator is distributed as a Helm chart. This document covers all configurable values.

**Chart:** `oci://ghcr.io/hedinit/azure-apim-operator/azure-apim-operator`
**Chart version:** `0.23.0`
**App version:** `1.23.0`

## Installation

```bash
helm install azure-apim-operator oci://ghcr.io/hedinit/azure-apim-operator/azure-apim-operator \
  --version 0.23.0 \
  --namespace azure-apim-operator-system \
  --create-namespace \
  -f values.yaml
```

## Values Reference

### Image

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `image.repository` | string | `ghcr.io/hedinit/azure-apim-operator` | Container image repository |
| `image.tag` | string | `v0.23.0-latest` | Image tag |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | list | `[]` | Secrets for private registries |

### Replicas

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `replicaCount` | int | `1` | Number of operator replicas |

### ServiceAccount and Workload Identity

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `serviceAccount.create` | bool | `true` | Create a ServiceAccount |
| `serviceAccount.automount` | bool | `true` | Automount API credentials |
| `serviceAccount.name` | string | `azure-apim-operator` | ServiceAccount name |
| `serviceAccount.annotations` | map | `{}` | Additional annotations |
| `serviceAccount.workloadIdentity.enabled` | bool | `true` | Enable Azure Workload Identity |
| `serviceAccount.workloadIdentity.clientID` | string | | Client ID of the Azure UAMI |
| `serviceAccount.workloadIdentity.tenantID` | string | | Azure AD tenant ID |

When `workloadIdentity.enabled` is `true`, the chart:

- Adds the `azure.workload.identity/client-id` annotation to the ServiceAccount
- Adds the `azure.workload.identity/use: "true"` label to the pod
- Injects `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` as environment variables

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

### Swagger / OpenAPI Discovery

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `swagger.annotationKey` | string | `operator.io/openapi-export` | Kubernetes annotation key the operator uses to detect services with OpenAPI specs |
| `swagger.defaultPath` | string | `/swagger.yaml` | Default path for the OpenAPI endpoint |

### Networking

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `service.type` | string | `ClusterIP` | Service type |
| `service.port` | int | `80` | Service port |
| `ingress.enabled` | bool | `false` | Enable ingress |
| `ingress.className` | string | `""` | Ingress class |
| `ingress.annotations` | map | `{}` | Ingress annotations |
| `ingress.hosts` | list | | Ingress host configuration |
| `ingress.tls` | list | `[]` | TLS configuration |

### Resources and Scheduling

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `resources` | map | `{}` | CPU/memory requests and limits |
| `nodeSelector` | map | `{}` | Node selector constraints |
| `tolerations` | list | `[]` | Pod tolerations |
| `affinity` | map | `{}` | Pod affinity rules |

### Pod Configuration

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `podAnnotations` | map | `{}` | Additional pod annotations |
| `podLabels` | map | `{}` | Additional pod labels |
| `podSecurityContext` | map | `{}` | Pod security context |
| `securityContext` | map | `{}` | Container security context |
| `nameOverride` | string | `""` | Override chart name |
| `fullnameOverride` | string | `""` | Override full name |

### Health Probes

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `livenessProbe.httpGet.path` | string | `/` | Liveness probe path |
| `livenessProbe.httpGet.port` | string | `http` | Liveness probe port |
| `readinessProbe.httpGet.path` | string | `/` | Readiness probe path |
| `readinessProbe.httpGet.port` | string | `http` | Readiness probe port |

### Autoscaling

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `autoscaling.enabled` | bool | `false` | Enable HPA |
| `autoscaling.minReplicas` | int | `1` | Minimum replicas |
| `autoscaling.maxReplicas` | int | `100` | Maximum replicas |
| `autoscaling.targetCPUUtilizationPercentage` | int | `80` | Target CPU utilization |

### Volumes

| Value | Type | Default | Description |
|-------|------|---------|-------------|
| `volumes` | list | `[]` | Additional volumes |
| `volumeMounts` | list | `[]` | Additional volume mounts |

### Telemetry (Optional)

Telemetry is disabled by default. To enable OpenTelemetry tracing, set the following environment variables:

```yaml
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "opentelemetry-collector.otel.svc.cluster.local:4317"
  - name: OTEL_TRACES_EXPORTER
    value: "otlp"
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: "grpc"
```

If `OTEL_EXPORTER_OTLP_ENDPOINT` is not set, telemetry is completely disabled.

## Minimal Production Example

```yaml
replicaCount: 1

image:
  repository: ghcr.io/hedinit/azure-apim-operator
  tag: "v0.23.0-latest"

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
