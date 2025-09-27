#!/bin/bash

# A script to build and tag Docker images with major and minor versions.
#
# Usage: ./scripts/build.sh <version>
# Example: ./scripts/build.sh 1.2

# --- Configuration ---
IMAGE_NAME="duckautomata/live-transcript-server"
# ---------------------

# --- Path and Environment Setup ---
# This ensures the script always runs from the project's root directory.
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)
PROJECT_ROOT=$(dirname "$SCRIPT_DIR")
cd "$PROJECT_ROOT" || exit 1

echo "Running from project root: $PWD"

# --- Input Validation ---
# Check if a version argument was provided
if [ -z "$1" ]; then
    echo "Error: No version specified."
    echo "   Usage: $0 <version>"
    exit 1
fi

VERSION=$1

# Validate the version format using a regular expression.
if ! [[ $VERSION =~ ^[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Invalid version format: '${VERSION}'"
    echo "   Please use the format 'major.minor' (e.g., '1.2' or '10.4')."
    exit 1
fi

# --- Tag Generation ---
# Extract the major version (e.g., '1' from '1.2')
MAJOR_VERSION=$(echo "$VERSION" | cut -d. -f1)

# Construct the full tags
SPECIFIC_TAG="${IMAGE_NAME}:${VERSION}"
MAJOR_TAG="${IMAGE_NAME}:${MAJOR_VERSION}"
LATEST_TAG="${IMAGE_NAME}:latest"

echo -e "\nWill build image with the following tags:"
echo "   - Specific: ${SPECIFIC_TAG}"
echo "   - Major:    ${MAJOR_TAG}"
echo "   - Latest:   ${LATEST_TAG}"
echo "-----------------------------------"

# --- Docker Command ---
# Check if the user can run docker without sudo
if command -v docker &> /dev/null && docker info > /dev/null 2>&1; then
    DOCKER_CMD="docker"
elif command -v sudo &> /dev/null && sudo docker info > /dev/null 2>&1; then
    DOCKER_CMD="sudo docker"
else
    echo "Error: Docker is not running or you lack permission to use it."
    exit 1
fi

# Build the image and apply all tags in a single, efficient command.
echo "Building Docker image..."
if ! $DOCKER_CMD build \
    -t "${SPECIFIC_TAG}" \
    -t "${MAJOR_TAG}" \
    -t "${LATEST_TAG}" \
    .; then
    echo "Docker build failed. Aborting."
    exit 1
fi

echo -e "\nBuild successful. Created images:"
$DOCKER_CMD images --filter=reference="${IMAGE_NAME}"

# --- Optional Push to Registry ---
read -p "Push these tags to the registry? (y/n) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo " Pushing ${SPECIFIC_TAG}..."
    $DOCKER_CMD push "${SPECIFIC_TAG}"

    echo " Pushing ${MAJOR_TAG}..."
    $DOCKER_CMD push "${MAJOR_TAG}"

    echo " Pushing ${LATEST_TAG}..."
    $DOCKER_CMD push "${LATEST_TAG}"
    echo "Push complete."
fi
