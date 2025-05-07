# Creating a Kubernetes Controller for a Custom Resource Definition (CRD)

This document outlines the end-to-end process for creating a controller for a Custom Resource Definition (CRD) using Kubebuilder, including integrating it into a Helm chart and deploying via Docker and Helm scripts.

---

## 🧰 Prerequisites

* Go installed
* Kubebuilder installed (`go install sigs.k8s.io/kubebuilder/...`)
* Docker and Azure CLI installed
* Helm installed and configured
* Access to Azure Container Registry (ACR)

---

## 🛠️ Step 1: Create API and Controller

Use `kubebuilder` to scaffold a new API and controller:

```bash
kubebuilder create api \
  --group apim \
  --version v1 \
  --kind <KindName>
```

Replace `<KindName>` with the name of your new resource, e.g., `ReplicaSetWatcher` or `APIMAPIRevision`.

This will generate:

* API type in `api/v1/<kind>_types.go`
* Controller logic in `internal/controller/<kind>_controller.go`

---

## ✏️ Step 2: Implement Controller Logic

Edit the controller file (e.g., `replicasetwatcher_controller.go`) and implement your logic inside the `Reconcile` function. You can reference existing controllers like `PodWatcher` or `APIMAPIReconciler` for structure and logging style.

Make sure you correctly set up `OwnerReferences` when creating related objects and extract necessary labels from your resource or associated objects.

---

## 🔒 Step 3: Update RBAC Permissions

Manually update `config/rbac/role.yaml` with permissions for your new CRD and any resources your controller interacts with.

Example:

```yaml
- apiGroups:
  - apim.hedinit.io
  resources:
  - apimapirevisions
  verbs:
  - get
  - list
  - watch
  - create
  - update
  - patch
  - delete
```

Also update `role.yaml` for any built-in resources (e.g., `pods`, `replicasets`).

---

## ⚙️ Step 4: Generate CRDs and Manifests

Run the following to regenerate CRDs and manifests:

```bash
make manifests
```

This updates `config/crd/bases/` and other relevant manifests.

---

## 📦 Step 5: Copy CRDs to Helm Chart

Ensure your Helm chart contains the necessary CRDs:

Copy:

```bash
cp config/crd/bases/*.yaml charts/aks-apim-operator/crds/
```

Also copy the updated `role.yaml` contents into:

```bash
charts/aks-apim-operator/templates/clusterrole.yaml
```

---

## 🐳 Step 6: Build and Push Docker Image

Use the provided script:

```bash
./scripts/docker.sh vX.Y.Z
```

This builds the image and pushes to Azure Container Registry (`hedinit.azurecr.io`).

---

## 🚀 Step 7: Package and Push Helm Chart

Use the Helm script:

```bash
./scripts/helm.sh X.Y.Z
```

This packages the chart and pushes it to the ACR Helm repository.

---

## ✅ Done

Your new controller is now deployed and will begin watching for the custom resource in the cluster.

---

## 📘 Tips

* Use `kubectl logs -l app.kubernetes.io/name=aks-apim-operator` to check controller logs.
* Validate CRDs are installed using `kubectl get crds`.
* Keep controller logic small and focused on a single responsibility.
* Name your controller with `.Named("yourcontroller")` in `SetupWithManager()` to distinguish in logs.
