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

### UAT (User Acceptance Testing)
The repository ships a black-box UAT suite under `cmd/uat` that interacts only
via the public HTTP API and process-level controls (no app imports, no direct
DB access).
```bash
# Against an already-running service (read-only + eventual-retrieval checks)
go run ./cmd/uat --base-url http://localhost:8080

# Self-managed isolated mode with fresh DB and fake render service
go run ./cmd/uat --start-command "go run ." --base-url http://localhost:8080 --timeout 30s

# Useful flags: --timeout, --verbose, --skip-destructive, --render-url
```
Exit codes: `0` all checks passed, `1` behavioral failures, `2` invalid CLI usage.

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
├── cmd/
│   └── uat/                   # Black-box UAT suite (see Development > UAT)
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

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->
