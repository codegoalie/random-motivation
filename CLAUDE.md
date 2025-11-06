# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Go REST API (`github.com/codegoalie/random-motivation`) that serves random motivational quotes. Built with Echo framework and SQLite database for persistence.

### Features
- **GET /motivation** - Returns a random motivational quote
- **POST /motivation** - Adds a new motivational quote
- Automatic migration from text file to SQLite on first run
- Graceful shutdown with proper connection cleanup

## Development Commands

### Building and Running
```bash
go run main.go          # Run the application directly
go build               # Build the executable
go build -o motivation # Build with custom output name
./motivation           # Run the compiled binary
```

The server starts on `http://localhost:8080`

### Testing the API
```bash
# Get a random motivation
curl http://localhost:8080/motivation

# Add a new motivation
curl -X POST -d "Your motivation here" http://localhost:8080/motivation
```

### Go Module Management
```bash
go mod tidy            # Clean up dependencies
go mod download        # Download dependencies
```

### Testing
```bash
go test ./...          # Run all tests
go test -v ./...       # Run tests with verbose output
```

### Database Operations
```bash
# View database content
sqlite3 motivations.db "SELECT * FROM motivations;"

# Count motivations
sqlite3 motivations.db "SELECT COUNT(*) FROM motivations;"
```

## Project Structure

```
.
├── main.go                    # Entry point, HTTP handlers, server setup
├── db/
│   ├── db.go                  # Database connection and initialization
│   ├── migrations.go          # Schema creation
│   ├── repository.go          # Data access methods (CRUD operations)
│   └── migrate_text.go        # Text file to SQLite migration
├── go.mod                     # Go module definition (Go 1.25.3)
├── motivations.db             # SQLite database (generated at runtime)
├── motivations.txt.backup     # Backup of original text file
└── CLAUDE.md                  # This file
```

## Architecture Notes

### Database Schema

```sql
CREATE TABLE motivations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    text TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_motivations_created_at ON motivations(created_at);
```

### Key Components

1. **main.go** - Echo web server with routes and handlers
2. **db/db.go** - Database initialization and connection management
3. **db/repository.go** - Repository pattern with methods:
   - `GetRandom()` - Retrieves a random motivation
   - `Insert(text)` - Adds a new motivation
   - `GetAll()` - Lists all motivations
   - `Count()` - Returns total count
4. **db/migrate_text.go** - One-time migration from motivations.txt

### Data Migration

On first run, the application automatically:
1. Checks if `motivations.db` exists
2. If not, reads `motivations.txt` (if present)
3. Migrates all quotes to the database
4. Backs up the original file as `motivations.txt.backup`

### Configuration

Environment variable:
- `DB_PATH` - Database file path (default: `./motivations.db`)

### Dependencies

- `github.com/labstack/echo/v4` - Web framework
- `modernc.org/sqlite` - Pure Go SQLite driver (no CGO)

## Future Enhancements

Possible additions:
- GET /motivations - List all motivations with pagination
- PUT /motivation/:id - Update a specific motivation
- DELETE /motivation/:id - Remove a motivation
- Search and filtering capabilities
- Categories and tags
- User voting/favorites system