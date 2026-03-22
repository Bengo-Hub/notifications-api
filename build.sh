#!/usr/bin/env bash
# =============================================================================
# Notifications API Build and Deploy Script
# =============================================================================
# Purpose: Builds Docker image, pushes to registry, and updates Helm values
# Usage:
#   ./build.sh                     # Build only
#   DEPLOY=true ./build.sh         # Build and deploy
# =============================================================================

set -euo pipefail

# Helper functions for logging
info()    { echo -e "\033[0;34m[INFO]\033[0m $1"; }
success() { echo -e "\033[0;32m[SUCCESS]\033[0m $1"; }
warn()    { echo -e "\033[1;33m[WARN]\033[0m $1"; }
error()   { echo -e "\033[0;31m[ERROR]\033[0m $1"; }

# Configuration
APP_NAME=${APP_NAME:-"notifications-api"}
NAMESPACE=${NAMESPACE:-"notifications"}
DEPLOY=${DEPLOY:-false}
SETUP_DATABASES=${SETUP_DATABASES:-false}
ENV_SECRET_NAME=${ENV_SECRET_NAME:-"notifications-api-env"}
SERVICE_DB_NAME=${SERVICE_DB_NAME:-"notifications"}

# Registry configuration
REGISTRY_SERVER=${REGISTRY_SERVER:-docker.io}
REGISTRY_NAMESPACE=${REGISTRY_NAMESPACE:-codevertex}
IMAGE_REPO="${REGISTRY_SERVER}/${REGISTRY_NAMESPACE}/${APP_NAME}"

# DevOps repository configuration
DEVOPS_REPO=${DEVOPS_REPO:-"Bengo-Hub/devops-k8s"}
DEVOPS_DIR=${DEVOPS_DIR:-"$HOME/devops-k8s"}

GIT_EMAIL=${GIT_EMAIL:-"dev@bengobox.com"}
GIT_USER=${GIT_USER:-"Notifications Bot"}
TRIVY_ECODE=${TRIVY_ECODE:-0}

# Determine Git commit ID
if [[ -z ${GITHUB_SHA:-} ]]; then
  GIT_COMMIT_ID=$(git rev-parse --short=8 HEAD || echo "localbuild")
else
  GIT_COMMIT_ID=${GITHUB_SHA::8}
fi

# Handle KUBE_CONFIG fallback for B64 variant
KUBE_CONFIG=${KUBE_CONFIG:-${KUBE_CONFIG_B64:-}}

info "Service : ${APP_NAME}"
info "Namespace: ${NAMESPACE}"
info "Image   : ${IMAGE_REPO}:${GIT_COMMIT_ID}"

# =============================================================================
# PREREQUISITE CHECKS
# =============================================================================
for tool in git docker trivy; do
  command -v "$tool" >/dev/null || { error "$tool is required"; exit 1; }
done

if [[ ${DEPLOY} == "true" ]]; then
  for tool in kubectl helm yq jq; do
    command -v "$tool" >/dev/null || { error "$tool is required"; exit 1; }
  done
fi
success "Prerequisite checks passed"

# =============================================================================
# SECRET SYNC
# =============================================================================
if [[ ${DEPLOY} == "true" ]]; then
  info "Checking and syncing required secrets from devops-k8s..."
  SYNC_SCRIPT=$(mktemp)
  if curl -fsSL "https://raw.githubusercontent.com/${DEVOPS_REPO}/main/scripts/tools/check-and-sync-secrets.sh" -o "$SYNC_SCRIPT" 2>/dev/null; then
    source "$SYNC_SCRIPT"
    check_and_sync_secrets "REGISTRY_USERNAME" "REGISTRY_PASSWORD" "POSTGRES_PASSWORD" "REDIS_PASSWORD" "KUBE_CONFIG" || warn "Secret sync failed - continuing with existing secrets"
    rm -f "$SYNC_SCRIPT"
  else
    warn "Unable to download secret sync script - continuing with existing secrets"
  fi
fi

# =============================================================================
# BUILD & SCAN
# =============================================================================
info "Running Trivy filesystem scan"
trivy fs . --exit-code "$TRIVY_ECODE" --format table || true

info "Building Docker image"
DOCKER_BUILDKIT=1 docker build -t "${IMAGE_REPO}:${GIT_COMMIT_ID}" .
success "Docker build complete"

if [[ ${DEPLOY} != "true" ]]; then
  warn "DEPLOY=false -> skipping push/deploy"
  exit 0
fi

# =============================================================================
# PUSH
# =============================================================================
if [[ -n ${REGISTRY_USERNAME:-} && -n ${REGISTRY_PASSWORD:-} ]]; then
  echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY_SERVER" -u "$REGISTRY_USERNAME" --password-stdin
fi

