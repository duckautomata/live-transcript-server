#!/bin/bash

# A script to test the go program
#
# Usage: ./scripts/test.sh

# Change to the project root directory
cd "$(dirname "$0")/.."
echo "Running from project root: $PWD"

# Check if tparse is installed
if command -v tparse &> /dev/null; then
    go test -json ./internal/... | tparse
else
    echo "tparse not found. Falling back to standard verbose output."
    echo "Install it with: go install github.com/mfridman/tparse@latest"
    go test ./internal/...
fi
