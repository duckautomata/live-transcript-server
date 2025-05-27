#!/bin/bash

# A script to cleanup all images and containers.
#
# Usage: ./cleanup.sh

CONTAINER_NAME="live_transcript_server"
IMAGE_NAME="duckautomata/live-transcript-server"

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

# --- Container Cleanup ---
# Get the list of container IDs to stop and remove
CONTAINER_IDS=$($DOCKER_CMD ps -a -q --filter "name=$CONTAINER_NAME")
if [ -n "$CONTAINER_IDS" ]; then
    echo "Stopping and removing container(s): $CONTAINER_NAME"
    $DOCKER_CMD stop $CONTAINER_IDS
    $DOCKER_CMD rm -f $CONTAINER_IDS
else
    echo "No containers found with name: $CONTAINER_NAME"
fi

# --- Image Cleanup (with Confirmation) ---
echo # Add a newline for readability
read -p "Do you want to delete the '$IMAGE_NAME' image and prune all other unused images? (y/n) " -r
echo # Move to a new line after input

if [[ $REPLY =~ ^[Yy]$ ]]; then

    echo "Proceeding with image deletion..."
    IMAGE_IDS=$($DOCKER_CMD images -q "$IMAGE_NAME")

    if [ -n "$IMAGE_IDS" ]; then
        echo "Removing image(s): $IMAGE_NAME"
        $DOCKER_CMD rmi -f $IMAGE_IDS
    else
        echo "No specific images found with name '$IMAGE_NAME' to remove."
    fi

    echo "Pruning all other unused images."
    $DOCKER_CMD image prune -f

else
    echo "Skipping image deletion."
fi

echo "Cleanup complete."
