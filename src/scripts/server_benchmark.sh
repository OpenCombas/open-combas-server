#!/bin/bash

#
# Open Combas Benchmark Runner (Linux/macOS)
# Simple wrapper to build and run the Go benchmark tool
#

set -e

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "\n======================================="
echo -e "   Open Combas Benchmark Suite"
echo -e "======================================="

# Get parameters
MODE="${1:-standard}"
OUTPUT_FILE="${2:-}"

echo -e "\nMode: $MODE"
if [ -n "$OUTPUT_FILE" ]; then
    echo "Output: $OUTPUT_FILE"
fi

# Check if Go is available
if ! [ -x "$(command -v go)" ]; then
    echo -e "${RED}❌ Go is not installed or not in PATH${NC}"
    exit 1
fi

echo -e "${GREEN}✅ Go is available${NC}"

# Build the benchmark runner
echo -e "\n🔨 Building benchmark runner..."
cd cmd/benchmark-runner
if ! go build -o ../../benchmark-runner .; then
    echo -e "${RED}❌ Failed to build benchmark runner${NC}"
    cd ../..
    exit 1
fi
cd ../..

echo -e "${GREEN}✅ Benchmark runner built successfully${NC}"

# Run the benchmark
echo -e "\n🚀 Running benchmarks..."
if [ -z "$OUTPUT_FILE" ]; then
    ./benchmark-runner -mode="$MODE"
else
    ./benchmark-runner -mode="$MODE" -output="$OUTPUT_FILE"
fi

BENCHMARK_EXIT=$?

# Cleanup
rm -f benchmark-runner

if [ $BENCHMARK_EXIT -eq 0 ]; then
    echo -e "\n${GREEN}✅ Benchmark completed successfully!${NC}"
else
    echo -e "\n${RED}❌ Benchmark failed${NC}"
fi

exit $BENCHMARK_EXIT