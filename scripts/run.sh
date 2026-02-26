#!/bin/bash

# A script to run the go program - mainly used for development.
#
# Usage: ./scripts/run.sh

# Change to the project root directory
cd "$(dirname "$0")/.."
echo "Running from project root: $PWD"

# Check if the config file exists.
if [ ! -f "./config.yaml" ]; then
    echo "Error: Configuration file not found at './config.yaml'"
    exit 1
fi

go run ./cmd/web/
