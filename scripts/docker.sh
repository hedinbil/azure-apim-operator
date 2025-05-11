#!/bin/bash

set -euo pipefail

IMAGE_NAME="azure-apim-operator"
ACR_NAME="hedinit"
ACR_REGISTRY="${ACR_NAME}.azurecr.io"
VERSION="${1:-v0.2.0}"
FULL_IMAGE_NAME="${ACR_REGISTRY}/${IMAGE_NAME}:${VERSION}"
DOCKERFILE_DIR="../"  # The directory where the Dockerfile is

log() {
  echo -e "[$(date '+%Y-%m-%d %H:%M:%S')] $1"
}

error_exit() {
  echo "❌ Error: $1"
  exit 1
}

log "🔐 Logging in to Azure Container Registry: ${ACR_NAME}..."
if ! az acr login --name "${ACR_NAME}" > /dev/null; then
  error_exit "Failed to login to Azure Container Registry"
fi
log "✅ Logged in successfully."

log "🐳 Building and pushing Docker image: ${FULL_IMAGE_NAME}"
if ! docker buildx build --platform linux/amd64 -t "${FULL_IMAGE_NAME}" --push "${DOCKERFILE_DIR}"; then
  error_exit "Docker build and push failed"
fi
log "✅ Docker image built and pushed successfully."
