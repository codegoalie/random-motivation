// Command uat runs a black-box User Acceptance Testing suite against the
// Random Motivation API. It interacts only via the public HTTP API and
// process-level controls; it must not import application packages, call
// handlers in-process, or inspect SQLite directly.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// waitReady polls GET / on baseURL until a 200 OK response is received
// or the context is cancelled. It uses a short, bounded poll interval
// and respects context cancellation between attempts.
func waitReady(ctx context.Context, client *http.Client, baseURL string) error {
	url := strings.TrimRight(baseURL, "/") + "/"
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build readiness request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return nil
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s readiness: %w", baseURL, ctx.Err())
		case <-ticker.C:
		}
	}
}

// supervisor manages a self-managed service subprocess for a UAT run.
// It owns the temporary working directory, the spawned process group,
// and ensures cleanup happens exactly once even if Stop is called
// multiple times.
type supervisor struct {
	cmd         *exec.Cmd
	tempDir     string
	dbPath      string
	waitDone    chan error
	logBuf      *bytes.Buffer
	stopOnce    sync.Once
	stopTimeout time.Duration
}

// defaultStopTimeout is how long the supervisor waits for the
// subprocess to exit cleanly after SIGTERM before escalating to
// SIGKILL.
const defaultStopTimeout = 3 * time.Second

// DBPath returns the absolute path to the isolated DB file the
// subprocess was told to use via DB_PATH.
func (s *supervisor) DBPath() string { return s.dbPath }

// TempDir returns the temporary directory created for this run.
func (s *supervisor) TempDir() string { return s.tempDir }

// Logs returns whatever the subprocess wrote to stdout/stderr so far.
// Useful for surfacing context on startup or readiness failures.
func (s *supervisor) Logs() string {
	if s.logBuf == nil {
		return ""
	}
	return s.logBuf.String()
}

// startSelfManaged spawns the service subprocess for self-managed mode,
// waits for HTTP readiness on cfg.baseURL, and returns a supervisor
// that the caller MUST Stop. cfg.startCommand is deliberately invoked
// through the system shell ("sh -c") because it is a user-provided
// command string that often relies on shell features (pipes, env
// substitution, e.g. `go run .`). extraEnv supplies additional
// child environment variables on top of the parent env and the
// auto-injected DB_PATH (e.g., RENDER_SERVICE_URL for check groups
// that need to point the service at a controlled render endpoint).
//
// On any failure (start error or readiness timeout), the subprocess
// is terminated and the temp directory is removed before returning.
func startSelfManaged(ctx context.Context, cfg config, extraEnv []string, client *http.Client, stdout, stderr io.Writer) (*supervisor, error) {
	if strings.TrimSpace(cfg.startCommand) == "" {
		return nil, fmt.Errorf("startSelfManaged: empty --start-command")
	}
	tempDir, err := os.MkdirTemp("", "uat-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	dbPath := filepath.Join(tempDir, "uat-motivations.db")

	// Deliberate shell invocation: --start-command is user-provided.
	cmd := exec.CommandContext(ctx, "sh", "-c", cfg.startCommand)
	env := append(os.Environ(), "DB_PATH="+dbPath)
	if len(extraEnv) > 0 {
		env = append(env, extraEnv...)
	}
	cmd.Env = env

	logBuf := &bytes.Buffer{}
	if cfg.verbose {
		cmd.Stdout = io.MultiWriter(stdout, logBuf)
		cmd.Stderr = io.MultiWriter(stderr, logBuf)
	} else {
		cmd.Stdout = logBuf
		cmd.Stderr = logBuf
	}
	// Run subprocess (and any children it spawns, e.g. `go run .`) in
	// their own process group so we can signal the whole tree on
	// shutdown rather than only the shell wrapper.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("start service subprocess (%q): %w", cfg.startCommand, err)
	}

	sup := &supervisor{
		cmd:         cmd,
		tempDir:     tempDir,
		dbPath:      dbPath,
		waitDone:    make(chan error, 1),
		logBuf:      logBuf,
		stopTimeout: defaultStopTimeout,
	}
	go func() { sup.waitDone <- cmd.Wait() }()

	if err := waitReady(ctx, client, cfg.baseURL); err != nil {
		sup.Stop()
		return nil, fmt.Errorf("service readiness on %s: %w\nsubprocess logs:\n%s",
			cfg.baseURL, err, logBuf.String())
	}
	return sup, nil
}

// Stop terminates the subprocess (SIGTERM, then SIGKILL after
// stopTimeout) and removes the temp directory. It is safe to call
// multiple times; only the first call has effect.
func (s *supervisor) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(s.stop)
}

func (s *supervisor) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		// Signal the whole process group on POSIX: passing a negative
		// PID to kill(2) targets every process in the group, so children
		// spawned by `sh -c` also receive the signal.
		target := -s.cmd.Process.Pid
		if pgid, err := syscall.Getpgid(s.cmd.Process.Pid); err == nil {
			target = -pgid
		}
		_ = syscall.Kill(target, syscall.SIGTERM)
		timeout := s.stopTimeout
		if timeout <= 0 {
			timeout = defaultStopTimeout
		}
		select {
		case <-s.waitDone:
		case <-time.After(timeout):
			_ = syscall.Kill(target, syscall.SIGKILL)
			<-s.waitDone
		}
	}
	if s.tempDir != "" {
		_ = os.RemoveAll(s.tempDir)
	}
}

