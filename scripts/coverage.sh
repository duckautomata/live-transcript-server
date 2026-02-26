#!/bin/bash

# A script to gather coverage data for the go program and generate a coverage report.
#
# Usage: ./scripts/coverage.sh

# Change to the project root directory
cd "$(dirname "$0")/.."
echo "Running from project root: $PWD"

if go test -coverprofile=coverage.out ./internal/...; then
    go tool cover -html=coverage.out -o coverage.html
else
    echo ""
    echo "Tests failed!"
    exit 1
fi

echo ""
echo "To view a visual HTML report on a remote server:"
echo "  1. Serve the report:    python3 -m http.server 8080"
echo "  2. Locally:             Open http://<server-ip>:8080/coverage.html"
