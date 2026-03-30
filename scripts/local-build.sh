#!/usr/bin/env bash
set -euo pipefail

IMAGE_NAME="duckautomata/live-transcript-server"
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# If NEW_VERSION is not set, we're running locally
if [ -z "${NEW_VERSION:-}" ]; then
    echo "No version provided, building with dev tag..."
    docker build \
        --build-arg VERSION=dev \
        --build-arg BUILD_TIME="${BUILD_TIME}" \
        -t "${IMAGE_NAME}:dev" \
        .
    echo "Pushing ${IMAGE_NAME}:dev..."
    docker push "${IMAGE_NAME}:dev"
    echo "Done. Published ${IMAGE_NAME}:dev"
    exit 0
fi

echo "Building version ${NEW_VERSION}..."
docker build \
    --build-arg VERSION="${NEW_VERSION}" \
    --build-arg BUILD_TIME="${BUILD_TIME}" \
    -t "${IMAGE_NAME}:${NEW_VERSION}" \
    -t "${IMAGE_NAME}:latest" \
    .

echo "Pushing ${IMAGE_NAME}:${NEW_VERSION}..."
docker push "${IMAGE_NAME}:${NEW_VERSION}"

echo "Pushing ${IMAGE_NAME}:latest..."
docker push "${IMAGE_NAME}:latest"

echo "Done. Published ${IMAGE_NAME}:${NEW_VERSION} and ${IMAGE_NAME}:latest"