// run wires up an Env from cfg, executes the supplied checks, and
// returns an appropriate exit code based on the run result.
//
// When cfg.startCommand is empty, run operates in existing-service
// mode: it polls the base URL for readiness over HTTP before running
// the suite, and does not create temp directories, set DB_PATH, or
// start subprocesses.
//
// When cfg.startCommand is non-empty, run operates in self-managed
// mode: it creates a temp dir, sets DB_PATH (plus any extraEnv such as
// RENDER_SERVICE_URL) for the child, launches the subprocess via
// `sh -c`, waits for readiness, runs the checks, and tears down the
// subprocess and temp dir on exit (success, failure, panic, or
// context timeout).
//
// extraEnv lets callers (typically check groups in main()) inject
// additional child env vars before startup. Checks needing different
// render behavior require separate self-managed runs/groups because a
// supervisor builds one env map before start.
func run(ctx context.Context, cfg config, extraEnv []string, checks []Check, stdout, stderr io.Writer) int {
	client := &http.Client{Timeout: cfg.timeout}
	if cfg.startCommand == "" {
		if err := waitReady(ctx, client, cfg.baseURL); err != nil {
			fmt.Fprintf(stderr, "readiness check failed: %v\n", err)
			return exitBehaviorFailure
		}
	} else {
		sup, err := startSelfManaged(ctx, cfg, extraEnv, client, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "self-managed startup failed: %v\n", err)
			return exitBehaviorFailure
		}
		// Cleanup runs on success, failure, context timeout, and panic.
		defer sup.Stop()
	}
	env := &Env{
		BaseURL:   cfg.baseURL,
		Client:    client,
		RunID:     newRunID(),
		Verbose:   cfg.verbose,
		RenderURL: cfg.renderURL,
		Out:       stdout,
		Err:       stderr,
	}
	res := runChecks(ctx, env, checks)
	if res.Failed > 0 {
		return exitBehaviorFailure
	}
	return exitOK
}

// checkLandingPage verifies that GET / returns 200 OK and that the
// response body documents the public API. Tagged non-destructive: it
// is safe to run against any reachable instance.
func checkLandingPage() Check {
	return Check{
		Name: "landing page describes API",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			const method, path = http.MethodGet, "/"
			resp, body, err := doRequest(ctx, env, method, path, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusOK); err != nil {
				return err
			}
			// "GET /motivation" is a substring of "GET /motivations", so
			// listing the singular endpoint before the plural one does
			// not weaken this assertion — both snippets are checked
			// independently and neither can satisfy the other's absence.
			snippets := []string{
				"Welcome to the Random Motivation API",
				"GET /motivation",
				"POST /motivation",
				"GET /motivations",
				"DELETE /motivation/:id",
				"GET /motivations.png",
			}
			for _, s := range snippets {
				if err := assertBodyContains(method, path, string(body), s); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// checkEmptyPOSTRejected verifies that POST /motivation with an empty
// body returns 400 with the documented error message. Tagged
// non-destructive because rejected submissions do not alter state.
func checkEmptyPOSTRejected() Check {
	return Check{
		Name: "empty motivation POST is rejected",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			const method, path = http.MethodPost, "/motivation"
			resp, body, err := doRequest(ctx, env, method, path, strings.NewReader(""))
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusBadRequest); err != nil {
				return err
			}
			return assertBodyContains(method, path, string(body), "Motivation cannot be empty")
		},
	}
}

// checkWhitespacePOSTRejected verifies that POST /motivation with a
// whitespace-only body is rejected after server-side trimming. Tagged
// non-destructive.
func checkWhitespacePOSTRejected() Check {
	return Check{
		Name: "whitespace motivation POST is rejected",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			const method, path = http.MethodPost, "/motivation"
			resp, body, err := doRequest(ctx, env, method, path, strings.NewReader("  \n\t  "))
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusBadRequest); err != nil {
				return err
			}
			return assertBodyContains(method, path, string(body), "Motivation cannot be empty")
		},
	}
}

// checkValidPOSTAccepted verifies that POST /motivation with non-empty
// text returns 201 and the documented success message. The submitted
// text embeds env.RunID to avoid collisions with concurrent runs.
// Tagged destructive because it mutates server state; selection logic
// in selection.go excludes it from existing-service mode by default.
func checkValidPOSTAccepted() Check {
	return Check{
		Name: "valid motivation POST is accepted",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			const method, path = http.MethodPost, "/motivation"
			payload := fmt.Sprintf("uat valid post %s", env.RunID)
			resp, body, err := doRequest(ctx, env, method, path, strings.NewReader(payload))
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusCreated); err != nil {
				return err
			}
			return assertBodyContains(method, path, string(body), "Motivation added successfully")
		},
	}
}

// retrievableExistingDefaultAttempts is the maximum number of GET
// /motivation attempts checkSubmittedMotivationRetrievableExisting
// makes while waiting for its submitted text to appear. The default
// matches the UAT specification's "bounded number of GET attempts".
const retrievableExistingDefaultAttempts = 20

// retrievableExistingDefaultSleep is the pause between consecutive
// GET attempts in checkSubmittedMotivationRetrievableExisting. It is
// short enough to keep the total wait bounded but long enough to
// avoid hammering a real service.
const retrievableExistingDefaultSleep = 100 * time.Millisecond

// checkSubmittedMotivationRetrievableIsolated submits a unique
// motivation via POST /motivation, then verifies GET /motivation
// returns 200 with a body equal to the submitted text. Tagged
// destructive because it assumes an empty/single-entry isolated
// database in which GET is deterministic; selection logic in
// selection.go excludes it from existing-service mode.
func checkSubmittedMotivationRetrievableIsolated() Check {
	return Check{
		Name: "submitted motivation is retrievable (isolated)",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			const postMethod, postPath = http.MethodPost, "/motivation"
			payload := "uat-retrievable-isolated-" + env.RunID
			postResp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
			if err != nil {
				return err
			}
			if err := assertStatus(postMethod, postPath, postResp.StatusCode, http.StatusCreated); err != nil {
				return err
			}

			const getMethod, getPath = http.MethodGet, "/motivation"
			getResp, getBody, err := doRequest(ctx, env, getMethod, getPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(getMethod, getPath, getResp.StatusCode, http.StatusOK); err != nil {
				return err
			}
			return assertBodyEquals(getMethod, getPath, string(getBody), payload)
		},
	}
}

// checkTrimmedSubmission submits a motivation surrounded by leading
// and trailing whitespace via POST /motivation, then verifies that
// GET /motivation returns 200 with a body equal to the trimmed core
// text (no surrounding whitespace). This proves the API strips
// whitespace before storing/serving. Tagged destructive because
// deterministic equality requires an empty/single-entry isolated
// database; selection logic in selection.go excludes it from
// existing-service mode.
func checkTrimmedSubmission() Check {
	return Check{
		Name: "submitted motivation is trimmed before storage",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			core := "Stay focused " + env.RunID + "."
			submission := "   " + core + "   "

			const postMethod, postPath = http.MethodPost, "/motivation"
			postResp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(submission))
			if err != nil {
				return err
			}
			if err := assertStatus(postMethod, postPath, postResp.StatusCode, http.StatusCreated); err != nil {
				return err
			}

			const getMethod, getPath = http.MethodGet, "/motivation"
			getResp, getBody, err := doRequest(ctx, env, getMethod, getPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(getMethod, getPath, getResp.StatusCode, http.StatusOK); err != nil {
				return err
			}
			return assertBodyEquals(getMethod, getPath, string(getBody), core)
		},
	}
}

