#!/bin/bash

set -e # Exit on error

# Check if version argument is provided
if [ -z "$1" ]; then
  echo "Error: Version argument is required"
  echo "Usage: ./deploy.sh <version>"
  echo "Example: ./deploy.sh v0.0.1"
  exit 1
fi

VERSION="$1"
IMAGE_NAME="ghcr.io/codegoalie/random-motivation"
IMAGE_TAG="${IMAGE_NAME}:${VERSION}"

# Quality gates setup
SKIP_UAT="${SKIP_UAT:-0}"

# Error handler for quality gate failures
_quality_gate_error() {
  local step="$1"
  echo ""
  echo "❌ Quality gate failed: ${step}"
  echo "🛑 RELEASE ABORTED - Nothing was published."
  echo "   No Docker image pushed, no git tag created."
  exit 1
}

# Only run quality gates if SKIP_UAT is not set to 1
if [ "$SKIP_UAT" != "1" ]; then
  echo "🧪 Running quality gates..."
  echo ""

  # Step 1: Unit tests
  echo "🧪 Running unit tests"
  if ! go test ./...; then
    _quality_gate_error "unit tests"
  fi
  echo "✅ Unit tests passed"
  echo ""

  # Step 2: Check if port 8080 is free before running UAT
  echo "🔍 Checking if port 8080 is available"
  if exec 3<>/dev/tcp/localhost/8080 2>/dev/null; then
    exec 3>&-
    _quality_gate_error "port 8080 is already in use (bind failed). Stop any service listening on 8080 and try again."
  fi
  echo "✅ Port 8080 is available"
  echo ""

  # Step 3: Self-managed UAT suite
  echo "🧪 Running UAT suite (self-managed mode)"
  if ! go run ./cmd/uat --start-command "go run ." --base-url http://localhost:8080 --timeout 180s; then
    _quality_gate_error "UAT suite"
  fi
  echo "✅ UAT suite passed"
  echo ""
else
  echo "⚠️  WARNING: SKIP_UAT=1 detected. Skipping all quality gates."
  echo "⚠️  RELEASING UNVERIFIED CODE - this is NOT recommended for production."
  echo ""
fi

echo "🐳 Building Docker image: ${IMAGE_TAG}"
docker build -f Dockerfile . -t "${IMAGE_TAG}"

echo "📤 Pushing image to ghcr.io"
docker push "${IMAGE_TAG}"

echo "🏷️  Creating git tag: ${VERSION}"
git tag -a "${VERSION}" -m "Release ${VERSION}"

echo "📤 Pushing git tag to remote"
git push origin "${VERSION}"

echo ""
echo "✅ Build & publish complete!"
echo "   Git tag: ${VERSION}"
echo ""
echo "👉 Update the workload in dockhand to use this image:"
echo ""
echo "      ${IMAGE_TAG}"
echo ""
