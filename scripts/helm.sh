#!/bin/bash

set -euo pipefail

CHART_DIR="../charts"
CHART_NAME="azure-apim-operator"
ACR_REPO="oci://hedinit.azurecr.io/helm-charts"

# Optional: Pass version as argument
VERSION="${1:-0.8.0}"
CHART_PACKAGE="${CHART_NAME}-${VERSION}.tgz"

log() {
  echo -e "[$(date '+%Y-%m-%d %H:%M:%S')] $1"
}

error_exit() {
  echo "❌ Error: $1"
  exit 1
}

log "📦 Packaging Helm chart '${CHART_NAME}' version ${VERSION}..."
cd "${CHART_DIR}" || error_exit "Chart directory not found: ${CHART_DIR}"

if ! helm package "${CHART_NAME}" --version "${VERSION}" > /dev/null; then
  error_exit "Failed to package Helm chart."
fi
log "✅ Helm chart packaged: ${CHART_PACKAGE}"

log "🔐 Logging in to ACR (hedinit.azurecr.io)..."
if ! az acr login --name hedinit > /dev/null; then
  error_exit "Failed to login to Azure Container Registry."
fi
log "✅ Logged in to ACR"

log "🚀 Pushing Helm chart to ${ACR_REPO}..."
if ! helm push "${CHART_PACKAGE}" "${ACR_REPO}"; then
  error_exit "Helm chart push failed."
fi
log "✅ Helm chart pushed successfully."

# Cleanup the local .tgz if needed
# rm -f "${CHART_PACKAGE}"
