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
