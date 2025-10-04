#!/bin/bash

# A script to run the Docker image in the background.
#
# Usage: ./start.sh

# Exit immediately if a command exits with a non-zero status.
set -e

# --- Configuration ---
CONTAINER_NAME_A="live_transcript_server"
CONTAINER_NAME_B="prometheus"
CONTAINER_NAME_C="grafana"

# --- Path and Environment Setup ---
# This ensures the script always runs relative to its own location.
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)
CONFIG_FILE_PATH="$SCRIPT_DIR/config.yaml"
PROMETHEUS_FILE_PATH="$SCRIPT_DIR/prometheus.yaml"
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
if [ ! -f "$PROMETHEUS_FILE_PATH" ]; then
    echo "Error: Prometheus file not found at '$PROMETHEUS_FILE_PATH'"
    exit 1
fi
echo "  Config files found"

# Create necessary host directories
mkdir -p "$TMP_DIR"

# --- Docker Command ---
$DOCKER_CMD compose up -d

# --- Post-run Check ---
# Give the container a moment to start or fail
sleep 3
if ! $DOCKER_CMD ps -q -f name="^${CONTAINER_NAME_A}$" > /dev/null; then
    echo "Error: Container failed to start. Check logs for details:"
    echo "   $DOCKER_CMD logs $CONTAINER_NAME_A"
else
    echo "Container '$CONTAINER_NAME_A' started successfully and is listening on port 8080."
fi

if ! $DOCKER_CMD ps -q -f name="^${CONTAINER_NAME_B}$" > /dev/null; then
    echo "Error: Container failed to start. Check logs for details:"
    echo "   $DOCKER_CMD logs $CONTAINER_NAME_B"
else
    echo "Container '$CONTAINER_NAME_B' started successfully and is listening on port 8090."
fi

if ! $DOCKER_CMD ps -q -f name="^${CONTAINER_NAME_C}$" > /dev/null; then
    echo "Error: Container failed to start. Check logs for details:"
    echo "   $DOCKER_CMD logs $CONTAINER_NAME_C"
else
    echo "Container '$CONTAINER_NAME_C' started successfully and is listening on port 3000."
fi