#!/bin/bash

# A script to cleanup all images and containers.
#
# Usage: ./cleanup.sh

# --- Pre-flight Checks ---

# 1. Determine the correct Docker command (docker or sudo docker)
if command -v docker &> /dev/null && docker info > /dev/null 2>&1; then
    DOCKER_CMD="docker"
elif command -v sudo &> /dev/null && sudo docker info > /dev/null 2>&1; then
    DOCKER_CMD="sudo docker"
else
    echo "Error: Docker is not running or you lack permission to use it."
    echo "Please ensure the Docker daemon is active and that your user is in the 'docker' group or has suda access."
    exit 1
fi

## Container Cleanup
$DOCKER_CMD compose stop
$DOCKER_CMD compose rm -rf
$DOCKER_CMD system prune -a -f

echo -e "\nCleanup complete."