// checkSubmittedMotivationRetrievableExisting submits a unique
// motivation via POST /motivation, then polls GET /motivation up to
// retrievableExistingDefaultAttempts times (sleeping
// retrievableExistingDefaultSleep between attempts) until the response
// body equals the submitted text. Tagged nonDestructive: the check
// does add a motivation to the remote database, but the UAT
// specification wants it eligible against existing services so
// operators opt into that single mutation by including the check.
func checkSubmittedMotivationRetrievableExisting() Check {
	return Check{
		Name: "submitted motivation is eventually retrievable (existing service)",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			return runRetrievableExisting(ctx, env,
				retrievableExistingDefaultAttempts,
				retrievableExistingDefaultSleep)
		},
	}
}

// runRetrievableExisting implements the body of
// checkSubmittedMotivationRetrievableExisting with a configurable
// attempt budget and sleep so tests can exercise the failure path
// without waiting for the production-tuned defaults.
func runRetrievableExisting(ctx context.Context, env *Env, attempts int, sleep time.Duration) error {
	const postMethod, postPath = http.MethodPost, "/motivation"
	payload := "uat-retrievable-existing-" + env.RunID
	postResp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
	if err != nil {
		return err
	}
	if err := assertStatus(postMethod, postPath, postResp.StatusCode, http.StatusCreated); err != nil {
		return err
	}

	const getMethod, getPath = http.MethodGet, "/motivation"
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sleep):
			}
		}
		getResp, getBody, err := doRequest(ctx, env, getMethod, getPath, nil)
		if err != nil {
			return err
		}
		if err := assertStatus(getMethod, getPath, getResp.StatusCode, http.StatusOK); err != nil {
			return err
		}
		if string(getBody) == payload {
			return nil
		}
	}
	return fmt.Errorf("%s %s: submitted motivation %q not observed after %d attempts",
		getMethod, getPath, payload, attempts)
}

// multipleRetrievableSubmissions is the number of unique motivations
// checkMultipleMotivationsRetrievable submits in isolated mode.
const multipleRetrievableSubmissions = 3

// multipleRetrievableMaxAttempts caps the number of GET /motivation
// calls checkMultipleMotivationsRetrievable issues while trying to
// observe each submitted text. Per UAT.md ("Multiple submitted
// motivations are retrievable"), this is twice the submitted count:
// the service shuffles a queue of size N and only reshuffles after a
// full cycle, so 2N attempts guarantee every entry appears at least
// once regardless of where the cycle boundary fell.
const multipleRetrievableMaxAttempts = 2 * multipleRetrievableSubmissions

// checkMultipleMotivationsRetrievable submits three unique motivations
// via POST /motivation, then calls GET /motivation up to six times and
// asserts that every response is 200 OK with a body matching one of
// the submitted texts and that all three submitted texts are observed
// at least once. Tagged destructive because deterministic coverage of
// the full set requires an isolated database; selection logic in
// selection.go excludes it from existing-service mode.
func checkMultipleMotivationsRetrievable() Check {
	return Check{
		Name: "multiple submitted motivations are retrievable (isolated)",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			return runMultipleMotivationsRetrievable(ctx, env,
				multipleRetrievableSubmissions,
				multipleRetrievableMaxAttempts)
		},
	}
}

// runMultipleMotivationsRetrievable implements the body of
// checkMultipleMotivationsRetrievable with a configurable submission
// count and GET attempt budget so tests can exercise the failure paths
// without paying for the production-tuned defaults.
func runMultipleMotivationsRetrievable(ctx context.Context, env *Env, count, maxAttempts int) error {
	const postMethod, postPath = http.MethodPost, "/motivation"
	submitted := make([]string, 0, count)
	remaining := make(map[string]bool, count)
	for i := 1; i <= count; i++ {
		payload := fmt.Sprintf("uat multi %s #%d", env.RunID, i)
		submitted = append(submitted, payload)
		remaining[payload] = true
		resp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
		if err != nil {
			return err
		}
		if err := assertStatus(postMethod, postPath, resp.StatusCode, http.StatusCreated); err != nil {
			return err
		}
	}

	const getMethod, getPath = http.MethodGet, "/motivation"
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, body, err := doRequest(ctx, env, getMethod, getPath, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s %s: attempt %d status got=%d want=%d",
				getMethod, getPath, attempt, resp.StatusCode, http.StatusOK)
		}
		got := string(body)
		known := false
		for _, s := range submitted {
			if s == got {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("%s %s: attempt %d returned unexpected body %q; expected one of %q",
				getMethod, getPath, attempt, got, submitted)
		}
		delete(remaining, got)
		if len(remaining) == 0 {
			return nil
		}
	}

	missing := make([]string, 0, len(remaining))
	for _, s := range submitted {
		if remaining[s] {
			missing = append(missing, s)
		}
	}
	return fmt.Errorf("%s %s: submitted motivations not observed after %d attempts: %q",
		getMethod, getPath, maxAttempts, missing)
}

// repeatedGETAvailabilitySubmissions is the number of unique motivations
// checkRepeatedGETAvailability submits before exercising repeated reads.
const repeatedGETAvailabilitySubmissions = 3

// repeatedGETAvailabilityAttempts is the number of GET /motivation calls
// checkRepeatedGETAvailability issues after submission. Per UAT.md
// ("Motivation retrieval remains available after repeated reads"), this
// must exceed the number of submitted motivations; 7 is one more than
// twice the submitted count, ensuring the service services more reads
// than there are entries.
const repeatedGETAvailabilityAttempts = 7

// checkRepeatedGETAvailability submits three unique motivations via
// POST /motivation, then issues seven GET /motivation calls and asserts
// every response is 200 OK with a body matching one of the submitted
// texts. Unlike checkMultipleMotivationsRetrievable, it does not
// require every submission to be observed; it only verifies that
// retrieval remains available across more reads than there are
// entries (no 404, no unexpected bodies). Tagged destructive: known-
// value-only assertion requires an isolated database; selection logic
// in selection.go excludes it from existing-service mode.
func checkRepeatedGETAvailability() Check {
	return Check{
		Name: "repeated GET /motivation remains available (isolated)",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			return runRepeatedGETAvailability(ctx, env,
				repeatedGETAvailabilitySubmissions,
				repeatedGETAvailabilityAttempts)
		},
	}
}

