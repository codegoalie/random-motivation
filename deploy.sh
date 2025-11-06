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
#
# echo "🐳 Building Docker image: ${IMAGE_TAG}"
# docker build -f Dockerfile . -t "${IMAGE_TAG}"
#
# echo "📤 Pushing image to ghcr.io"
# docker push "${IMAGE_TAG}"
#
# echo "🏷️  Creating git tag: ${VERSION}"
# git tag -a "${VERSION}" -m "Release ${VERSION}"
#
# echo "📤 Pushing git tag to remote"
# git push origin "${VERSION}"

echo "🔄 Updating docker-compose.yml on remote host"
REMOTE_HOST="casaos"
COMPOSE_FILE="/var/lib/casaos/apps/kind_eldan/docker-compose.yml"

# Update the image in docker-compose.yml on remote host
# shellcheck disable=SC2029
ssh "${REMOTE_HOST}" "sudo -S sed -i 's|image: ${IMAGE_NAME}:.*|image: ${IMAGE_TAG}|g' ${COMPOSE_FILE}"

echo "🔄 Restarting application on remote host"
# Navigate to the directory and restart using docker compose
ssh "${REMOTE_HOST}" "cd /var/lib/casaos/apps/kind_eldan && sudo -S docker compose down && sudo -S docker compose up -d"

echo "✅ Deployment complete!"
echo "   Image: ${IMAGE_TAG}"
echo "   Git tag: ${VERSION}"
echo "   Remote host: ${REMOTE_HOST}"
