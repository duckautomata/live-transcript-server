#!/bin/bash

# A script to update the Docker image.
#
# Usage: ./update.sh

# --- Configuration ---
CONTAINER_NAME="live_transcript_server"
TAG="latest"
RESTART_POLICY="unless-stopped"
CONFIG_FILE_PATH="./config.yaml"

if [ -f "$CONFIG_FILE_PATH" ]; then
    echo "Config file '$CONFIG_FILE_PATH' found."
else
    echo "Error: Config file '$CONFIG_FILE_PATH' does not exist."
    exit 1
fi

mkdir -p tmp

docker stop $NAME
docker rm -f $NAME
docker rmi -f duckautomata/live-transcript-server:$TAG
docker run \
    --name $CONTAINER_NAME \
    -d --restart $RESTART_POLICY \
    -v "$CONFIG_FILE_PATH:/app/config.yaml:ro" \
    -v "./tmp:/app/tmp" \
    -p 8080:8080 \
    duckautomata/live-transcript-server:$TAG
