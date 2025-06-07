# Open Combas Benchmark Suite

A comprehensive, cross-platform benchmarking system for the Open Combas server built in Go.

## Quick Start

### **Windows (Recommended)**
```batch
# Quick test (minimal, fast)
./scripts/run-benchmark.bat quick

# Standard test (recommended)
./scripts/run-benchmark.bat standard

# Extensive test with report
./scripts/run-benchmark.bat extensive results.txt
```

### **Linux/macOS**
```bash
# Quick test (skips unit tests)
./run-benchmark.sh quick

# Standard test (recommended) 
./run-benchmark.sh standard

# Extensive test with report
./run-benchmark.sh extensive results.txt
```

### **Manual Go Execution**
```bash
# Build and run manually
cd cmd/benchmark-runner
go build -o ../../benchmark-runner .
cd ../..
./benchmark-runner -mode=standard -output=results.txt
```

## Benchmark Modes

| Mode | Duration | Tests Included | Use Case |
|------|----------|----------------|----------|
| **quick** | ~30 seconds | Server benchmarks only | Quick validation |
| **standard** | ~2-3 minutes | Unit tests + benchmarks + server tests | Development workflow |
| **extensive** | ~5-10 minutes | All tests + sustained load testing | Performance comparison |

## 🔧 What Gets Tested

### **Build Phase**
- Compiles the Open Combas server
- Builds the benchmark tool
- Validates Go environment

### **Testing Phase** (standard/extensive modes)
- Go unit tests (`go test ./...`)
- Go benchmark tests (`go test -bench=. ./...`)
- Integration tests

### **Server Benchmarking Phase**
- **Status Server** (Port 1207) - Chromehounds protocol testing
- **Echo Servers** (Ports 1215, 1255) - Simple relay testing
- **Concurrent Clients** - 1, 10, 50 client load testing
- **Memory Tracking** - Allocation patterns and GC impact
- **Latency Measurements** - Response time analysis
- **Throughput Metrics** - Packets/second, bytes/second

## Troubleshooting

### **"Go is not installed"**
- Install Go from https://golang.org/dl/
- Ensure `go` is in your PATH

### **"Port already in use"**
- Stop any running Open Combas servers
- Check with `netstat -an | findstr :1207` (Windows) or `lsof -i :1207` (Linux/macOS)

### **"Failed to build"**
- Ensure you're in the project root directory
- Run `go mod tidy` to fix dependencies
- Check that `go.mod` exists

### **Permission Issues (Linux/macOS)**
```bash
chmod +x run-benchmark.sh
```