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

go run ./cmd/web/
