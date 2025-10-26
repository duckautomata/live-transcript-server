#!/bin/bash

# A script to run the Docker image in the background.
#
# Usage: ./start.sh

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

echo "Running from: $SCRIPT_DIR"

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

# Check if a container with the same name is already running (exact match)
if [ $($DOCKER_CMD ps -q -f name="^${CONTAINER_NAME}$") ]; then
    echo "Warning: A container named '$CONTAINER_NAME' is already running."
    echo "   Please stop it first if you wish to restart it."
    exit 1
fi
echo "  No conflicting containers found"

# --- Docker Command ---
echo -e "\nStarting container: $CONTAINER_NAME"
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
    echo "Error: Container failed to start. Check logs for details:"
    echo "   $DOCKER_CMD logs $CONTAINER_NAME"
    exit 1
fi

echo "Container '$CONTAINER_NAME' started successfully and is listening on port 8080."
