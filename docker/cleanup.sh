#!/bin/bash

# A script to cleanup all images and containers.
#
# Usage: ./cleanup.sh

CONTAINER_NAME="live_transcript_server"
IMAGE_NAME="duckautomata/live-transcript-server"

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

# Find the container using an exact name match to prevent accidentally removing others.
CONTAINER_ID=$($DOCKER_CMD ps -a -q --filter "name=^${CONTAINER_NAME}$")

if [ -n "$CONTAINER_ID" ]; then
    echo "Stopping and removing container: $CONTAINER_NAME"
    $DOCKER_CMD stop "$CONTAINER_ID" > /dev/null
    $DOCKER_CMD rm -f "$CONTAINER_ID" > /dev/null
    echo "Container removed."
else
    echo "No container named '$CONTAINER_NAME' found."
fi

---

## Image Cleanup (with Confirmation)

echo # Add a newline for readability
read -p "Do you want to delete the '$IMAGE_NAME' image and prune all other unused images? (y/n) " -r
echo # Move to a new line after input

if [[ $REPLY =~ ^[Yy]$ ]]; then

    echo "Proceeding with image deletion..."
    # Find the specific image ID(s) to remove
    IMAGE_IDS=$($DOCKER_CMD images -q "$IMAGE_NAME")

    if [ -n "$IMAGE_IDS" ]; then
        echo "   -> Removing image: $IMAGE_NAME"
        $DOCKER_CMD rmi -f $IMAGE_IDS > /dev/null
    else
        echo "   -> No specific image named '$IMAGE_NAME' found to remove."
    fi

    echo "   -> Pruning all other unused images..."
    $DOCKER_CMD image prune -f

else
    echo "Skipping image deletion."
fi

echo -e "\nCleanup complete."