// runRepeatedGETAvailability implements the body of
// checkRepeatedGETAvailability with a configurable submission count
// and GET attempt budget so tests can exercise failure paths cheaply.
func runRepeatedGETAvailability(ctx context.Context, env *Env, count, attempts int) error {
	const postMethod, postPath = http.MethodPost, "/motivation"
	submitted := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		payload := fmt.Sprintf("uat avail %s #%d", env.RunID, i)
		submitted = append(submitted, payload)
		resp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("%s %s: submission #%d status got=%d want=%d",
				postMethod, postPath, i, resp.StatusCode, http.StatusCreated)
		}
	}

	const getMethod, getPath = http.MethodGet, "/motivation"
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, body, err := doRequest(ctx, env, getMethod, getPath, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s %s: attempt %d status got=%d want=%d",
				getMethod, getPath, attempt, resp.StatusCode, http.StatusOK)
		}
		got := string(body)
		known := false
		for _, s := range submitted {
			if s == got {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("%s %s: attempt %d returned unexpected body %q; expected one of %q",
				getMethod, getPath, attempt, got, submitted)
		}
	}
	return nil
}

// checkEmptyMotivationCollection verifies that GET /motivation on a
// service with no stored motivations returns 404 Not Found with the
// documented "No motivations found" body. Tagged destructive because it
// presumes an empty database: selection logic in selection.go excludes
// it from existing-service mode so it only runs in isolated
// self-managed mode.
func checkEmptyMotivationCollection() Check {
	return Check{
		Name: "empty motivation collection returns 404",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			const method, path = http.MethodGet, "/motivation"
			resp, body, err := doRequest(ctx, env, method, path, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusNotFound); err != nil {
				return err
			}
			return assertBodyContains(method, path, string(body), "No motivations found")
		},
	}
}

// checkPNGNoMotivations verifies that GET /motivations.png on a service
// with no stored motivations returns 404 Not Found with the documented
// "No motivations found" body. Tagged destructive because it presumes
// an empty database: selection logic in selection.go excludes it from
// existing-service mode so it only runs in isolated self-managed mode.
func checkPNGNoMotivations() Check {
	return Check{
		Name: "empty motivation collection PNG returns 404",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			const method, path = http.MethodGet, "/motivations.png"
			resp, body, err := doRequest(ctx, env, method, path, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusNotFound); err != nil {
				return err
			}
			return assertBodyContains(method, path, string(body), "No motivations found")
		},
	}
}

