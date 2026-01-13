#!/bin/bash

# Default values
DEFAULT_MODE="active"
DEFAULT_URL="ws://localhost:8080/test/websocket"
DEFAULT_DURATION="10s"
DEFAULT_CONNS="100"
DEFAULT_RATE="10"

echo "Performance Test Runner"
echo "-----------------------"

# Ask for mode
read -p "Enter mode (active/rate) [${DEFAULT_MODE}]: " MODE
MODE=${MODE:-$DEFAULT_MODE}

# Ask for URL
read -p "Enter WebSocket URL [${DEFAULT_URL}]: " URL
URL=${URL:-$DEFAULT_URL}

# Ask for duration
read -p "Enter duration [${DEFAULT_DURATION}]: " DURATION
DURATION=${DURATION:-$DEFAULT_DURATION}

# Construct arguments based on mode
ARGS="-mode ${MODE} -url ${URL} -d ${DURATION}"

if [ "$MODE" = "active" ]; then
    read -p "Enter number of connections [${DEFAULT_CONNS}]: " CONNS
    CONNS=${CONNS:-$DEFAULT_CONNS}
    ARGS="${ARGS} -c ${CONNS}"
elif [ "$MODE" = "rate" ]; then
    read -p "Enter rate (conn/sec) [${DEFAULT_RATE}]: " RATE
    RATE=${RATE:-$DEFAULT_RATE}
    ARGS="${ARGS} -r ${RATE}"
else
    echo "Invalid mode selected."
    exit 1
fi

echo "Running: go run cmd/perf-test/main.go ${ARGS}"
echo "-----------------------"

go run cmd/perf-test/main.go ${ARGS}
