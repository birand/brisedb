# brisedb

A distributed key-value store with volume-based blob storage, replication across multiple machines and drives, and an HTTP API optimised for values between 1 MB and 1 GB. Inspired by [SeaweedFS](https://github.com/seaweedfs/seaweedfs), but simple.

---

## Features

- **Persistent storage** — append-only volume files + WAL for crash recovery
- **On-disk hash index** — O(1) key lookup with 2 disk seeks; keys do not need to fit in RAM
- **Multi-drive support** — spread volumes across multiple local directories
- **Remote volume servers** — blob data can live on separate machines over HTTP
- **Replication** — each blob written to N servers simultaneously; reads auto-failover
- **HTTP blob API** — streaming PUT/GET/DELETE with `Range` header and TTL
- **TCP command protocol** — Redis-inspired text protocol for key management
- **Transactions** — nested `BEGIN` / `COMMIT` / `ROLLBACK` with per-connection isolation
- **TTL / expiry** — `SET key value EX 60`, lazy eviction + background sweep
- **Pub/Sub** — `SUBSCRIBE` / `PUBLISH` / `UNSUBSCRIBE`
- **WAL replication** — stream changes to read-only replicas in real time
- **Compaction** — rewrite index and WAL, discarding deleted and expired entries

---

## Quick start

```bash
# Build everything
go build ./...

# Start a single-node server (TCP :6380, HTTP blob API :6381)
./server --data ./data --http :6381

# Store and retrieve a value via TCP
echo "SET hello world" | nc localhost 6380   # +OK
echo "GET hello"       | nc localhost 6380   # +world

# Store and retrieve a blob via HTTP
curl -X PUT http://localhost:6381/v1/blobs/myfile --data-binary @photo.jpg
curl     http://localhost:6381/v1/blobs/myfile -o photo-copy.jpg
```

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                    brisedb master                   │
│                                                     │
│  TCP server (:6380)     HTTP blob API (:6381)       │
│       │                        │                    │
│       └──────────┬─────────────┘                    │
│                  │                                  │
│             BriseDB core                            │
│          ┌────────────────┐                         │
│          │  hash index    │  index.hash (on disk)   │
│          │  WAL           │  wal.log                │
│          │  VolumePool    │                         │
│          └───────┬────────┘                         │
│                  │                                  │
│        ┌─────────┼──────────┐                       │
│   local vol   remote     remote                     │
│   (drives)    server 1   server 2                   │
└─────────────────────────────────────────────────────┘
                   │ HTTP
           ┌───────┴───────┐
      volume-server   volume-server
      (:8081)          (:8082)
```

Each **volume file** (`vol-000001.data`) is an append-only binary file.
A **volume group** (created when replication is enabled) holds N identical copies across N servers — because all members receive the same writes in order, offsets are identical, allowing any member to serve a read.

---

## Binaries

| Binary | Description |
|---|---|
| `./server` | Master node — TCP protocol + optional HTTP blob API |
| `./volume-server` | Standalone blob storage node (HTTP only) |
| `./replica` | Read-only replica that streams WAL from master |
| `./cli` | Interactive REPL for local embedded use |

---

## TCP protocol

Connect with `nc`, `redis-cli -3`, or the Go client (`pkg/client`).

### Key-value

```
SET key value             → +OK
SET key value EX 60       → +OK   (TTL 60 seconds)
GET key                   → +value | +nil
DELETE key                → +OK
TTL key                   → +<seconds> | +-1 (no TTL) | +-2 (not found)
PERSIST key               → +1 | +0
COUNT value               → +<n>   (number of keys whose value equals value)
KEYS pattern              → +<n>
                             +key1
                             +key2 ...
SCAN cursor COUNT n       → +nextCursor n
                             +key1 ...
```

### Transactions

```
BEGIN
SET x 1
SET y 2
COMMIT            → +OK  (writes to volumes + WAL atomically)

BEGIN
SET x bad
ROLLBACK          → +OK  (discards all changes)
```

Transactions nest — each `BEGIN` pushes a new savepoint. `COMMIT` merges into the parent transaction or the main store.

### Pub/Sub

```
SUBSCRIBE news sports      → +SUBSCRIBE news 1
PUBLISH  news "breaking"   → +1
# subscribers receive:       +MESSAGE news breaking
UNSUBSCRIBE news           → +UNSUBSCRIBE news 0
```

### Admin

```
COMPACT    → rewrite index + WAL, reclaim space from deleted/expired keys
VOLINFO    → list volume files: +<id> <drive> <size_bytes>
STOP       → close this connection
```

---

## HTTP blob API

Start the HTTP server with `--http :6381` or set `http_addr` in the config file.

### Endpoints

| Method | Path | Description |
|---|---|---|
| `PUT` | `/v1/blobs/{key}` | Store blob; optional `?ttl=30s` |
| `GET` | `/v1/blobs/{key}` | Retrieve blob; supports `Range` header |
| `HEAD` | `/v1/blobs/{key}` | Metadata only (no body) |
| `DELETE` | `/v1/blobs/{key}` | Remove blob |
| `GET` | `/v1/keys?pattern=*` | List keys matching glob |
| `GET` | `/v1/scan?cursor=0&count=100` | Paginated key scan |
| `GET` | `/v1/status` | Health check + volume stats |

### Examples

```bash
# Store with TTL
curl -X PUT "http://localhost:6381/v1/blobs/session:abc?ttl=1h" \
     --data-binary @token.bin

# Retrieve full blob
curl http://localhost:6381/v1/blobs/session:abc -o token.bin

# Resume a partial download (Range)
curl -H "Range: bytes=1000000-" \
     http://localhost:6381/v1/blobs/bigfile >> partial.bin

# Check metadata without downloading
curl -I http://localhost:6381/v1/blobs/myfile
# X-Brise-Size: 104857600
# X-Brise-TTL: 3542   (remaining seconds; -1 = no expiry)

# List keys
curl "http://localhost:6381/v1/keys?pattern=session:*"
# {"keys":["session:abc","session:xyz"],"count":2}

# Paginated scan
curl "http://localhost:6381/v1/scan?cursor=0&count=100"
# {"next_cursor":100,"keys":[...],"count":100}
```

---

## Multi-machine cluster

### 1. Start volume servers on storage nodes

```bash
# node2
./volume-server --addr :8081 --data /mnt/ssd1

# node3
./volume-server --addr :8082 --data /mnt/ssd2
```

### 2. Configure and start the master

```json
{
  "addr": ":6380",
  "http_addr": ":6381",
  "data_dir": "/var/brisedb",
  "volume_servers": ["http://node2:8081", "http://node3:8082"],
  "replication_factor": 2,
  "max_volume_size": 2147483648
}
```

```bash
./server --config cluster.json
```

With `replication_factor: 2`, every blob is written to 2 backends simultaneously (local + one remote, assigned round-robin per volume group). If a server goes down, reads automatically fail over to the surviving replica.

### 3. Start a read replica (optional)

```bash
./replica --master localhost:6380 --data /var/brisedb-replica --addr :6382
```

The replica streams WAL entries from the master in real time and serves read-only queries on its own TCP port.

---

## Configuration reference

All fields are optional; command-line flags override file values.

```json
{
  "addr": ":6380",
  "http_addr": ":6381",
  "data_dir": "brisedb-data",
  "drives": ["/mnt/disk2/volumes", "/mnt/disk3/volumes"],
  "volume_servers": ["http://10.0.0.2:8081"],
  "replication_factor": 2,
  "max_volume_size": 2147483648
}
```

| Field | Default | Description |
|---|---|---|
| `addr` | `:6380` | TCP server listen address |
| `http_addr` | _(disabled)_ | HTTP blob API listen address |
| `data_dir` | `brisedb-data` | Primary data directory |
| `drives` | `[]` | Extra local drive directories for volume files |
| `volume_servers` | `[]` | Remote HTTP volume server base URLs |
| `replication_factor` | `1` | Blob copies per write (1 = no replication) |
| `max_volume_size` | `2147483648` | Max bytes per volume file before rotation (2 GiB) |

---

## Go client

```go
import "github.com/birand/brisedb/pkg/client"

c, err := client.Dial("localhost:6380")
if err != nil { ... }
defer c.Close()

c.Set("name", "Alice")
val, ok := c.Get("name")      // "Alice", true
c.SetEX("token", "xyz", 60)   // expires in 60 s
c.Delete("name")

// Transactions
c.Begin()
c.Set("x", "1")
c.Set("y", "2")
c.Commit()

// Pub/Sub
sub, _ := c.Subscribe("news")
go func() {
    for msg := range sub {
        fmt.Println(msg.Channel, msg.Payload)
    }
}()
c.Publish("news", "hello")

// WAL replication stream
r, _ := client.DialReplica("localhost:6380")
for entry := range r.Entries() {
    fmt.Println(entry.Type, entry.Key)
}
```

---

## Storage layout

```
brisedb-data/
├── index.hash          # persistent hash index (key → NeedleAddr)
├── wal.log             # write-ahead log (needle addresses, no values)
├── vol-registry.json   # volume group registry (globalID → servers)
└── volumes/
    ├── vol-000000.data
    ├── vol-000001.data
    └── ...
```

- **`index.hash`** — open-addressing hash table, append-only. Default 1 M buckets (8 MiB). `COMPACT` rewrites it, removing tombstones and expired entries.
- **`wal.log`** — one JSON line per committed SET/DELETE. Only used when the hash index is empty (crash recovery / first open). `COMPACT` rewrites it to match the live index.
- **Volume files** — `[8-byte big-endian size][payload bytes]` per entry. Never overwritten; values accumulate until the file reaches `max_volume_size`, then a new file is created in the same drive.

---

## Benchmarks

Measured on Apple M1, Go 1.24, local disk (`-benchtime=1s`).

### Hot-path operations

| Benchmark | ops/s | ns/op | allocs/op |
|---|---|---|---|
| Set (single key, WAL flush) | ~92 K | 10 821 | 10 |
| Get (cache miss) | ~568 K | 2 143 | 3 |
| Get (miss / not found) | ~42 M | 28 | 0 |
| Delete | ~691 K | 1 482 | 3 |
| SetBlob 1 KB | ~92 K | 13 028 | 10 |
| SetBlob 64 KB | ~12 K | 104 181 | 10 |
| SetBlob 1 MB | ~866 | 1 513 876 | 13 |
| SetBlobStream 1 MB (zero-copy) | ~1 797 | 1 023 929 | — |
| GetBlob 1 KB | ~427 K | 2 420 | 2 |
| GetBlob 64 KB | ~149 K | 7 983 | 2 |
| GetBlob 1 MB (cached) | ~1 M | 1 062 | 1 |
| Transaction (1 key) | ~106 K | 11 221 | 19 |
| Transaction (10 keys) | ~10 K | 103 723 | 104 |
| Rollback | ~5 M | 231 | 6 |
| HashIndex Set | ~310 K | 3 544 | 1 |
| HashIndex Get | ~1 M | 1 041 | 1 |
| HashIndex GetMiss | ~84 M | 14 | 0 |
| Set (8 goroutines, WAL group-commit) | ~135 K | 8 830 | 9 |
| Mixed 80%R/20%W (8 goroutines) | ~283 K | 4 125 | 5 |

### ForEach / Compact — memory vs. speed trade-off

`ForEach` and `Compact` iterate the on-disk bucket table instead of loading all
keys into a `map[string]...`.  RAM usage scales with chain depth (O(1) in
practice) rather than with the total number of live keys.

| Operation | Keys | RAM before | RAM after | Δ RAM | Speed before | Speed after |
|---|---|---|---|---|---|---|
| ForEach | 1 K | 283 KB | 8 KB | **−97%** | 1.4 ms | 12.8 ms |
| ForEach | 10 K | 2.2 MB | 78 KB | **−96%** | 14.8 ms | 49.5 ms |
| ForEach | 100 K | 19.6 MB | 1.5 MB | **−92%** | 148 ms | 496 ms |
| Compact | 1 K | 8.9 MB | 8.4 MB | −6% | 20 ms | 45 ms |
| Compact | 10 K | 12.5 MB | 8.5 MB | **−32%** | 77 ms | 130 ms |
| Compact | 100 K | 43.6 MB | 9.9 MB | **−77%** | 569 ms | 943 ms |

**Trade-off:** the bucket-chain scan avoids a global key map (prevents OOM on
large datasets) but traverses the full 8 MiB bucket table on every call — a
fixed overhead that dominates for small key counts.  Compact is typically
run infrequently (e.g. via `COMPACT` command or a nightly cron), so the
latency increase is acceptable in exchange for bounded memory usage.

Run benchmarks:

```bash
go test ./pkg/brisedb/ -bench=. -benchtime=1s -benchmem
```

---

## Development

```bash
# Run all tests
go test ./...

# Verbose output for one package
go test -v ./pkg/brisedb/

# Run a specific test
go test ./pkg/brisedb/ -run TestTransaction

# Build all binaries
go build ./cmd/server/ ./cmd/volume-server/ ./cmd/replica/ ./cmd/cli/
```
