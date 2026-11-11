# Agent Instructions

This file is the single source of agent guidance for this repository. `CLAUDE.md` is a symlink to it, so Claude Code and other agents read the same content and cannot drift apart.

## Project Overview

This is a Go REST API (`github.com/codegoalie/random-motivation`) that serves random motivational quotes. Built with Echo framework and SQLite database for persistence.

### Features
- **GET /motivation** - Returns a random motivational quote
- **POST /motivation** - Adds a new motivational quote
- **GET /motivations** - Lists all motivations
- **DELETE /motivation/:id** - Deletes a motivation by id
- Automatic migration from text file to SQLite on first run
- Graceful shutdown with proper connection cleanup

## Development Commands

The server starts on `http://localhost:8080`.

### Testing the API
```bash
# Get a random motivation
curl http://localhost:8080/motivation

# Add a new motivation
curl -X POST -d "Your motivation here" http://localhost:8080/motivation

# List all motivations
curl http://localhost:8080/motivations

# Delete a motivation by id
curl -X DELETE http://localhost:8080/motivation/1
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

## Architecture Notes

### Data Migration

On first run, the application automatically:
1. Checks if `motivations.db` exists
2. If not, reads `motivations.txt` (if present)
3. Migrates all quotes to the database
4. Backs up the original file as `motivations.txt.backup`

### Configuration

Environment variable:
- `DB_PATH` - Database file path (default: `./motivations.db`)

## Future Enhancements

Possible additions:
- Pagination for GET /motivations
- PUT /motivation/:id - Update a specific motivation
- Search and filtering capabilities
- Categories and tags
- User voting/favorites system

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation prompts.

Shell commands like `cp`, `mv`, and `rm` may be aliased to include `-i` (interactive) mode on some systems, causing the agent to hang indefinitely waiting for y/n input.

**Use these forms instead:**
```bash
# Force overwrite without prompting
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file

# For recursive operations
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

**Other commands that may prompt:**
- `scp` - use `-o BatchMode=yes` for non-interactive
- `ssh` - use `-o BatchMode=yes` to fail instead of prompting
- `apt-get` - use `-y` flag
- `brew` - use `HOMEBREW_NO_AUTO_UPDATE=1` env var

## Session Completion

**This section overrides the generated "Session Completion" guidance in the beads
block below, which says to always push and conflicts with the `bd prime` hook's
own "conservative by default" instruction.**

When code changed:
1. Commit in self-contained chunks using Conventional Commit messages.
2. Push to a **branch** — never directly to `main`. Open a PR when the work is
   ready for review.
3. Don't leave committed work unpushed at the end of a session; stranded local
   commits are the failure this rule exists to prevent.
4. File beads issues for follow-up work and close what you finished.

When nothing changed (questions, research, review), no git action is needed.

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
