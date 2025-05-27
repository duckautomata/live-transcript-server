#!/bin/bash

# A script to run the Docker image in the background.
#
# Usage: ./start.sh

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

# Check if a container with the same name is already running
if [ $($DOCKER_CMD ps -q -f name=$CONTAINER_NAME) ]; then
    echo "Error: A container '$CONTAINER_NAME' is already running."
    echo "Please stop or remove it first using ./cleanup.sh or update it with ./update.sh"
    exit 1
fi

echo "Starting container: $CONTAINER_NAME"
$DOCKER_CMD run \
    --name $CONTAINER_NAME \
    -d --restart $RESTART_POLICY \
    -v "$CONFIG_FILE_PATH:/app/config.yaml:ro" \
    -v "./tmp:/app/tmp" \
    -p 8080:8080 \
    $IMAGE_NAME:$TAG

echo "Container started successfully."
