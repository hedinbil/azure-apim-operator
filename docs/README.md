# Azure APIM Operator Documentation

The Azure APIM Operator is a Kubernetes operator that automates API registration in Azure API Management. When services deploy to Kubernetes, the operator detects OpenAPI specs and registers, updates, and configures APIs in Azure APIM automatically.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](architecture.md) | How the operator works internally -- controllers, reconciliation flows, and APIM integration |
| [Getting Started](getting-started.md) | Installation via Helm and deploying your first API to APIM |
| [Custom Resources](custom-resources.md) | CRD reference for all six resource types with field descriptions and example YAML |
| [Authentication](authentication.md) | Azure Workload Identity setup, fallback methods, and required permissions |
| [OpenAPI Spec Requirements](openapi-spec-requirements.md) | Producing APIM-compatible OpenAPI specs -- operationId, naming, and migration guide |
| [Helm Configuration](helm-configuration.md) | Complete Helm chart values reference |
| [Troubleshooting](troubleshooting.md) | Common errors, their causes, and how to resolve them |

## Quick Links

- **I want to deploy the operator** -- start with [Getting Started](getting-started.md), then [Authentication](authentication.md)
- **I want to register an API in APIM** -- see the `APIMAPI` section in [Custom Resources](custom-resources.md)
- **My APIM import is failing** -- check [Troubleshooting](troubleshooting.md) and [OpenAPI Spec Requirements](openapi-spec-requirements.md)
- **I want to understand how it works** -- read [Architecture](architecture.md)
- **I need to configure Helm values** -- see [Helm Configuration](helm-configuration.md)
