# Azure APIM Operator for Kubernetes

This repository provides a Kubernetes operator built using **Kubebuilder** and **Go**, designed to seamlessly deploy APIs into Azure API Management (APIM). It leverages Kubernetes Custom Resource Definitions (CRDs) to automate API registration and updates, ensuring your APIs are always synchronized with your Kubernetes deployments.

---

## 📌 Features

* Automatic registration and updating of APIs in Azure APIM.
* Integrated with Kubernetes via CRDs and controllers.
* Uses Helm charts for easy deployment and lifecycle management.
* Fully automated workflows for image building, Helm chart packaging, and deployment to Azure Container Registry (ACR).

---

## 📚 Prerequisites

Ensure you have the following tools installed:

* **Go** (version 1.21+)
* **Kubebuilder** (`go install sigs.k8s.io/kubebuilder/...`)
* **Docker**
* **Helm**
* **Azure CLI**
* Access to Azure Container Registry (ACR)

---

## 🚀 Quick Start

### Step 1: Clone Repository

```bash
git clone https://github.com/hedinit/azure-apim-operator.git
cd azure-apim-operator
```

### Step 2: Deploy Operator using Helm

Ensure your Kubernetes context is set correctly, then install using Helm:

```bash
helm upgrade --install azure-apim-operator ./charts/azure-apim-operator --namespace apim-operator --create-namespace
```

### Step 3: Verify Installation

Check that your CRDs are successfully installed:

```bash
kubectl get crds | grep apim.hedinit.io
```

Check the operator deployment status:

```bash
kubectl get pods -n apim-operator
```

---

## ⚙️ Creating Custom Resources

To register a new API in Azure APIM, define an `APIMAPI` custom resource:

```yaml
apiVersion: apim.hedinit.io/v1
kind: APIMAPI
metadata:
  name: example-api
  namespace: your-app-namespace
spec:
  APIID: example-api
  host: example.yourdomain.com
  routePrefix: /v1
  openAPIDefinitionURL: /swagger/v1/swagger.json
  apimService: your-apim-instance
```

Apply the configuration:

```bash
kubectl apply -f your-api.yaml
```

---

## 🔄 How It Works

1. **ReplicaSet Watcher Controller**

   * Watches for ReplicaSets.
   * Matches ReplicaSets to `APIMAPI` resources based on labels.
   * Creates an intermediate `APIMAPIRevision` CR once the corresponding Pod and Ingress are ready.

2. **APIM API Revision Controller**

   * Watches for `APIMAPIRevision` CRs.
   * Fetches the Swagger/OpenAPI spec from the deployed application's endpoint.
   * Registers or updates the API in Azure APIM.
   * Cleans up the intermediate CR after successful deployment.

---

## 📦 Building & Deployment

### Build Docker Image

Use the provided script to build and push the Docker image:

```bash
./scripts/docker.sh vX.Y.Z
```

Replace `vX.Y.Z` with your desired version.

### Deploy Helm Chart

Package and push the Helm chart:

```bash
./scripts/helm.sh X.Y.Z
```

---

## 🛡️ RBAC Permissions

Adjust RBAC permissions by updating:

* `config/rbac/role.yaml` for CRDs and Kubernetes built-in resources.
* Helm templates located in `charts/azure-apim-operator/templates/clusterrole.yaml`.

After changes, regenerate manifests:

```bash
make manifests
```

---

## 📝 Logging & Troubleshooting

Check the operator logs with:

```bash
kubectl logs -l app.kubernetes.io/name=azure-apim-operator -n apim-operator
```

For troubleshooting:

* Validate CRDs are registered (`kubectl get crds`).
* Ensure Pods, ReplicaSets, and Ingress resources match correctly.

---

## 🌟 Best Practices

* Clearly label your Kubernetes deployments and ReplicaSets (`app.kubernetes.io/name`).
* Define concise and clear `APIMAPI` resource specifications.
* Monitor operator logs regularly for proactive troubleshooting.

---

## 📌 Contributions

Contributions are welcome! Please open issues or PRs to improve the operator.

---

© 2025 Hedin IT - All rights reserved.
