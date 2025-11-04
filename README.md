# Distributed Bitcoin Miner

A complete, production-style Go project that simulates a Bitcoin mining pool running on top of a custom reliable UDP protocol. The system demonstrates how to build fault-tolerant distributed services from the transport layer up: clients submit hashing jobs, a central server orchestrates work distribution, and a fleet of miners compete to return the lowest hash.

---

## Why this project is interesting
- **Custom reliability layer.** Implements a Lightweight Session Protocol (`src/github.com/cmu440/lsp`) with sliding windows, retransmissions, exponential backoff, and epoch-based failure detection to provide TCP-like semantics over UDP.
- **Resilient work scheduler.** The server (`src/github.com/cmu440/bitcoin/server/server.go`) chunks jobs, load-balances miners, and eagerly redistributes stalled work when nodes drop.
- **End-to-end ownership.** Covers everything from CLI tooling (`client`, `miner`, `server`) to message serialization and binary packaging (`bin/`).
- **Well-tested core.** Includes a comprehensive test suite for the transport layer (`src/github.com/cmu440/lsp/lsp*_test.go`) plus sanity binaries for the application protocols.

---

## Architecture

```
client → LSP client → bitcoin server → LSP server → miners
```

- **Client** (`src/github.com/cmu440/bitcoin/client`): CLI that submits a message and nonce search space, then blocks waiting for the best result.
- **Server** (`src/github.com/cmu440/bitcoin/server`): Maintains miner membership, breaks jobs into 10k-nonce chunks, tracks outstanding work, and replies to clients with the best hash observed.
- **Miner** (`src/github.com/cmu440/bitcoin/miner`): Joins the pool, brute-forces assigned nonce ranges, and streams intermediate results back to the server.
- **LSP transport** (`src/github.com/cmu440/lsp`): Shared by all binaries; handles connection lifecycle, ordered delivery, retransmissions, and configurable reliability parameters.
- **Support binaries** (`bin/`): Pre-built sanity test harnesses for different platforms.

---

## Getting Started

1. **Prerequisites**
   - Go 1.20+ (module mode is enabled via `src/github.com/cmu440/go.mod`).
   - macOS/Linux/Windows terminal with access to `go` tooling.

2. **Clone & bootstrap**
   ```bash
   git clone <repo-url>
   cd bitcoin-miner
   go mod tidy
   ```

3. **Build all components**
   ```bash
   go install github.com/cmu440/bitcoin/server
   go install github.com/cmu440/bitcoin/miner
   go install github.com/cmu440/bitcoin/client
   ```

4. **Run the system**
   ```bash
   # Terminal 1 – start the server
   server 6060

   # Terminal 2 – launch one or more miners
   miner localhost:6060
   miner localhost:6060

   # Terminal 3 – submit work
   client localhost:6060 "distributed-systems" 200000
   ```
   The client prints `Result <hash> <nonce>` once the server aggregates responses. If connectivity drops, it prints `Disconnected`.

---

## Testing & Tooling

- **Transport layer tests**
  ```bash
  cd src/github.com/cmu440/lsp
  go test ./...
  go test -race ./...
  ```
- **Application sanity checks**
  - Build the platform-specific binaries (`go build`) inside `bitcoin/miner` and `bitcoin/client`.
  - Run the provided harnesses (e.g., `bin/darwin/arm64/ctest`, `bin/darwin/arm64/mtest`) to confirm baseline behavior.

---

## Implementation Highlights

- **Reliable messaging over UDP** (`src/github.com/cmu440/lsp/client_impl.go`, `src/github.com/cmu440/lsp/server_impl.go`)
  - Connection handshake with randomized ISNs, ACK tracking, and application-facing channels for non-blocking writes.
  - Sliding window plus `MaxUnackedMessages` enforcement to avoid flooding slow peers.
  - Epoch timer that triggers retransmissions and connection teardown after configurable limits.
  - Exponential backoff per-message to handle congestion without global pauses.
- **Adaptive job scheduler** (`src/github.com/cmu440/bitcoin/server/server.go`)
  - FIFO request queue for fairness between clients.
  - Chunking (`chunkSize = 10,000`) to keep miners busy without monopolizing the pool.
  - Redo queue for in-flight work when miners disconnect or fail to ACK.
  - Best-hash tracking that only updates when a strictly better candidate is found.
- **Stateless miners and client UX**
  - Miners work streaming assignments and can be scaled horizontally without coordination.
  - Client prints deterministic output expected by grading scripts while gracefully handling server failures.

---

## Repository Layout

```
README.md
src/github.com/cmu440/
 ├── bitcoin/
 ├── lsp/
 ├── lspnet/
 ├── srunner/
 └── crunner/
bin/
sh/
```

---

## Ideas for future work

- Turn the scheduler into a dynamic work-stealing pool.
- Promote transport parameters to CLI flags to make tuning easier.
- Wrap the cluster in Docker Compose for one-command demos.

---

_Reach out if you'd like a guided walkthrough or a live demo._
