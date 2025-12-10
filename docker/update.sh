#!/bin/bash

# A script to update the Docker container with the latest image.
#
# Usage: ./update.sh

# Exit immediately if a command exits with a non-zero status.
set -e

# --- Configuration ---
IMAGE_NAME="duckautomata/live-transcript-server"
TAG="latest"
CONTAINER_NAME="live_transcript_server"
RESTART_POLICY="always"

# --- Path and Environment Setup ---
# This ensures the script always runs relative to its own location.
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)
CONFIG_FILE_PATH="$SCRIPT_DIR/config.yaml"
TMP_DIR="$SCRIPT_DIR/tmp"

echo "Running update from: $SCRIPT_DIR"

# --- Pre-flight Checks ---
echo -e "\nPerforming pre-flight checks..."

# Determine the correct Docker command (docker or sudo docker)
if command -v docker &> /dev/null && docker info > /dev/null 2>&1; then
    DOCKER_CMD="docker"
elif command -v sudo &> /dev/null && sudo docker info > /dev/null 2>&1; then
    DOCKER_CMD="sudo docker"
else
    echo "Error: Docker is not running or you lack permission to use it."
    exit 1
fi
echo "  Docker command set to '$DOCKER_CMD'"

# Check for the required config file
if [ ! -f "$CONFIG_FILE_PATH" ]; then
    echo "Error: Config file not found at '$CONFIG_FILE_PATH'"
    exit 1
fi
echo "  Config file found"

# Create necessary host directories
mkdir -p "$TMP_DIR"

# --- Update Process ---
# Stop and remove the existing container if it exists
if [ $($DOCKER_CMD ps -a -q -f name="^${CONTAINER_NAME}$") ]; then
    echo -e "Stopping and removing existing container: $CONTAINER_NAME"
    $DOCKER_CMD stop "$CONTAINER_NAME" > /dev/null
    $DOCKER_CMD rm -f "$CONTAINER_NAME" > /dev/null
else
    echo -e "No existing container named '$CONTAINER_NAME' found. Will create a new one."
fi

# Pull the latest image
echo "Pulling the latest image: $IMAGE_NAME:$TAG"
$DOCKER_CMD pull "$IMAGE_NAME:$TAG"

# Start the new container
echo "Starting new container: $CONTAINER_NAME"
$DOCKER_CMD run \
    --name "$CONTAINER_NAME" \
    -d --restart "$RESTART_POLICY" \
    -p 8080:8080 \
    -v "$CONFIG_FILE_PATH:/app/config.yaml:ro,z" \
    -v "$TMP_DIR:/app/tmp:z" \
    "$IMAGE_NAME:$TAG"

# --- Post-run Check ---
# Give the container a moment to start or fail
sleep 3
if ! $DOCKER_CMD ps -q -f name="^${CONTAINER_NAME}$" > /dev/null; then
    echo "Error: Container failed to start after update. Check logs for details:"
    echo "   $DOCKER_CMD logs $CONTAINER_NAME"
    exit 1
fi

echo -e "\nUpdate complete. Container '$CONTAINER_NAME' is running with the latest image."
echo -e "\nRetrieving startup logs for $CONTAINER_NAME..."
echo "---------------------------------------------------"
$DOCKER_CMD logs "$CONTAINER_NAME"
echo "---------------------------------------------------"
