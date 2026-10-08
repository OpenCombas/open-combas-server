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

# Shop servers serve the in-game buy list. The four ports below are the ones
# the game polls; each answers with the same catalog, filtered by the faction
# byte in the request header.
[[ShopServers]]
Label = "SHOP_SERVER_NO_UPDATE"
Port = 1208

[[ShopServers]]
Label = "SHOP_SERVER_UPDATE"
Port = 1257

[[ShopServers]]
Label = "SHOP_SERVER_RETAIL"
Port = 1388

[[ShopServers]]
Label = "SHOP_SERVER_RETAIL_TITLE_UPDATE"
Port = 1397

```

## Shop Server

The shop endpoints answer the client's buy-list request with the parts catalog:
per faction, the complete-hound preset parts first, then the rest of the
faction's catalog, up to the 160-row limit the packet format allows. The reply
is byte-for-byte compatible with the TypeScript reference implementation
(`OpenCombas/open-combas-server-js`); `go test ./shop/` pins that parity against
golden packets generated from the reference sources.

The catalog lives in `shop/catalog.go`, generated from the reference data:

```bash
# Regenerate catalog.go and the golden packets from a checkout of the
# TypeScript reference server (needs node + esbuild).
scripts/regen-catalog.sh ../open-combas-server-js
go test ./shop/ -run TestBuildResponseMatchesGolden -v
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
