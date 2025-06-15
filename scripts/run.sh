#!/bin/bash

# A script to run the go program - mainly used for development.
#
# Usage: ./scripts/run.sh

if [[ "$PWD" == */scripts ]]; then
    echo "Error: This script must be run from the project's root directory, not from within the 'scripts' subdirectory."
    echo "You are currently in: $PWD"
    echo "Please change to the parent directory (e.g., 'cd ..') and run the script like this: ./scripts/run.sh"
    exit 1
fi

# Check if the config file exists.
if [ ! -f "./config.yaml" ]; then
    echo "Error: Configuration file not found at './config.yaml'"
    exit 1
fi

go run ./cmd/web/
