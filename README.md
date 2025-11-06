# Random Motivation API

A simple REST API that serves random motivational quotes. Built with Go, Echo framework, and SQLite.

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

## Project Structure

```
.
├── main.go              # HTTP server and handlers
├── db/                  # Database package
│   ├── db.go           # Connection management
│   ├── migrations.go   # Schema definitions
│   ├── repository.go   # CRUD operations
│   └── migrate_text.go # Text file migration
└── motivations.db      # SQLite database (generated)
```

## Dependencies

- [Echo v4](https://echo.labstack.com/) - High performance Go web framework
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) - Pure Go SQLite driver

## License

This project is open source and available under the MIT License.
