#!/bin/bash

# A script to run the go program - mainly used for development.
#
# Usage: ./scripts/run.sh

# This ensures the script always runs from the project's root directory.
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)
PROJECT_ROOT=$(dirname "$SCRIPT_DIR")
cd "$PROJECT_ROOT" || exit 1

echo "Running from project root: $PWD"

# Check if the config file exists.
if [ ! -f "./config.yaml" ]; then
    echo "Error: Configuration file not found at './config.yaml'"
    exit 1
fi

go run ./cmd/web/
