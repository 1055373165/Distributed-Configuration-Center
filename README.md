# PaladinCore — Distributed Configuration Center

A lightweight distributed configuration center inspired by etcd, built from scratch in Go.

## Architecture

```text
cmd/paladin-core/     CLI & HTTP entry point
server/               HTTP API layer (REST over ServeMux)
store/                Core KV storage interface + BoltDB implementation
```

**Key design:** configuration keys use hierarchical paths `{tenant}/{namespace}/{name}`, giving multi-tenant namespace isolation without a relational database.

Each key tracks etcd-style versioning:

- **Revision** — global monotonic counter (logical clock for the whole store)
- **CreateRevision** — revision when the key was first created
- **ModRevision** — revision when the key was last modified
- **Version** — per-key mutation count (starts at 1)

## Quick Start

```bash
# Build
go build -o paladin-core ./cmd/paladin-core

# CLI usage
./paladin-core put public/prod/db_host 10.0.0.1
./paladin-core get public/prod/db_host
./paladin-core list public/prod/
./paladin-core delete public/prod/db_host
./paladin-core rev
```

## HTTP API

Base path: `/api/v1/config/{tenant}/{namespace}/{name}`

| Method   | Path                                      | Description          | Status        |
|----------|-------------------------------------------|----------------------|---------------|
| `PUT`    | `/api/v1/config/public/prod/db_host`      | Create or update key | 201 / 200     |
| `GET`    | `/api/v1/config/public/prod/db_host`      | Get single key       | 200 / 404     |
| `GET`    | `/api/v1/config/public/prod/`             | List by prefix       | 200           |
| `DELETE` | `/api/v1/config/public/prod/db_host`      | Delete key           | 200 / 404     |
| `GET`    | `/healthz`                                | Health check         | 200           |

PUT response includes `X-Paladin-Revision` header.

**Example:**

```bash
# Create
curl -X PUT -d '10.0.0.1' http://localhost:8080/api/v1/config/public/prod/db_host

# Read
curl http://localhost:8080/api/v1/config/public/prod/db_host

# List namespace
curl http://localhost:8080/api/v1/config/public/prod/

# Delete
curl -X DELETE http://localhost:8080/api/v1/config/public/prod/db_host
```

## Testing

```bash
go test -count=1 ./server/
```

## Tech Stack

- **Go** — standard library `net/http` for HTTP, no third-party web framework
- **BoltDB** (`go.etcd.io/bbolt`) — embedded KV store with serializable transactions
