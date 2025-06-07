# Open Combas Server

OpenCombas server repository.

## Quick Start

### Prerequisites
- [Go 1.21+](https://golang.org/dl/)

### Installation


### Linux
```bash
# Clone the repository
git clone https://github.com/your-org/open-combas-server.git
cd open-combas-server

# Build the server
go build -o open-combas-server .

# Run the server
./open-combas-server
```

### Windows
```batch
go build -o open-combas-server.exe .
open-combas-server.exe
```

## Configuration

The server auto-generates a `config.toml` file on first run:

```toml
ServerStatusPort = 1207
ListeningAddress = "0.0.0.0"
BufferSize = 4000

# Performance and logging options.
PerfReportIntervalSec = 30
EnablePerformanceMonitoring = false
VerboseLogging = false

[[EchoingServers]]
Label = "WORLD"
Port = 1215

[[EchoingServers]]
Label = "WORLD_OLD"
Port = 1255

```

## Performance Testing

Run comprehensive benchmarks:

```bash
# Windows
server_benchmark.bat standard results.txt

# Linux/macOS
./server_benchmark.sh standard results.txt
```

## Development

```bash
# Run tests
go test ./...

# Run benchmarks
go test -bench=. ./...

# Quick performance check
run-benchmark.bat quick
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Run tests: `go test ./...`
4. Run benchmarks: `./server_benchmark.sh standard`
5. Submit a pull request