// checkPNGRenderSuccess submits a unique motivation via POST /motivation,
// then verifies GET /motivations.png returns 200 OK with Content-Type
// image/png and bytes equal to the fake render service's PNG fixture.
// Tagged destructive | renderRequired: it mutates server state and
// assumes a controlled render endpoint (the fake render service in
// self-managed mode, or an explicit --render-url in existing mode).
func checkPNGRenderSuccess() Check {
	return Check{
		Name: "PNG render success",
		Kind: destructive | renderRequired,
		Run: func(ctx context.Context, env *Env) error {
			const postMethod, postPath = http.MethodPost, "/motivation"
			payload := "uat-render-success-" + env.RunID
			postResp, postBody, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
			if err != nil {
				return err
			}
			if err := assertStatus(postMethod, postPath, postResp.StatusCode, http.StatusCreated); err != nil {
				return err
			}
			if err := assertBodyContains(postMethod, postPath, string(postBody), "Motivation added successfully"); err != nil {
				return err
			}

			const getMethod, getPath = http.MethodGet, "/motivations.png"
			getResp, getBody, err := doRequest(ctx, env, getMethod, getPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(getMethod, getPath, getResp.StatusCode, http.StatusOK); err != nil {
				return err
			}
			if err := assertContentTypePrefix(getMethod, getPath, getResp.Header, "image/png"); err != nil {
				return err
			}
			if !bytes.Equal(getBody, png1x1) {
				return fmt.Errorf("%s %s: body bytes do not match PNG fixture (got %d bytes, want %d)",
					getMethod, getPath, len(getBody), len(png1x1))
			}
			return nil
		},
	}
}

// checkRenderServiceUnreachable submits a unique motivation via
// POST /motivation, then verifies GET /motivations.png returns 500
// with the documented "Error rendering motivation image" body when the
// service's configured render endpoint is unreachable. Tagged
// destructive | renderRequired: it mutates server state and assumes
// the runner/supervisor has pointed the service at an unreachable
// render URL (T22's responsibility). This check itself does not
// configure the render URL.
func checkRenderServiceUnreachable() Check {
	return Check{
		Name: "PNG render fails when render service is unreachable",
		Kind: destructive | renderRequired,
		Run: func(ctx context.Context, env *Env) error {
			const postMethod, postPath = http.MethodPost, "/motivation"
			payload := "uat-render-unreachable-" + env.RunID
			postResp, postBody, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
			if err != nil {
				return err
			}
			if err := assertStatus(postMethod, postPath, postResp.StatusCode, http.StatusCreated); err != nil {
				return err
			}
			if err := assertBodyContains(postMethod, postPath, string(postBody), "Motivation added successfully"); err != nil {
				return err
			}

			const getMethod, getPath = http.MethodGet, "/motivations.png"
			getResp, getBody, err := doRequest(ctx, env, getMethod, getPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(getMethod, getPath, getResp.StatusCode, http.StatusInternalServerError); err != nil {
				return err
			}
			return assertBodyContains(getMethod, getPath, string(getBody), "Error rendering motivation image")
		},
	}
}

// checkRenderServiceNonOK submits a unique motivation via
// POST /motivation, then verifies GET /motivations.png returns 500
// with the documented "Error rendering motivation image" body when the
// service's configured render endpoint returns a non-200 response.
// Tagged destructive | renderRequired: it mutates server state and
// assumes the runner/supervisor has pointed the service at a render
// endpoint that returns a non-OK status (T22's responsibility). From
// the API/client perspective this is indistinguishable from the
// unreachable case (T20); the difference lies in how the render
// service is supervised.
func checkRenderServiceNonOK() Check {
	return Check{
		Name: "PNG render fails when render service returns non-OK",
		Kind: destructive | renderRequired,
		Run: func(ctx context.Context, env *Env) error {
			const postMethod, postPath = http.MethodPost, "/motivation"
			payload := "uat-render-nonok-" + env.RunID
			postResp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
			if err != nil {
				return err
			}
			if err := assertStatus(postMethod, postPath, postResp.StatusCode, http.StatusCreated); err != nil {
				return err
			}

			const getMethod, getPath = http.MethodGet, "/motivations.png"
			getResp, getBody, err := doRequest(ctx, env, getMethod, getPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(getMethod, getPath, getResp.StatusCode, http.StatusInternalServerError); err != nil {
				return err
			}
			return assertBodyContains(getMethod, getPath, string(getBody), "Error rendering motivation image")
		},
	}
}

// checkUnsupportedMethods verifies that the service rejects HTTP
// methods that are not part of the documented API with 405 Method Not
// Allowed. It exercises PUT /motivation, DELETE /motivation, and
// POST /motivations.png. Tagged non-destructive: rejected requests do
// not mutate state. The response body is not asserted because
// framework-generated 405 bodies vary.
func checkUnsupportedMethods() Check {
	return Check{
		Name: "unsupported methods are rejected with 405",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			cases := []struct {
				method, path string
			}{
				{http.MethodPut, "/motivation"},
				{http.MethodDelete, "/motivation"},
				{http.MethodPost, "/motivations.png"},
			}
			for _, c := range cases {
				resp, _, err := doRequest(ctx, env, c.method, c.path, nil)
				if err != nil {
					return err
				}
				if err := assertStatus(c.method, c.path, resp.StatusCode, http.StatusMethodNotAllowed); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// checkUnknownRoute verifies that requesting an undefined path returns
// 404 Not Found. The path embeds env.RunID to avoid collisions with
// any future endpoints. Tagged non-destructive. Only the status code is
// asserted because the response body is framework-specific.
func checkUnknownRoute() Check {
	return Check{
		Name: "unknown route returns 404",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			const method = http.MethodGet
			path := "/uat-route-that-should-not-exist-" + env.RunID
			resp, _, err := doRequest(ctx, env, method, path, nil)
			if err != nil {
				return err
			}
			return assertStatus(method, path, resp.StatusCode, http.StatusNotFound)
		},
	}
}

// motivationListItem mirrors the JSON shape of one entry returned by
// GET /motivations. It is a local decoding target rather than a
// db.Motivation import: this suite is strictly black-box and must not
// depend on application packages.
type motivationListItem struct {
	ID        int64  `json:"id"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

// decodeMotivationList parses a GET /motivations response body into a
// slice of motivationListItem. Note that "null" is valid JSON and
// decodes successfully to a nil/empty slice without error; callers
// that must distinguish "null" from "[]" (e.g. checkMotivationsListEmpty)
// need to check the raw body themselves before decoding.
func decodeMotivationList(body []byte) ([]motivationListItem, error) {
	var items []motivationListItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("decode motivations list: %w (body=%q)", err, string(body))
	}
	return items, nil
}

// checkMotivationsListEmpty verifies that GET /motivations on a
// service with no stored motivations returns 200 with a body that
// parses as an empty JSON array -- specifically not the literal
// "null", which encoding/json would also accept for a nil slice.
// Tagged destructive because it presumes an empty database: selection
// logic in selection.go excludes it from existing-service mode so it
// only runs in isolated self-managed mode, before any POST.
func checkMotivationsListEmpty() Check {
	return Check{
		Name: "empty motivations list returns empty JSON array",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			const method, path = http.MethodGet, "/motivations"
			resp, body, err := doRequest(ctx, env, method, path, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusOK); err != nil {
				return err
			}
			if trimmed := strings.TrimSpace(string(body)); trimmed == "null" {
				return fmt.Errorf("%s %s: body is %q, want an empty JSON array (not null)", method, path, trimmed)
			}
			items, err := decodeMotivationList(body)
			if err != nil {
				return err
			}
			if len(items) != 0 {
				return fmt.Errorf("%s %s: expected empty array, got %d entries (body=%q)",
					method, path, len(items), string(body))
			}
			return nil
		},
	}
}

// checkMotivationsListAfterPost submits a unique motivation via
// POST /motivation, then verifies GET /motivations returns 200 with a
// body that decodes to exactly one entry whose text matches what was
// submitted, whose id is a positive integer, and whose created_at is
// non-empty. Tagged destructive because deterministic single-entry
// assertions require an isolated database; selection logic in
// selection.go excludes it from existing-service mode.
func checkMotivationsListAfterPost() Check {
	return Check{
		Name: "motivations list reflects a single POST (isolated)",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			const postMethod, postPath = http.MethodPost, "/motivation"
			payload := "uat-list-after-post-" + env.RunID
			postResp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
			if err != nil {
				return err
			}
			if err := assertStatus(postMethod, postPath, postResp.StatusCode, http.StatusCreated); err != nil {
				return err
			}

			const getMethod, getPath = http.MethodGet, "/motivations"
			getResp, getBody, err := doRequest(ctx, env, getMethod, getPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(getMethod, getPath, getResp.StatusCode, http.StatusOK); err != nil {
				return err
			}
			items, err := decodeMotivationList(getBody)
			if err != nil {
				return err
			}
			if len(items) != 1 {
				return fmt.Errorf("%s %s: expected exactly 1 entry, got %d (body=%q)",
					getMethod, getPath, len(items), string(getBody))
			}
			item := items[0]
			if item.Text != payload {
				return fmt.Errorf("%s %s: entry text = %q, want %q", getMethod, getPath, item.Text, payload)
			}
			if item.ID <= 0 {
				return fmt.Errorf("%s %s: entry id = %d, want positive integer", getMethod, getPath, item.ID)
			}
			if strings.TrimSpace(item.CreatedAt) == "" {
				return fmt.Errorf("%s %s: entry created_at is empty", getMethod, getPath)
			}
			return nil
		},
	}
}

// checkDeleteUnknownID verifies that DELETE /motivation/:id for an id
// that cannot possibly exist (the maximum int64 value) returns 404
// with the documented "Motivation not found" body. Tagged
// non-destructive: it is safe against any deployment because no row
// can plausibly have this id (auto-increment primary keys never reach
// it), so nothing is mutated and no real data can be deleted.
func checkDeleteUnknownID() Check {
	return Check{
		Name: "DELETE unknown motivation id returns 404",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			const method = http.MethodDelete
			const path = "/motivation/9223372036854775807"
			resp, body, err := doRequest(ctx, env, method, path, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(method, path, resp.StatusCode, http.StatusNotFound); err != nil {
				return err
			}
			return assertBodyContains(method, path, string(body), "Motivation not found")
		},
	}
}

// checkDeleteInvalidID verifies that DELETE /motivation/:id returns
// 400 with the documented "Invalid motivation id" body for several
// unparseable id forms: a non-numeric string, a float, and a string
// with a run-unique suffix (to rule out any accidental match against
// real data). Tagged non-destructive: every variant fails to parse as
// an integer id before the server can look anything up, so nothing is
// mutated and the check is safe against any deployment.
func checkDeleteInvalidID() Check {
	return Check{
		Name: "DELETE unparseable motivation id returns 400",
		Kind: nonDestructive,
		Run: func(ctx context.Context, env *Env) error {
			const method = http.MethodDelete
			ids := []string{
				"not-a-number",
				"1.5",
				"id-" + env.RunID,
			}
			for _, id := range ids {
				path := "/motivation/" + id
				resp, body, err := doRequest(ctx, env, method, path, nil)
				if err != nil {
					return err
				}
				if err := assertStatus(method, path, resp.StatusCode, http.StatusBadRequest); err != nil {
					return err
				}
				if err := assertBodyContains(method, path, string(body), "Invalid motivation id"); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// checkDeleteRemovesFromList submits two unique motivations, uses
// GET /motivations to learn their ids, deletes the first, and asserts
// the DELETE returns 204 with an empty body while GET /motivations
// afterward contains only the second (with the deleted id absent).
// Tagged destructive because deterministic before/after counts require
// an isolated database; selection logic in selection.go excludes it
// from existing-service mode.
func checkDeleteRemovesFromList() Check {
	return Check{
		Name: "DELETE removes a motivation from the list (isolated)",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			const postMethod, postPath = http.MethodPost, "/motivation"
			first := "uat-delete-list-first-" + env.RunID
			second := "uat-delete-list-second-" + env.RunID
			for _, payload := range []string{first, second} {
				resp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
				if err != nil {
					return err
				}
				if err := assertStatus(postMethod, postPath, resp.StatusCode, http.StatusCreated); err != nil {
					return err
				}
			}

			const listMethod, listPath = http.MethodGet, "/motivations"
			listResp, listBody, err := doRequest(ctx, env, listMethod, listPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(listMethod, listPath, listResp.StatusCode, http.StatusOK); err != nil {
				return err
			}
			items, err := decodeMotivationList(listBody)
			if err != nil {
				return err
			}
			if len(items) != 2 {
				return fmt.Errorf("%s %s: expected 2 entries before delete, got %d (body=%q)",
					listMethod, listPath, len(items), string(listBody))
			}
			var firstID, secondID int64
			for _, item := range items {
				switch item.Text {
				case first:
					firstID = item.ID
				case second:
					secondID = item.ID
				}
			}
			if firstID == 0 || secondID == 0 {
				return fmt.Errorf("%s %s: could not locate both submitted entries in %q",
					listMethod, listPath, string(listBody))
			}

			const deleteMethod = http.MethodDelete
			deletePath := fmt.Sprintf("/motivation/%d", firstID)
			delResp, delBody, err := doRequest(ctx, env, deleteMethod, deletePath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(deleteMethod, deletePath, delResp.StatusCode, http.StatusNoContent); err != nil {
				return err
			}
			if len(delBody) != 0 {
				return fmt.Errorf("%s %s: expected empty body, got %q", deleteMethod, deletePath, string(delBody))
			}

			listResp2, listBody2, err := doRequest(ctx, env, listMethod, listPath, nil)
			if err != nil {
				return err
			}
			if err := assertStatus(listMethod, listPath, listResp2.StatusCode, http.StatusOK); err != nil {
				return err
			}
			items2, err := decodeMotivationList(listBody2)
			if err != nil {
				return err
			}
			if len(items2) != 1 {
				return fmt.Errorf("%s %s: expected 1 entry after delete, got %d (body=%q)",
					listMethod, listPath, len(items2), string(listBody2))
			}
			if items2[0].ID == firstID {
				return fmt.Errorf("%s %s: deleted id %d still present after delete", listMethod, listPath, firstID)
			}
			if items2[0].Text != second {
				return fmt.Errorf("%s %s: remaining entry text = %q, want %q",
					listMethod, listPath, items2[0].Text, second)
			}
			return nil
		},
	}
}

// deletedMotivationRegressionAttempts is the number of GET /motivation
// calls checkDeletedMotivationNotServed issues after deleting one of
// two submitted motivations. It comfortably exceeds the post-delete
// queue size (1) and the pre-delete queue size (2), so were the
// eviction regression to reappear (the deleted motivation staying in
// the in-memory rotation instead of being removed on DELETE), the
// deleted text would almost certainly resurface within this budget.
const deletedMotivationRegressionAttempts = 8

// checkDeletedMotivationNotServed submits exactly two motivations,
// deletes one via DELETE /motivation/:id, then calls GET /motivation
// deletedMotivationRegressionAttempts times and asserts the deleted
// text is never returned while the surviving text is observed at
// least once. This is the regression test for the queue-eviction
// behavior documented alongside DELETE /motivation/:id: deletion must
// also evict the entry from the in-memory rotation, not just the
// database row. Tagged destructive and given its own self-managed
// group (see buildSelfManagedGroups) because it needs solo state: any
// other motivation sharing the database would make the GET
// /motivation body assertions non-deterministic.
func checkDeletedMotivationNotServed() Check {
	return Check{
		Name: "deleted motivation stops being served by GET /motivation (isolated)",
		Kind: destructive,
		Run: func(ctx context.Context, env *Env) error {
			return runDeletedMotivationNotServed(ctx, env, deletedMotivationRegressionAttempts)
		},
	}
}

// runDeletedMotivationNotServed implements the body of
// checkDeletedMotivationNotServed with a configurable GET attempt
// budget so tests can exercise the failure paths cheaply.
func runDeletedMotivationNotServed(ctx context.Context, env *Env, attempts int) error {
	const postMethod, postPath = http.MethodPost, "/motivation"
	toDelete := "uat-delete-served-victim-" + env.RunID
	survivor := "uat-delete-served-survivor-" + env.RunID
	for _, payload := range []string{toDelete, survivor} {
		resp, _, err := doRequest(ctx, env, postMethod, postPath, strings.NewReader(payload))
		if err != nil {
			return err
		}
		if err := assertStatus(postMethod, postPath, resp.StatusCode, http.StatusCreated); err != nil {
			return err
		}
	}

	const listMethod, listPath = http.MethodGet, "/motivations"
	listResp, listBody, err := doRequest(ctx, env, listMethod, listPath, nil)
	if err != nil {
		return err
	}
	if err := assertStatus(listMethod, listPath, listResp.StatusCode, http.StatusOK); err != nil {
		return err
	}
	items, err := decodeMotivationList(listBody)
	if err != nil {
		return err
	}
	var victimID int64
	for _, item := range items {
		if item.Text == toDelete {
			victimID = item.ID
			break
		}
	}
	if victimID == 0 {
		return fmt.Errorf("%s %s: could not locate victim entry %q in %q",
			listMethod, listPath, toDelete, string(listBody))
	}

	const deleteMethod = http.MethodDelete
	deletePath := fmt.Sprintf("/motivation/%d", victimID)
	delResp, _, err := doRequest(ctx, env, deleteMethod, deletePath, nil)
	if err != nil {
		return err
	}
	if err := assertStatus(deleteMethod, deletePath, delResp.StatusCode, http.StatusNoContent); err != nil {
		return err
	}

	const getMethod, getPath = http.MethodGet, "/motivation"
	survivorSeen := false
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, body, err := doRequest(ctx, env, getMethod, getPath, nil)
		if err != nil {
			return err
		}
		if err := assertStatus(getMethod, getPath, resp.StatusCode, http.StatusOK); err != nil {
			return err
		}
		got := string(body)
		switch got {
		case toDelete:
			return fmt.Errorf("%s %s: attempt %d returned deleted motivation %q; eviction from rotation regressed",
				getMethod, getPath, attempt, got)
		case survivor:
			survivorSeen = true
		default:
			return fmt.Errorf("%s %s: attempt %d returned unexpected body %q; expected %q",
				getMethod, getPath, attempt, got, survivor)
		}
	}
	if !survivorSeen {
		return fmt.Errorf("%s %s: surviving motivation %q not observed after %d attempts",
			getMethod, getPath, survivor, attempts)
	}
	return nil
}

// buildExistingServiceSuite returns the ordered list of checks for
// existing-service mode. Selection (selectChecks) automatically drops
// destructive entries; render-required checks would also need a
// --render-url. The list intentionally contains only nonDestructive
// checks today: render-required checks (T18, T20, T21) are all also
// destructive and therefore unreachable in existing-service mode even
// when --render-url is supplied.
func buildExistingServiceSuite() []Check {
	return []Check{
		checkLandingPage(),                            // T7
		checkEmptyPOSTRejected(),                      // T8
		checkWhitespacePOSTRejected(),                 // T9
		checkUnsupportedMethods(),                     // T11
		checkUnknownRoute(),                           // T12
		checkDeleteUnknownID(),                        // T24
		checkDeleteInvalidID(),                        // T25
		checkSubmittedMotivationRetrievableExisting(), // T14-existing
	}
}

// renderSetup provisions a render endpoint for one self-managed group.
// It returns the RENDER_SERVICE_URL to inject into the child env and
// a cleanup function the caller MUST invoke when the group is done.
// The cleanup function is never nil so callers can defer it
// unconditionally.
type renderSetup func() (renderURL string, cleanup func(), err error)

// selfManagedGroup is a single self-managed run: a fresh DB +
// tailored render endpoint + an ordered slice of checks. Groups exist
// because some checks (T15, T14-isolated) require a single-entry
// queue and render-failure checks (T20, T21) require different
// RENDER_SERVICE_URL values; both can only be guaranteed with a
// fresh supervisor spawn.
type selfManagedGroup struct {
	name   string
	checks []Check
	setup  renderSetup
}

// fakeRenderSuccessSetup provisions a fake render server returning a
// 1x1 PNG. RENDER_SERVICE_URL must include the /render path because
// the application appends ?text=... directly.
func fakeRenderSuccessSetup() (string, func(), error) {
	fr := newFakeRender()
	return fr.URL() + "/render", fr.Close, nil
}

// unreachableRenderSetup picks an address on 127.0.0.1 that is not
// listening at the moment the supervisor starts. There is a small race
// window between picking the port and the child starting, but it is
// acceptable for UAT.
func unreachableRenderSetup() (string, func(), error) {
	url, err := pickUnreachableAddr()
	if err != nil {
		return "", func() {}, err
	}
	return url, func() {}, nil
}

// failingRenderSetup returns a renderSetup that stands up a stub
// render server which responds with the given status to every
// request.
func failingRenderSetup(status int) renderSetup {
	return func() (string, func(), error) {
		srv, url := newFailingRender(status)
		return url, srv.Close, nil
	}
}

// pickUnreachableAddr opens a listener on 127.0.0.1:0, captures the
// URL, and closes the listener before returning. The returned URL
// includes the /render path so it matches the application's
// expectation that RENDER_SERVICE_URL already contains the full
// render endpoint.
func pickUnreachableAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("pickUnreachableAddr listen: %w", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", fmt.Errorf("pickUnreachableAddr close: %w", err)
	}
	return "http://" + addr + "/render", nil
}

// newFailingRender starts a stub render server that responds with the
// given status code (and an empty body) to every request. It returns
// the server (so the caller can Close it) and the RENDER_SERVICE_URL
// the application should be pointed at, including the /render path.
func newFailingRender(status int) (*httptest.Server, string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	return srv, srv.URL + "/render"
}

// buildSelfManagedGroups assembles the sequential groups that make up
// the self-managed suite. Each group runs in its own freshly-spawned
// service subprocess with an isolated database, so destructive checks
// that assert on specific queue contents get the state they expect.
//
// Ordering rationale:
//
//   - Group A: empty-DB, PNG-empty-DB, and empty-list assertions before
//     any state-mutating POST; T15 is the sole mutating POST and must
//     therefore be last so its GET equality check sees a single entry.
//   - Group B: T14-isolated needs a single-entry DB after its own POST.
//   - Group C: T23 needs a single-entry DB after its own POST, just
//     like T14-isolated, so it gets its own group rather than sharing
//     group B (two POSTs in one DB would break both checks' "exactly
//     one entry" assertions).
//   - Group D: T10 (validate POST) and T18 (PNG render success) both
//     work in a shared DB because neither asserts that the queue
//     contains only their submissions. T10 runs first to keep
//     diagnostic output predictable; T18's POST then PNG fetch is
//     unaffected by T10's entry.
//   - Group E: T16 requires a fresh DB because it asserts every GET
//     returns one of its three submissions.
//   - Group F: T17 has the same isolation requirement as T16.
//   - Group G: T26 requires a fresh DB because it asserts an exact
//     count of entries before and after the delete.
//   - Group H: T27 requires a fresh DB (solo state) because it asserts
//     every GET /motivation response is one of exactly two submissions
//     minus the deleted one; any other motivation sharing the database
//     would make that assertion non-deterministic.
//   - Group I: render service unreachable.
//   - Group J: render service returns 500.
func buildSelfManagedGroups() []selfManagedGroup {
	return []selfManagedGroup{
		{
			name: "A-fake-render-empty-and-trimmed",
			checks: []Check{
				checkLandingPage(),               // T7
				checkEmptyPOSTRejected(),         // T8
				checkWhitespacePOSTRejected(),    // T9
				checkUnsupportedMethods(),        // T11
				checkUnknownRoute(),              // T12
				checkEmptyMotivationCollection(), // T13 (pre-POST)
				checkPNGNoMotivations(),          // T19 (pre-POST)
				checkMotivationsListEmpty(),      // T22 (pre-POST)
				checkTrimmedSubmission(),         // T15 (first POST)
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name: "B-fake-render-isolated-retrieval",
			checks: []Check{
				checkSubmittedMotivationRetrievableIsolated(), // T14-isolated
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name: "C-fake-render-list-after-post",
			checks: []Check{
				checkMotivationsListAfterPost(), // T23 (needs solo state)
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name: "D-fake-render-valid-post-and-png",
			checks: []Check{
				checkValidPOSTAccepted(), // T10
				checkPNGRenderSuccess(),  // T18
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name: "E-fake-render-multiple-retrievable",
			checks: []Check{
				checkMultipleMotivationsRetrievable(), // T16 (needs solo state)
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name: "F-fake-render-repeated-availability",
			checks: []Check{
				checkRepeatedGETAvailability(), // T17 (needs solo state)
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name: "G-fake-render-delete-removes-from-list",
			checks: []Check{
				checkDeleteRemovesFromList(), // T26 (needs solo state)
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name: "H-fake-render-deleted-not-served",
			checks: []Check{
				checkDeletedMotivationNotServed(), // T27 (needs solo state)
			},
			setup: fakeRenderSuccessSetup,
		},
		{
			name:   "I-render-unreachable",
			checks: []Check{checkRenderServiceUnreachable()}, // T20
			setup:  unreachableRenderSetup,
		},
		{
			name:   "J-render-non-ok",
			checks: []Check{checkRenderServiceNonOK()}, // T21
			setup:  failingRenderSetup(http.StatusInternalServerError),
		},
	}
}

// runFunc matches the signature of run() so runGroups can be unit-
// tested without spawning real subprocesses.
type runFunc func(ctx context.Context, cfg config, extraEnv []string, checks []Check, stdout, stderr io.Writer) int

// runGroups executes each group in order, sharing the overall ctx
// (so --timeout covers the full suite). It returns exitOK only when
// every group's runOne returns exitOK. On any failure it returns
// exitBehaviorFailure but still attempts every remaining group so
// operators get a complete picture in one run. If ctx is cancelled
// or has expired between groups it stops early.
func runGroups(ctx context.Context, cfg config, groups []selfManagedGroup, stdout, stderr io.Writer, runOne runFunc) int {
	final := exitOK
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(stderr, "context cancelled before group %s: %v\n", g.name, err)
			return exitBehaviorFailure
		}
		fmt.Fprintf(stdout, "===== group %s =====\n", g.name)
		renderURL, cleanup, err := g.setup()
		if err != nil {
			fmt.Fprintf(stderr, "group %s render setup failed: %v\n", g.name, err)
			if cleanup != nil {
				cleanup()
			}
			final = exitBehaviorFailure
			continue
		}
		extraEnv := []string{"RENDER_SERVICE_URL=" + renderURL}
		// Apply selectChecks so --skip-destructive still drops
		// checks even in self-managed mode.
		selected := selectChecks(modeSelfManaged, cfg, g.checks)
		code := runOne(ctx, cfg, extraEnv, selected, stdout, stderr)
		cleanup()
		if code != exitOK {
			final = exitBehaviorFailure
		}
	}
	return final
}

// runSelfManagedSuite builds the self-managed groups and runs them
// against the real run() function.
func runSelfManagedSuite(ctx context.Context, cfg config, stdout, stderr io.Writer) int {
	return runGroups(ctx, cfg, buildSelfManagedGroups(), stdout, stderr, run)
}

func main() {
	cfg, code := parseConfig(os.Args[1:], os.Stdout, os.Stderr)
	if code != exitOK {
		os.Exit(code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	if selectMode(cfg) == modeExisting {
		checks := selectChecks(modeExisting, cfg, buildExistingServiceSuite())
		// Propagate --render-url into extraEnv for symmetry with
		// self-managed mode. In existing-service mode run() does not
		// spawn a subprocess, so extraEnv is currently unused; pass
		// it anyway in case future checks need it.
		var extraEnv []string
		if cfg.renderURL != "" {
			extraEnv = append(extraEnv, "RENDER_SERVICE_URL="+cfg.renderURL)
		}
		os.Exit(run(ctx, cfg, extraEnv, checks, os.Stdout, os.Stderr))
	}

	os.Exit(runSelfManagedSuite(ctx, cfg, os.Stdout, os.Stderr))
}
