# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Go project (`github.com/codegoalie/random-motivation`) that appears to be in its initial stages. Currently contains a basic "Hello, World!" application.

## Development Commands

### Building and Running
```bash
go run main.go          # Run the application directly
go build               # Build the executable
go build -o motivation # Build with custom output name
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

## Project Structure

- `main.go` - Entry point with main function
- `go.mod` - Go module definition (Go 1.25.3)

## Architecture Notes

Currently a simple single-file Go application. The project structure suggests it may evolve into a random motivation quote/message generator based on the repository name.