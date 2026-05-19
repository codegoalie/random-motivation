// Command uat runs a black-box User Acceptance Testing suite against the
// Random Motivation API. It interacts only via the public HTTP API and
// process-level controls; it must not import application packages, call
// handlers in-process, or inspect SQLite directly.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
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
			snippets := []string{
				"Welcome to the Random Motivation API",
				"GET /motivation",
				"POST /motivation",
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
	for i := 0; i < attempts; i++ {
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

func main() {
	cfg, code := parseConfig(os.Args[1:], os.Stdout, os.Stderr)
	if code != exitOK {
		os.Exit(code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	// No checks are wired yet; later beads register the actual suite.
	checks := []Check{}
	if len(checks) == 0 {
		fmt.Fprintln(os.Stdout, "UAT skeleton ready")
		os.Exit(exitOK)
	}
	// Default extraEnv for self-managed mode: propagate --render-url
	// into the child as RENDER_SERVICE_URL so the spawned service
	// points at the same render endpoint the suite asserts against.
	var extraEnv []string
	if cfg.renderURL != "" {
		extraEnv = append(extraEnv, "RENDER_SERVICE_URL="+cfg.renderURL)
	}
	os.Exit(run(ctx, cfg, extraEnv, checks, os.Stdout, os.Stderr))
}