docker push "${IMAGE_REPO}:${GIT_COMMIT_ID}"
success "Image pushed"

# =============================================================================
# KUBERNETES SETUP
# =============================================================================
if [[ -n ${KUBE_CONFIG:-} ]]; then
  mkdir -p ~/.kube
  echo "$KUBE_CONFIG" | base64 -d > ~/.kube/config 2>/dev/null || echo "$KUBE_CONFIG" > ~/.kube/config
  chmod 600 ~/.kube/config
  export KUBECONFIG=~/.kube/config
fi

kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

if [[ -n ${REGISTRY_USERNAME:-} && -n ${REGISTRY_PASSWORD:-} ]]; then
  kubectl -n "$NAMESPACE" create secret docker-registry registry-credentials \
    --docker-server="$REGISTRY_SERVER" \
    --docker-username="$REGISTRY_USERNAME" \
    --docker-password="$REGISTRY_PASSWORD" \
    --dry-run=client -o yaml | kubectl apply -f - || warn "registry secret creation failed"
fi

# =============================================================================
# DATABASE & SERVICE SECRETS
# =============================================================================
# Ensure devops-k8s is available for infrastructure scripts
if [[ ! -d "$DEVOPS_DIR" ]]; then
  info "DevOps directory not found. Cloning ${DEVOPS_REPO}..."
  TOKEN="${GH_PAT:-${GIT_SECRET:-${GITHUB_TOKEN:-}}}"
  CLONE_URL="https://github.com/${DEVOPS_REPO}.git"
  [[ -n $TOKEN ]] && CLONE_URL="https://x-access-token:${TOKEN}@github.com/${DEVOPS_REPO}.git"
  git clone "$CLONE_URL" "$DEVOPS_DIR" || warn "Unable to clone devops-k8s. Some deployment steps may fail."
fi

if [[ "$SETUP_DATABASES" == "true" && -d "$DEVOPS_DIR" ]]; then
  CREATE_DB_SCRIPT="${DEVOPS_DIR}/scripts/infrastructure/create-service-database.sh"
  if [[ -f "$CREATE_DB_SCRIPT" ]]; then
    info "Creating database '${SERVICE_DB_NAME}'..."
    chmod +x "$CREATE_DB_SCRIPT"
    SERVICE_DB_NAME="$SERVICE_DB_NAME" APP_NAME="$APP_NAME" NAMESPACE="$NAMESPACE" \
    POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-}" \
    bash "$CREATE_DB_SCRIPT" || warn "Database creation failed"
  fi
fi

CREATE_SECRETS_SCRIPT="${DEVOPS_DIR}/scripts/infrastructure/create-service-secrets.sh"
if [[ -d "$DEVOPS_DIR" && -f "$CREATE_SECRETS_SCRIPT" ]]; then
  info "Configuring service secrets using centralized script..."
  chmod +x "$CREATE_SECRETS_SCRIPT"
  SERVICE_NAME="$APP_NAME" NAMESPACE="$NAMESPACE" SECRET_NAME="$ENV_SECRET_NAME" \
  DB_NAME="$SERVICE_DB_NAME" DB_USER="${SERVICE_DB_NAME}_user" \
  POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-}" \
  bash "$CREATE_SECRETS_SCRIPT" || { error "Secret setup failed"; exit 1; }
  success "Service secrets configured"
fi

# =============================================================================
# DEPLOYMENT (Helm Value Update)
# =============================================================================
# Source centralized Helm values update script
source "${DEVOPS_DIR}/scripts/helm/update-values.sh" 2>/dev/null || {
  warn "Centralized helm update script not available at ${DEVOPS_DIR}/scripts/helm/update-values.sh"
}

# Update Helm values using centralized script function
if declare -f update_helm_values >/dev/null 2>&1; then
  info "Updating Helm values in devops repo..."
  export GIT_EMAIL="$GIT_EMAIL"
  export GIT_USER="$GIT_USER"
  update_helm_values "$APP_NAME" "$GIT_COMMIT_ID" "$IMAGE_REPO" || warn "Helm values update failed"
else
  warn "update_helm_values function not available - seeking binary fallback"
  UPDATE_HELM_BINARY="${DEVOPS_DIR}/scripts/tools/update-helm-values.sh"
  if [[ -f "$UPDATE_HELM_BINARY" ]]; then
    chmod +x "$UPDATE_HELM_BINARY"
    "$UPDATE_HELM_BINARY" "$APP_NAME" "$GIT_COMMIT_ID" || warn "Helm binary update failed"
  fi
fi

info "Deployment summary"
echo "  Image      : ${IMAGE_REPO}:${GIT_COMMIT_ID}"
echo "  Namespace  : ${NAMESPACE}"
echo "  Deployment : ${DEPLOY}"
echo "  ArgoCD will auto-deploy the new tag"
