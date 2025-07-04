#!/bin/bash

# A script to build and tag Docker images with major and minor versions.
#
# Usage: ./scripts/build.sh <version>
# Example: ./scripts/build.sh 1.2

# --- Configuration ---
IMAGE_NAME="duckautomata/live-transcript-server"
# ---------------------

if [[ "$PWD" == */scripts ]]; then
    echo "Error: This script must be run from the project's root directory, not from within the 'scripts' subdirectory."
    echo "You are currently in: $PWD"
    echo "Please change to the parent directory (e.g., 'cd ..') and run the script like this: ./scripts/build.sh"
    exit 1
fi

# Check if a version argument was provided
if [ -z "$1" ]; then
    echo "Error: No version specified."
    echo "Usage: $0 <version>"
    exit 1
fi

VERSION=$1

# Validate the version format using a regular expression.
if ! [[ $VERSION =~ ^[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Invalid version format: '${VERSION}'"
    echo "Please use the format 'major.minor' (e.g., '1.2' or '10.4')."
    exit 1
fi

# Extract the major version (e.g., '1' from '1.2')
MAJOR_VERSION=$(echo $VERSION | cut -d. -f1)

# Construct the full tags
SPECIFIC_TAG="${IMAGE_NAME}:${VERSION}"
MAJOR_TAG="${IMAGE_NAME}:${MAJOR_VERSION}"
LATEST_TAG="${IMAGE_NAME}:latest"

echo "Building image with tags:"
echo "  - Specific: ${SPECIFIC_TAG}"
echo "  - Major:    ${MAJOR_TAG}"
echo "  - Latest:   ${LATEST_TAG}"
echo "-----------------------------------"

# --- Docker Command ---
# Check if the user can run docker without sudo
if docker info > /dev/null 2>&1; then
    DOCKER_CMD="docker"
elif sudo docker info > /dev/null 2>&1; then
    DOCKER_CMD="sudo docker"
else
    echo "Error: Docker is not running or you don't have permission to run it."
    exit 1
fi

# Build the image with the specific version tag
$DOCKER_CMD build -t "${SPECIFIC_TAG}" .

# Check if the build succeeded before adding the second tag
if [ $? -ne 0 ]; then
    echo "Docker build failed. Aborting."
    exit 1
fi
echo "Build successful. Applying major version tag..."
$DOCKER_CMD tag "${SPECIFIC_TAG}" "${MAJOR_TAG}"
$DOCKER_CMD tag "${SPECIFIC_TAG}" "${LATEST_TAG}"
echo ""
echo "Successfully tagged images:"
$DOCKER_CMD images | grep "${IMAGE_NAME}"

# (Optional) Ask if the user wants to push the tags
read -p "Push these tags to the registry? (y/n) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo "Pushing ${SPECIFIC_TAG}..."
    $DOCKER_CMD push "${SPECIFIC_TAG}"
    echo "Pushing ${MAJOR_TAG}..."
    $DOCKER_CMD push "${MAJOR_TAG}"
    echo "Pushing ${LATEST_TAG}..."
    $DOCKER_CMD push "${LATEST_TAG}"
    echo "Push complete."
fi
