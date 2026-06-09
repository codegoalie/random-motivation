# Random Motivation API

A simple REST API that serves random motivational quotes. Built with Go, Echo
framework, and SQLite.

## Features

- Get random motivational quotes
- Add new motivational quotes
- Persistent storage with SQLite
- Automatic migration from text file to database

## Quick Start

```bash
# Build the application
go build -o motivation

# Run the server
./motivation
```

The server will start on `http://localhost:8080`

## API Endpoints

### Get Random Motivation

```bash
curl http://localhost:8080/motivation
```

**Response:** A random motivational quote as plain text

**Status Codes:**

- `200 OK` - Success
- `404 Not Found` - No motivations in database
- `500 Internal Server Error` - Database error

### Add New Motivation

```bash
curl -X POST -d "Your motivational quote here" http://localhost:8080/motivation
```

**Response:** `Motivation added successfully`

**Status Codes:**

- `201 Created` - Successfully added
- `400 Bad Request` - Empty motivation
- `500 Internal Server Error` - Database error

## Configuration

Environment variables:

- `DB_PATH` - Database file path (default: `./motivations.db`)

Example:

```bash
DB_PATH=/var/data/motivations.db ./motivation
```

## Data Migration

On first run, if `motivations.txt` exists, the application will:

1. Migrate all quotes to the SQLite database
2. Back up the original file as `motivations.txt.backup`

This migration only happens once when the database is empty.

## Database

The application uses SQLite with the following schema:

```sql
CREATE TABLE motivations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    text TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

You can query the database directly:

```bash
sqlite3 motivations.db "SELECT * FROM motivations;"
```

## Development

```bash
# Install dependencies
go mod download

# Run without building
go run main.go

# Run tests
go test ./...

# Format code
go fmt ./...
```

## UAT

The repository ships a black-box User Acceptance Testing suite under
[`cmd/uat`](./cmd/uat). It interacts only via the public HTTP API and
process-level controls — it does not import application packages or
read the SQLite database directly.

### Existing-service mode

Run the suite against an already-running service. Only checks that are
safe against arbitrary deployments are executed (read-only checks plus
an eventual-retrieval check that adds one motivation):

```bash
go run ./cmd/uat --base-url http://localhost:8080
```

### Self-managed isolated mode

Have the UAT command supervise its own service subprocess with an
isolated database and a controlled fake render service, then run the
full suite:

```bash
go run ./cmd/uat --start-command "go run ." --base-url http://localhost:8080 --timeout 30s
```

The self-managed suite runs in several sequential groups, each with a
fresh database and a tailored `RENDER_SERVICE_URL`, so checks that
require deterministic single-entry queues or specific render failures
all get the state they expect.

### Useful flags

- `--timeout <duration>` — overall budget covering all groups
  (default `30s`).
- `--verbose` — print each HTTP request/response and stream the
  service subprocess output.
- `--skip-destructive` — skip checks that mutate state, useful when
  pointing the suite at a service whose data you must not change.
- `--render-url <url>` — explicitly point the suite at a controlled
  render endpoint in existing-service mode.

### Exit codes

- `0` — all checks passed.
- `1` — one or more behavioral checks failed.
- `2` — invalid CLI usage or configuration.

## Project Structure

```txt
.
├── main.go              # HTTP server and handlers
├── db/                  # Database package
│   ├── db.go           # Connection management
│   ├── migrations.go   # Schema definitions
│   ├── repository.go   # CRUD operations
│   └── migrate_text.go # Text file migration
├── cmd/
│   └── uat/            # Black-box UAT suite (see UAT section above)
└── motivations.db      # SQLite database (generated)
```

## Dependencies

- [Echo v4](https://echo.labstack.com/) - High performance Go web framework
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) - Pure Go SQLite driver

## License

This project is open source and available under the MIT License.
