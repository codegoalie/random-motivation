// Command uat runs a black-box User Acceptance Testing suite against the
// Random Motivation API. It interacts only via the public HTTP API and
// process-level controls; it must not import application packages, call
// handlers in-process, or inspect SQLite directly.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Exit codes for the uat command.
const (
	exitOK              = 0
	exitBehaviorFailure = 1
	exitUsage           = 2
)

// config captures the parsed CLI configuration for a UAT run.
type config struct {
	baseURL         string
	startCommand    string
	timeout         time.Duration
	verbose         bool
	skipDestructive bool
	renderURL       string
}

// parseConfig parses the given command-line arguments and returns the
// resulting config and the exit code that should be used if the program
// were to exit immediately. exitOK means parsing succeeded; exitUsage
// indicates a CLI error (including --help being requested).
func parseConfig(args []string, stdout, stderr io.Writer) (config, int) {
	fs := flag.NewFlagSet("uat", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var cfg config
	fs.StringVar(&cfg.baseURL, "base-url", "http://localhost:8080", "Base URL of the motivation service")
	fs.StringVar(&cfg.startCommand, "start-command", "", "Optional command used to start the service under test")
	fs.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "Overall timeout for the UAT run")
	fs.BoolVar(&cfg.verbose, "verbose", false, "Print every request and response assertion")
	fs.BoolVar(&cfg.skipDestructive, "skip-destructive", false, "Skip checks that assume an empty or isolated database")
	fs.StringVar(&cfg.renderURL, "render-url", "", "Optional explicit render service URL")

	if err := fs.Parse(args); err != nil {
		return config{}, exitUsage
	}
	return cfg, exitOK
}

func main() {
	cfg, code := parseConfig(os.Args[1:], os.Stdout, os.Stderr)
	if code != exitOK {
		os.Exit(code)
	}
	_ = cfg
	fmt.Fprintln(os.Stdout, "UAT skeleton ready")
	os.Exit(exitOK)
}
