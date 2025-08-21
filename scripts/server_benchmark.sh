#!/bin/bash

set -e

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Ensure proper environment is loaded
# This handles cases where the script runs without inheriting user's PATH
load_user_environment() {
    # Try to source common profile files where Go PATH might be set
    local profile_files=(
        "$HOME/.bashrc"
        "$HOME/.bash_profile"
        "$HOME/.profile"
        "$HOME/.zshrc"
        "/etc/profile"
    )

    for profile in "${profile_files[@]}"; do
        if [[ -f "$profile" ]]; then
            echo -e "${BLUE}📋 Sourcing $profile${NC}"
            # Source safely - don't exit if profile has issues
            source "$profile" 2>/dev/null || true
        fi
    done
}

# Try to find Go in common installation locations
find_go_installation() {
    local common_go_paths=(
        "/usr/local/go/bin"
        "/usr/bin"
        "/usr/local/bin"
        "$HOME/go/bin"
        "$HOME/.local/bin"
        "/opt/go/bin"
        "/snap/bin"                    # Snap packages
        "$HOME/.asdf/shims"            # asdf version manager
        "$HOME/.g/go/bin"              # g version manager
        "/usr/local/Cellar/go/*/bin"   # Homebrew on macOS
    )

    for go_path in "${common_go_paths[@]}"; do
        if [[ -x "$go_path/go" ]]; then
            echo -e "${GREEN}🔍 Found Go at: $go_path/go${NC}"
            export PATH="$go_path:$PATH"
            return 0
        fi
    done

    return 1
}

# Main environment setup
setup_environment() {
    echo -e "${BLUE}🔧 Setting up environment...${NC}"

    # Load user environment first
    load_user_environment

    # Check if Go is now available
    if command -v go >/dev/null 2>&1; then
        echo -e "${GREEN}✅ Go found in PATH${NC}"
        return 0
    fi

    echo -e "${YELLOW}⚠️  Go not found in current PATH, searching common locations...${NC}"

    # Try to find Go installation
    if find_go_installation; then
        return 0
    fi

    return 1
}

echo -e "\n======================================="
echo -e "   Open Combas Benchmark Suite"
echo -e "======================================="

# Setup environment before proceeding
if ! setup_environment; then
    echo -e "${RED}❌ Go is not installed or not found in common locations${NC}"
    echo -e "${YELLOW}💡 Installation suggestions:${NC}"
    echo "   • Install Go from https://golang.org/dl/"
    echo "   • Or use package manager: apt install golang-go (Ubuntu/Debian)"
    echo "   • Or use package manager: brew install go (macOS)"
    echo "   • Make sure Go's bin directory is in your PATH"
    echo ""
    echo "Current PATH: $PATH"
    exit 1
fi

# Show Go version for confirmation
GO_VERSION=$(go version 2>/dev/null)
echo -e "${GREEN}✅ Using Go: $GO_VERSION${NC}"

# Get parameters
MODE="${1:-standard}"
OUTPUT_FILE="${2:-}"

echo -e "\nMode: $MODE"
if [ -n "$OUTPUT_FILE" ]; then
    echo "Output: $OUTPUT_FILE"
fi

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