# Razpravljalnica - Distributed Discussion System

A distributed discussion system built with Go, gRPC, and the Raft consensus algorithm. This project implements a fault-tolerant, replicated state machine for managing discussions across multiple nodes.

## Project Structure

- **`cmd/`** - Command-line interface commands (client, server, control plane, root)
- **`server/`** - Server implementation with gRPC service
- **`client/`** - Client implementation for communicating with servers
- **`controlplane/`** - Control plane logic for cluster coordination
- **`proto/`** - Protocol buffer definitions and generated gRPC code

## Architecture

The project consists of three main components:

1. **Control Plane** - Manages cluster coordination using Raft consensus
2. **Server** - gRPC server that handles client requests
3. **Client** - Client application for interacting with the distributed system

## Prerequisites

- Go 1.16 or higher
- Protocol Buffers compiler (protoc) - for modifying `.proto` files

## Building

Build the project into a single executable:

```bash
cd razpravljalnica
go build -o razpravljalnica.exe .
```

On macOS or Linux (omit the `.exe` extension):
```bash
go build -o razpravljalnica .
```

## Running the Project

### Using `go run`

All commands are run from the `razpravljalnica/` directory.

#### Control Plane

```bash
go run . control-plane [FLAGS]
```

Flags:
- `--addr` - gRPC server address (default: 6000)
- `--raft` - Raft consensus port (default: 7000)
- `--id` - Node ID (required)
- `--bs` - Bootstrap flag (default: false, use `--bs` for true or `--bs=false` for false)
- `--leader` - Leader address (default: localhost:6000)

```bash
#Example:
go run . control-plane --addr 6000 --raft 7000 --id 1 --bs --leader 6000
go run . control-plane --addr 6001 --raft 7001 --id 2 --bs=false --leader 6000
```
#### Server

```bash
go run . server [FLAGS]
```

Flags:
- `--addr` - Server address (default: 50051)

#### Client

```bash
go run . client
```
```bash
#Example:
go run . server --addr 50051
go run . server --addr 50052
go run . server --addr 50053
```

### Using Compiled Binary

```bash
./razpravljalnica.exe control-plane --addr 6000 --raft 7000 --id 1 --bs --leader 6000
./razpravljalnica.exe server --addr 50051
./razpravljalnica.exe client
```
On Linux and Mac ./razpravljalnica you avoid .exe.

## Example: Running a Multi-Node Cluster

Start a cluster with 5 nodes:

```bash
# Terminal 1 - Node 1 (Leader)
go run . control-plane --addr 6000 --raft 7000 --id 1 --bs --leader 6000

# Terminal 2 - Node 2
go run . control-plane --addr 6001 --raft 7001 --id 2 --bs=false --leader 6000

# Terminal 3 - Node 3
go run . control-plane --addr 6002 --raft 7002 --id 3 --bs=false --leader 6000

# Terminal 4 - Node 4
go run . control-plane --addr 6003 --raft 7003 --id 4 --bs=false --leader 6000

# Terminal 5 - Node 5
go run . control-plane --addr 6004 --raft 7004 --id 5 --bs=false --leader 6000
```

**Important Note:** The control plane expects Raft addresses in the format `localhost:RAFT_PORT+1000` (e.g., port 7000 becomes `localhost:8000` for gRPC communication).

## Testing

### Run All Tests
You need to be in folders of a client, server or controleplane to run tests.

```bash
go test
```

With verbose output:
```bash
go test -v
```

### Server Tests

The server tests verify service methods without requiring control plane or other dependencies.

### Control Plane Tests

```bash
go test
```

### Fuzzing

Test with fuzzing (example with FuzzCreateUser):
```bash
go test -fuzz=FuzzCreateUser -fuzztime=100s
```

## Protocol Buffers

### Regenerating Proto Files

If you modify `razpravljalnica.proto`, regenerate the Go files:

```bash
cd proto
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative razpravljalnica.proto
```

This generates:
- `razpravljalnica.pb.go` - Data structures defined in the proto file
- `razpravljalnica_grpc.pb.go` - gRPC service definitions

### Implementing gRPC Services

Implement new service methods with this signature:

```go
func (s *server) Create(ctx context.Context, in *protobufStorage.Todo) (*emptypb.Empty, error) {
    // implementation
}
```

## Storage

The project uses Bolt database for persistent storage. You'll find the following files:

- `raft-N-log.bolt` - Raft log storage for node N
- `raft-N-stable.bolt` - Stable state storage for node N
- `snapshots-N/` - Snapshot directory for node N

These files are created automatically when nodes start up.

## Adding New Flags

To add a new flag:

1. Open the relevant command file in `cmd/` (e.g., `server.go`, `controlPlane.go`)
2. Add the flag to the command's flags section
3. Access the flag value in the `Run` function of that command

## Development Notes

- All services use localhost for communication if you want to change that look at the start of a run comment there I join strings with localhost: + port number if you delete that line it wont run just on localhost but it will scan everything for this open ports  
- Default ports: Control Plane (6000-6004), Raft (7000-7004), Server (50051)
- The `--id` flag is mandatory for control plane nodes
- At least one control plane node must be started with `--bs` flag to bootstrap the cluster (to start it with default/first leader)
- Raft consensus ensures consistency across all nodes in case of failures

## Project Context

This is a distributed systems project built as part of an academic coursework on distributed systems (Porazdaljeni Sistemi). The implementation demonstrates key concepts including:

- Consensus algorithms (Raft)
- gRPC for inter-process communication
- Fault tolerance through replication
- Cluster coordination

