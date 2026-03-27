#!/bin/bash

# A script to test the go program for flakiness by running tests 100 times.
#
# Usage: ./scripts/test-flaky.sh

# Change to the project root directory
cd "$(dirname "$0")/.."
echo "Running from project root: $PWD"

go clean -testcache && go test -count=100 -failfast ./internal/...
