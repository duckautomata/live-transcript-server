#!/bin/bash

# A script to update the Docker container with the latest image.
#
# Usage: ./update.sh

# --- Configuration ---
IMAGE_NAME="duckautomata/live-transcript-server"
TAG="latest"
CONTAINER_NAME="live_transcript_server"
RESTART_POLICY="unless-stopped"
CONFIG_FILE_PATH="./config.yaml"

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

if [ -f "$CONFIG_FILE_PATH" ]; then
    echo "Config file '$CONFIG_FILE_PATH' found."
else
    echo "Error: Config file '$CONFIG_FILE_PATH' does not exist."
    exit 1
fi

mkdir -p tmp

echo "Stopping and removing existing container: $CONTAINER_NAME"
$DOCKER_CMD stop $CONTAINER_NAME
$DOCKER_CMD rm -f $CONTAINER_NAME

echo "Pulling the latest image: $IMAGE_NAME:$TAG"
$DOCKER_CMD pull $IMAGE_NAME:$TAG

echo "Starting new container: $CONTAINER_NAME"
$DOCKER_CMD run \
    --name $CONTAINER_NAME \
    -d --restart $RESTART_POLICY \
    -v "$CONFIG_FILE_PATH:/app/config.yaml:ro" \
    -v "./tmp:/app/tmp" \
    -p 8080:8080 \
    $IMAGE_NAME:$TAG

echo "Update complete."
