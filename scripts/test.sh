#!/bin/bash

# A script to test the go program
#
# Usage: ./scripts/test.sh

# This ensures the script always runs from the project's root directory.
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)
PROJECT_ROOT=$(dirname "$SCRIPT_DIR")
cd "$PROJECT_ROOT" || exit 1

echo "Running from project root: $PWD"

# Check if tparse is installed
if command -v tparse &> /dev/null; then
    go test -json ./internal/... | tparse
else
    echo "tparse not found. Falling back to standard verbose output."
    echo "Install it with: go install github.com/mfridman/tparse@latest"
    go test ./internal/...
fi
