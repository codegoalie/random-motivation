package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestMain dispatches between the normal test runner and a helper mode
// that lets supervisor tests launch real subprocesses without
// depending on any external binary. When UAT_TEST_HELPER is non-empty,
// the test binary acts as a tiny stand-in service that:
//   - listens on UAT_TEST_ADDR (or skips listening for some modes),
//   - writes its DB_PATH and RENDER_SERVICE_URL env to UAT_TEST_REPORT
//     (if set),
//   - either responds 200 on / (mode=server), responds 503
//     (mode=unready), hangs forever ignoring SIGTERM (mode=stubborn),
//     or never serves HTTP and blocks (mode=nostart).
func TestMain(m *testing.M) {
	if mode := os.Getenv("UAT_TEST_HELPER"); mode != "" {
		runTestHelper(mode)
		return
	}
	os.Exit(m.Run())
}

func runTestHelper(mode string) {
	if rf := os.Getenv("UAT_TEST_REPORT"); rf != "" {
		data := fmt.Sprintf("DB_PATH=%s\nRENDER_SERVICE_URL=%s\n",
			os.Getenv("DB_PATH"), os.Getenv("RENDER_SERVICE_URL"))
		_ = os.WriteFile(rf, []byte(data), 0o644)
	}
	switch mode {
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		select {}
	case "nostart":
		// Print a marker so readiness-failure tests can verify logs.
		fmt.Fprintln(os.Stderr, "UAT_TEST_HELPER_MARKER not_listening")
		select {}
	}
	addr := os.Getenv("UAT_TEST_ADDR")
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper listen error:", err)
		os.Exit(1)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "unready" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := http.Serve(ln, handler); err != nil {
		os.Exit(0)
	}
}

// freePort returns a localhost TCP port that was free at the moment
// of the call. There is a small race between closing the listener and
// the helper subprocess binding the port, but it is acceptable for
// tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("freePort close: %v", err)
	}
	return port
}

// helperEnv returns the env that re-execs the test binary into helper
// mode. Callers append additional vars (e.g., UAT_TEST_ADDR).
func helperEnv(mode, reportPath string) []string {
	env := []string{"UAT_TEST_HELPER=" + mode}
	if reportPath != "" {
		env = append(env, "UAT_TEST_REPORT="+reportPath)
	}
	return env
}

// helperStartCommand builds a `sh -c` payload that re-execs the
// current test binary so the supervisor (which uses `sh -c`) spawns
// our helper process. The env vars (UAT_TEST_HELPER, UAT_TEST_ADDR,
// UAT_TEST_REPORT) come through cmd.Env, not the command string.
func helperStartCommand() string {
	return os.Args[0]
}

func TestParseConfig_Defaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg, code := parseConfig(nil, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("expected exitOK, got %d, stderr=%q", code, stderr.String())
	}
	if cfg.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL default = %q", cfg.baseURL)
	}
	if cfg.startCommand != "" {
		t.Errorf("startCommand default = %q", cfg.startCommand)
	}
	if cfg.timeout != 30*time.Second {
		t.Errorf("timeout default = %v", cfg.timeout)
	}
	if cfg.verbose != false {
		t.Errorf("verbose default = %v", cfg.verbose)
	}
	if cfg.skipDestructive != false {
		t.Errorf("skipDestructive default = %v", cfg.skipDestructive)
	}
	if cfg.renderURL != "" {
		t.Errorf("renderURL default = %q", cfg.renderURL)
	}
}

func TestParseConfig_CustomValues(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{
		"--base-url", "http://example.com",
		"--start-command", "go run .",
		"--timeout", "45s",
		"--verbose",
		"--skip-destructive",
		"--render-url", "http://render.example.com",
	}
	cfg, code := parseConfig(args, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("expected exitOK, got %d, stderr=%q", code, stderr.String())
	}
	if cfg.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q", cfg.baseURL)
	}
	if cfg.startCommand != "go run ." {
		t.Errorf("startCommand = %q", cfg.startCommand)
	}
	if cfg.timeout != 45*time.Second {
		t.Errorf("timeout = %v", cfg.timeout)
	}
	if !cfg.verbose {
		t.Errorf("verbose should be true")
	}
	if !cfg.skipDestructive {
		t.Errorf("skipDestructive should be true")
	}
	if cfg.renderURL != "http://render.example.com" {
		t.Errorf("renderURL = %q", cfg.renderURL)
	}
}

func TestParseConfig_UnknownFlagReturnsUsageExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, code := parseConfig([]string{"--bogus"}, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("expected exitUsage (%d), got %d", exitUsage, code)
	}
}

func TestParseConfig_InvalidDurationReturnsUsageExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, code := parseConfig([]string{"--timeout", "not-a-duration"}, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("expected exitUsage (%d), got %d", exitUsage, code)
	}
}

func TestExitCodeConstants(t *testing.T) {
	if exitOK != 0 {
		t.Errorf("exitOK = %d, want 0", exitOK)
	}
	if exitBehaviorFailure != 1 {
		t.Errorf("exitBehaviorFailure = %d, want 1", exitBehaviorFailure)
	}
	if exitUsage != 2 {
		t.Errorf("exitUsage = %d, want 2", exitUsage)
	}
}

func TestParseConfig_HelpListsAllFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, _ = parseConfig([]string{"--help"}, &stdout, &stderr)
	usage := stderr.String() + stdout.String()
	for _, flag := range []string{"base-url", "start-command", "timeout", "verbose", "skip-destructive", "render-url"} {
		if !strings.Contains(usage, flag) {
			t.Errorf("usage output missing flag %q; got: %s", flag, usage)
		}
	}
}

func TestWaitReady_ImmediateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := waitReady(ctx, srv.Client(), srv.URL); err != nil {
		t.Fatalf("waitReady returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waitReady took too long for immediate success: %v", elapsed)
	}
}

func TestWaitReady_EventualSuccess(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := waitReady(ctx, srv.Client(), srv.URL); err != nil {
		t.Fatalf("waitReady returned error: %v", err)
	}
	if got := attempts.Load(); got < 3 {
		t.Errorf("expected at least 3 attempts, got %d", got)
	}
}

func TestWaitReady_ContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := waitReady(ctx, srv.Client(), srv.URL)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("waitReady should have errored on timeout")
	}
	if elapsed > 2*time.Second {
		t.Errorf("waitReady took too long to return after timeout: %v", elapsed)
	}
}

func TestRun_ExistingServiceModeNoSubprocess(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config{
		baseURL:      srv.URL,
		startCommand: "",
		timeout:      2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	code := run(ctx, cfg, nil, []Check{}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("run returned %d, stderr=%q", code, stderr.String())
	}
	if attempts.Load() < 1 {
		t.Errorf("expected readiness probe against base URL, got 0 attempts")
	}
	// Existing-service mode must not create temp dirs, set DB_PATH, or spawn subprocesses.
	if v := stderr.String(); strings.Contains(v, "subprocess") || strings.Contains(v, "DB_PATH") {
		t.Errorf("existing-service mode leaked subprocess/env setup: %q", v)
	}
}

func TestRun_ExistingServiceModeReadinessTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := config{
		baseURL:      srv.URL,
		startCommand: "",
		timeout:      400 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	code := run(ctx, cfg, nil, []Check{}, &stdout, &stderr)
	if code != exitBehaviorFailure {
		t.Fatalf("expected exitBehaviorFailure for readiness timeout, got %d", code)
	}
}

// startTestSupervisor launches the test-binary helper in server mode
// on the given port via the supervisor and returns the supervisor and
// the report file path. The caller MUST defer sup.Stop().
func startTestSupervisor(t *testing.T, extraEnv []string, port int) (*supervisor, string) {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "report.txt")
	env := append(helperEnv("server", reportPath),
		fmt.Sprintf("UAT_TEST_ADDR=127.0.0.1:%d", port))
	env = append(env, extraEnv...)
	cfg := config{
		baseURL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		startCommand: helperStartCommand(),
		timeout:      5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	t.Cleanup(cancel)
	client := &http.Client{Timeout: cfg.timeout}
	var stdout, stderr bytes.Buffer
	sup, err := startSelfManaged(ctx, cfg, env, client, &stdout, &stderr)
	if err != nil {
		t.Fatalf("startSelfManaged: %v\nstdout=%q\nstderr=%q", err, stdout.String(), stderr.String())
	}
	return sup, reportPath
}

func TestStartSelfManaged_InjectsDBPathAndExtraEnv(t *testing.T) {
	port := freePort(t)
	sup, reportPath := startTestSupervisor(t, []string{"RENDER_SERVICE_URL=http://render.test/x"}, port)
	defer sup.Stop()

	if sup.DBPath() == "" {
		t.Fatalf("supervisor DBPath empty")
	}
	if !strings.HasPrefix(sup.DBPath(), sup.TempDir()+string(os.PathSeparator)) {
		t.Errorf("DBPath %q not inside TempDir %q", sup.DBPath(), sup.TempDir())
	}
	if !strings.HasSuffix(sup.DBPath(), "uat-motivations.db") {
		t.Errorf("DBPath %q does not end with uat-motivations.db", sup.DBPath())
	}

	// Wait briefly for the helper to write its report file (it writes
	// before listening, but readiness already proved it bound the port).
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := string(data)
	if !strings.Contains(report, "DB_PATH="+sup.DBPath()+"\n") {
		t.Errorf("child env missing DB_PATH; report=%q", report)
	}
	if !strings.Contains(report, "RENDER_SERVICE_URL=http://render.test/x\n") {
		t.Errorf("child env missing RENDER_SERVICE_URL; report=%q", report)
	}
}

func TestSupervisor_StopRemovesTempDirAndTerminatesChild(t *testing.T) {
	port := freePort(t)
	sup, _ := startTestSupervisor(t, nil, port)
	tempDir := sup.TempDir()
	pid := sup.cmd.Process.Pid

	sup.Stop()

	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Errorf("temp dir %q should be removed, stat err=%v", tempDir, err)
	}
	// After Stop returns, Wait must have completed. Confirm the
	// process is no longer reachable.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("subprocess pid=%d still alive after Stop", pid)
	}
	// Calling Stop again must be a no-op (idempotent).
	sup.Stop()
}

func TestSupervisor_KillsStubbornChildAfterTimeout(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.txt")
	// stubborn mode ignores SIGTERM, so the supervisor must escalate
	// to SIGKILL after stopTimeout. Use a tiny stopTimeout to keep the
	// test fast.
	port := freePort(t)
	env := append(helperEnv("stubborn", reportPath),
		fmt.Sprintf("UAT_TEST_ADDR=127.0.0.1:%d", port))
	cfg := config{
		baseURL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		startCommand: helperStartCommand(),
		timeout:      3 * time.Second,
	}
	// stubborn helper never listens, so we cannot rely on readiness.
	// Call startSelfManaged with a context that will time out, then
	// observe Stop's SIGKILL path on the resulting (returned) error.
	// Instead, bypass startSelfManaged readiness by constructing a
	// supervisor manually around the exec.Cmd.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cfg.startCommand)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tempDir := t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stubborn helper: %v", err)
	}
	sup := &supervisor{
		cmd:         cmd,
		tempDir:     tempDir,
		waitDone:    make(chan error, 1),
		logBuf:      &bytes.Buffer{},
		stopTimeout: 200 * time.Millisecond,
	}
	go func() { sup.waitDone <- cmd.Wait() }()

	// Give the child a moment to install its signal handler.
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	sup.Stop()
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("Stop took too long against stubborn child: %v", elapsed)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err == nil {
		t.Errorf("stubborn subprocess pid=%d still alive after Stop", cmd.Process.Pid)
	}
}

func TestSupervisor_CleanupOnPanic(t *testing.T) {
	port := freePort(t)
	sup, _ := startTestSupervisor(t, nil, port)
	tempDir := sup.TempDir()
	pid := sup.cmd.Process.Pid

	func() {
		defer func() {
			_ = recover()
		}()
		defer sup.Stop()
		panic("simulated check panic")
	}()

	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Errorf("temp dir %q should be removed after panic, stat err=%v", tempDir, err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("subprocess pid=%d still alive after panic+Stop", pid)
	}
}

func TestStartSelfManaged_ReadinessTimeoutIncludesLogs(t *testing.T) {
	port := freePort(t)
	reportPath := filepath.Join(t.TempDir(), "report.txt")
	env := append(helperEnv("nostart", reportPath),
		fmt.Sprintf("UAT_TEST_ADDR=127.0.0.1:%d", port))
	cfg := config{
		baseURL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		startCommand: helperStartCommand(),
		timeout:      600 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	client := &http.Client{Timeout: cfg.timeout}
	var stdout, stderr bytes.Buffer

	sup, err := startSelfManaged(ctx, cfg, env, client, &stdout, &stderr)
	if err == nil {
		sup.Stop()
		t.Fatalf("expected readiness error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "service readiness on") {
		t.Errorf("error missing readiness context: %q", msg)
	}
	if !strings.Contains(msg, "subprocess logs:") {
		t.Errorf("error missing 'subprocess logs:' prefix: %q", msg)
	}
	if !strings.Contains(msg, "UAT_TEST_HELPER_MARKER not_listening") {
		t.Errorf("error missing child stderr marker: %q", msg)
	}
}

func TestRun_SelfManagedMode_LifecycleAndCleanup(t *testing.T) {
	port := freePort(t)
	reportPath := filepath.Join(t.TempDir(), "report.txt")
	cfg := config{
		baseURL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		startCommand: helperStartCommand(),
		timeout:      5 * time.Second,
	}
	// Snapshot temp roots so we can detect that run cleaned up its
	// uat-* directory after exit.
	tempRoot := os.TempDir()
	before, err := matchTempDirs(tempRoot)
	if err != nil {
		t.Fatalf("snapshot temp before: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	extraEnv := []string{
		"UAT_TEST_HELPER=server",
		"UAT_TEST_ADDR=127.0.0.1:" + fmt.Sprint(port),
		"UAT_TEST_REPORT=" + reportPath,
		"RENDER_SERVICE_URL=http://render.test/integration",
	}
	var observedDBPath string
	checks := []Check{{
		Name: "snapshot-env",
		Run: func(ctx context.Context, env *Env) error {
			data, err := os.ReadFile(reportPath)
			if err != nil {
				return fmt.Errorf("read report: %w", err)
			}
			observedDBPath = parseReportValue(string(data), "DB_PATH")
			return nil
		},
	}}
	var stdout, stderr bytes.Buffer
	code := run(ctx, cfg, extraEnv, checks, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("run returned %d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if observedDBPath == "" {
		t.Fatalf("check did not observe DB_PATH")
	}
	if !strings.HasSuffix(observedDBPath, "uat-motivations.db") {
		t.Errorf("DB_PATH %q does not look isolated", observedDBPath)
	}
	if _, err := os.Stat(filepath.Dir(observedDBPath)); !os.IsNotExist(err) {
		t.Errorf("temp dir %q should be removed after run, stat err=%v", filepath.Dir(observedDBPath), err)
	}
	after, err := matchTempDirs(tempRoot)
	if err != nil {
		t.Fatalf("snapshot temp after: %v", err)
	}
	for p := range after {
		if !before[p] {
			t.Errorf("run leaked temp dir: %s", p)
		}
	}
}

// matchTempDirs returns the set of entries under root whose name
// starts with "uat-".
func matchTempDirs(root string) (map[string]bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "uat-") {
			out[filepath.Join(root, e.Name())] = true
		}
	}
	return out, nil
}

// parseReportValue extracts the value for KEY=VAL from the helper
// report file content (one key per line).
func parseReportValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

// runCheckAgainst executes a Check against a temporary httptest.Server and
// returns the resulting error. The Check is constructed via the supplied
// factory after the server URL is known, so checks can capture env.RunID
// at construction time if desired.
func runCheckAgainst(t *testing.T, handler http.Handler, build func() Check) error {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	c := build()
	return c.Run(context.Background(), env)
}

func TestCheckLandingPage_PassesWhenAllSnippetsPresent(t *testing.T) {
	body := "Welcome to the Random Motivation API\nGET /motivation\nPOST /motivation\n" +
		"GET /motivations\nDELETE /motivation/:id\nGET /motivations.png\n"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
	if err := runCheckAgainst(t, h, checkLandingPage); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckLandingPage_TaggedNonDestructive(t *testing.T) {
	c := checkLandingPage()
	if c.Kind&nonDestructive == 0 {
		t.Errorf("landing page check should be tagged nonDestructive")
	}
	if c.Kind&destructive != 0 {
		t.Errorf("landing page check must not be tagged destructive")
	}
}

func TestCheckLandingPage_FailsWhenStatusNotOK(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := runCheckAgainst(t, h, checkLandingPage)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/", "500"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckLandingPage_FailsWhenMissingSnippet(t *testing.T) {
	// Missing "GET /motivations.png" only; the other new snippets
	// ("GET /motivations", "DELETE /motivation/:id") are present so this
	// still isolates the final-snippet failure path.
	body := "Welcome to the Random Motivation API\nGET /motivation\nPOST /motivation\n" +
		"GET /motivations\nDELETE /motivation/:id\n"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
	err := runCheckAgainst(t, h, checkLandingPage)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/", "motivations.png"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckEmptyPOSTRejected_PassesWhen400AndExpectedBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/motivation" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) != 0 {
			t.Errorf("expected empty body, got %q", b)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Motivation cannot be empty")
	})
	if err := runCheckAgainst(t, h, checkEmptyPOSTRejected); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckEmptyPOSTRejected_TaggedNonDestructive(t *testing.T) {
	c := checkEmptyPOSTRejected()
	if c.Kind&nonDestructive == 0 || c.Kind&destructive != 0 {
		t.Errorf("empty POST check should be tagged nonDestructive only, got kind=%d", c.Kind)
	}
}

func TestCheckEmptyPOSTRejected_FailsWhen201(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "Motivation added successfully")
	})
	err := runCheckAgainst(t, h, checkEmptyPOSTRejected)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected status mismatch detail, got: %s", err)
	}
}

func TestCheckEmptyPOSTRejected_FailsOnWrongMessage(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "some other error")
	})
	err := runCheckAgainst(t, h, checkEmptyPOSTRejected)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Motivation cannot be empty") {
		t.Errorf("expected expected-message reference, got: %s", err)
	}
}

func TestCheckWhitespacePOSTRejected_PassesWhen400AndExpectedBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/motivation" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		if len(s) == 0 || strings.TrimSpace(s) != "" {
			t.Errorf("expected whitespace-only body, got %q", s)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Motivation cannot be empty")
	})
	if err := runCheckAgainst(t, h, checkWhitespacePOSTRejected); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckWhitespacePOSTRejected_TaggedNonDestructive(t *testing.T) {
	c := checkWhitespacePOSTRejected()
	if c.Kind&nonDestructive == 0 || c.Kind&destructive != 0 {
		t.Errorf("whitespace POST check should be tagged nonDestructive only, got kind=%d", c.Kind)
	}
}

func TestCheckWhitespacePOSTRejected_FailsOnWrongStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "Motivation added successfully")
	})
	err := runCheckAgainst(t, h, checkWhitespacePOSTRejected)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected status detail, got: %s", err)
	}
}

func TestCheckValidPOSTAccepted_PassesWhen201AndSuccessMessage(t *testing.T) {
	var receivedBody string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/motivation" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "Motivation added successfully")
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-xyz"
	c := checkValidPOSTAccepted()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if receivedBody == "" {
		t.Fatal("expected non-empty body to be sent")
	}
	if !strings.Contains(receivedBody, env.RunID) {
		t.Errorf("expected body %q to contain run ID %q", receivedBody, env.RunID)
	}
}

func TestCheckValidPOSTAccepted_TaggedDestructive(t *testing.T) {
	c := checkValidPOSTAccepted()
	if c.Kind&destructive == 0 {
		t.Errorf("valid POST check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckValidPOSTAccepted_FailsOnWrongStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := runCheckAgainst(t, h, checkValidPOSTAccepted)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "201") {
		t.Errorf("expected status detail, got: %s", err)
	}
}

func TestCheckValidPOSTAccepted_FailsOnWrongMessage(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "something else")
	})
	err := runCheckAgainst(t, h, checkValidPOSTAccepted)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Motivation added successfully") {
		t.Errorf("expected expected-message reference, got: %s", err)
	}
}

func TestCheckEmptyMotivationCollection_PassesWhen404AndExpectedBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/motivation" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "No motivations found")
	})
	if err := runCheckAgainst(t, h, checkEmptyMotivationCollection); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckEmptyMotivationCollection_TaggedDestructive(t *testing.T) {
	c := checkEmptyMotivationCollection()
	if c.Kind&destructive == 0 {
		t.Errorf("empty motivation collection check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckEmptyMotivationCollection_FailsWhen200(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Some motivation")
	})
	err := runCheckAgainst(t, h, checkEmptyMotivationCollection)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivation", "404", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckEmptyMotivationCollection_FailsOnWrongBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not the expected body")
	})
	err := runCheckAgainst(t, h, checkEmptyMotivationCollection)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivation", "No motivations found"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckPNGNoMotivations_PassesWhen404AndExpectedBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/motivations.png" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "No motivations found")
	})
	if err := runCheckAgainst(t, h, checkPNGNoMotivations); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckPNGNoMotivations_TaggedDestructive(t *testing.T) {
	c := checkPNGNoMotivations()
	if c.Kind&destructive == 0 {
		t.Errorf("PNG no motivations check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckPNGNoMotivations_FailsWhen200(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "\x89PNG\r\n\x1a\n")
	})
	err := runCheckAgainst(t, h, checkPNGNoMotivations)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "404", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckPNGNoMotivations_FailsOnWrongBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not the expected body")
	})
	err := runCheckAgainst(t, h, checkPNGNoMotivations)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "No motivations found"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckUnsupportedMethods_PassesWhenAll405(t *testing.T) {
	type seenKey struct{ method, path string }
	seen := map[seenKey]bool{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[seenKey{r.Method, r.URL.Path}] = true
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	if err := runCheckAgainst(t, h, checkUnsupportedMethods); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	want := []seenKey{
		{http.MethodPut, "/motivation"},
		{http.MethodDelete, "/motivation"},
		{http.MethodPost, "/motivations.png"},
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("expected check to send %s %s", k.method, k.path)
		}
	}
}

func TestCheckUnsupportedMethods_TaggedNonDestructive(t *testing.T) {
	c := checkUnsupportedMethods()
	if c.Kind&nonDestructive == 0 || c.Kind&destructive != 0 {
		t.Errorf("unsupported methods check should be tagged nonDestructive only, got kind=%d", c.Kind)
	}
}

func TestCheckUnsupportedMethods_FailsIdentifiesMethodAndPath(t *testing.T) {
	// PUT /motivation returns 200 instead of 405 -> error must mention it.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/motivation" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	err := runCheckAgainst(t, h, checkUnsupportedMethods)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"PUT", "/motivation", "405", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckUnsupportedMethods_FailsIdentifiesPOSTPng(t *testing.T) {
	// POST /motivations.png returns 201 instead of 405.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/motivations.png" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	err := runCheckAgainst(t, h, checkUnsupportedMethods)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"POST", "/motivations.png", "405"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckUnknownRoute_PassesWhen404AndPathHasRunID(t *testing.T) {
	var observedPath, observedMethod string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedMethod = r.Method
		observedPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-abc123"
	c := checkUnknownRoute()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if observedMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", observedMethod)
	}
	if !strings.Contains(observedPath, env.RunID) {
		t.Errorf("expected path %q to include run ID %q", observedPath, env.RunID)
	}
}

func TestCheckUnknownRoute_TaggedNonDestructive(t *testing.T) {
	c := checkUnknownRoute()
	if c.Kind&nonDestructive == 0 || c.Kind&destructive != 0 {
		t.Errorf("unknown route check should be tagged nonDestructive only, got kind=%d", c.Kind)
	}
}

func TestCheckUnknownRoute_FailsWhen200(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	err := runCheckAgainst(t, h, checkUnknownRoute)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "404", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

// pngRenderSuccessHandler builds the app-emulation handler used by
// checkPNGRenderSuccess unit tests. POST /motivation stashes the body
// (trimmed, matching the real service) and returns 201. GET
// /motivations.png proxies to the supplied fake render service using
// the stashed text, then returns the render service's status,
// Content-Type, and body to the caller.
func pngRenderSuccessHandler(t *testing.T, fr *fakeRender, stashed *string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			*stashed = strings.TrimSpace(string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			renderURL := fr.URL() + "/render?text=" + url.QueryEscape(*stashed)
			resp, err := http.Get(renderURL)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestCheckPNGRenderSuccess_PassesAndForwardsTextToRender(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()
	var stashed string
	srv := httptest.NewServer(pngRenderSuccessHandler(t, fr, &stashed))
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-png"
	c := checkPNGRenderSuccess()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	want := "uat-render-success-" + env.RunID
	if stashed != want {
		t.Errorf("emulated app stashed %q, want %q", stashed, want)
	}
	texts := fr.Texts()
	if len(texts) == 0 {
		t.Fatal("expected fake render to record at least one text")
	}
	if got := texts[len(texts)-1]; got != want {
		t.Errorf("fake render last recorded text = %q, want %q", got, want)
	}
}

func TestCheckPNGRenderSuccess_TaggedDestructiveAndRenderRequired(t *testing.T) {
	c := checkPNGRenderSuccess()
	if c.Kind&destructive == 0 {
		t.Errorf("PNG render success check should be tagged destructive, got kind=%d", c.Kind)
	}
	if c.Kind&renderRequired == 0 {
		t.Errorf("PNG render success check should be tagged renderRequired, got kind=%d", c.Kind)
	}
}

func TestCheckPNGRenderSuccess_FailsOnWrongPNGStatus(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	c := checkPNGRenderSuccess()
	err := c.Run(context.Background(), env)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "200", "500"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckPNGRenderSuccess_FailsOnWrongContentType(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(png1x1)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	c := checkPNGRenderSuccess()
	err := c.Run(context.Background(), env)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "image/png", "text/plain"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckRenderServiceUnreachable_PassesWhen500AndExpectedBody(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = strings.TrimSpace(string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "Error rendering motivation image")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-unreachable"
	c := checkRenderServiceUnreachable()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	want := "uat-render-unreachable-" + env.RunID
	if stashed != want {
		t.Errorf("emulated app stashed %q, want %q", stashed, want)
	}
}

func TestCheckRenderServiceUnreachable_TaggedDestructiveAndRenderRequired(t *testing.T) {
	c := checkRenderServiceUnreachable()
	if c.Kind&destructive == 0 {
		t.Errorf("render-unreachable check should be tagged destructive, got kind=%d", c.Kind)
	}
	if c.Kind&renderRequired == 0 {
		t.Errorf("render-unreachable check should be tagged renderRequired, got kind=%d", c.Kind)
	}
}

func TestCheckRenderServiceUnreachable_FailsOnWrongPNGStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Error rendering motivation image")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkRenderServiceUnreachable)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "500", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckRenderServiceUnreachable_FailsOnWrongBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "something else")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkRenderServiceUnreachable)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "Error rendering motivation image"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckRenderServiceNonOK_PassesWhen500AndExpectedBody(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = strings.TrimSpace(string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "Error rendering motivation image")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-nonok"
	c := checkRenderServiceNonOK()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	want := "uat-render-nonok-" + env.RunID
	if stashed != want {
		t.Errorf("emulated app stashed %q, want %q", stashed, want)
	}
}

func TestCheckRenderServiceNonOK_TaggedDestructiveAndRenderRequired(t *testing.T) {
	c := checkRenderServiceNonOK()
	if c.Kind&destructive == 0 {
		t.Errorf("render-non-OK check should be tagged destructive, got kind=%d", c.Kind)
	}
	if c.Kind&renderRequired == 0 {
		t.Errorf("render-non-OK check should be tagged renderRequired, got kind=%d", c.Kind)
	}
}

func TestCheckRenderServiceNonOK_FailsOnWrongPNGStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Error rendering motivation image")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkRenderServiceNonOK)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "500", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckRenderServiceNonOK_FailsOnWrongBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "something else")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkRenderServiceNonOK)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "Error rendering motivation image"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckPNGRenderSuccess_FailsOnWrongBytes(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations.png":
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-the-png-fixture"))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	c := checkPNGRenderSuccess()
	err := c.Run(context.Background(), env)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "PNG fixture"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckSubmittedMotivationRetrievableIsolated_PassesWhenGETReturnsSubmittedText(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, stashed)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-iso"
	c := checkSubmittedMotivationRetrievableIsolated()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	want := "uat-retrievable-isolated-" + env.RunID
	if stashed != want {
		t.Errorf("emulated app stashed %q, want %q", stashed, want)
	}
}

func TestCheckSubmittedMotivationRetrievableIsolated_TaggedDestructive(t *testing.T) {
	c := checkSubmittedMotivationRetrievableIsolated()
	if c.Kind&destructive == 0 {
		t.Errorf("retrievable isolated check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckSubmittedMotivationRetrievableIsolated_FailsOnWrongGETBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "some other motivation")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkSubmittedMotivationRetrievableIsolated)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivation", "some other motivation"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckSubmittedMotivationRetrievableIsolated_FailsOnWrongPOSTStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/motivation" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	err := runCheckAgainst(t, h, checkSubmittedMotivationRetrievableIsolated)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"POST", "/motivation", "201", "500"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckTrimmedSubmission_PassesWhenServerTrimsAndReturnsCore(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = strings.TrimSpace(string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, stashed)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-trim"
	c := checkTrimmedSubmission()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	want := "Stay focused " + env.RunID + "."
	if stashed != want {
		t.Errorf("emulated app stashed %q, want %q", stashed, want)
	}
}

func TestCheckTrimmedSubmission_TaggedDestructive(t *testing.T) {
	c := checkTrimmedSubmission()
	if c.Kind&destructive == 0 {
		t.Errorf("trimmed submission check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckTrimmedSubmission_FailsWhenServerDoesNotTrim(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, stashed)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	err := runCheckAgainst(t, h, checkTrimmedSubmission)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivation", "Stay focused"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckTrimmedSubmission_FailsOnWrongPOSTStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/motivation" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	err := runCheckAgainst(t, h, checkTrimmedSubmission)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"POST", "/motivation", "201", "500"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

// fakeRotationState models the deployed service's fixed-order
// MotivationQueue for tests: GET /motivation walks entries in
// insertion order and wraps around; POST appends a new entry to the
// end of that order without reshuffling; DELETE removes an entry from
// both the backing list and the rotation (matching the real service's
// documented eviction-on-delete behavior).
type fakeRotationState struct {
	mu         sync.Mutex
	items      []motivationListItem
	nextID     int64
	cursor     int
	deletedIDs []int64
}

// newFakeRotationServer starts an httptest.Server backed by a
// fakeRotationState pre-seeded with seedCount entries (texts
// "seed-0".."seed-N"), and returns both the server and the state so
// tests can inspect it after a check runs (e.g. to confirm cleanup).
func newFakeRotationServer(t *testing.T, seedCount int) (*httptest.Server, *fakeRotationState) {
	t.Helper()
	state := &fakeRotationState{}
	for i := 0; i < seedCount; i++ {
		state.nextID++
		state.items = append(state.items, motivationListItem{
			ID:        state.nextID,
			Text:      fmt.Sprintf("seed-%d", i),
			CreatedAt: "2024-01-01T00:00:00Z",
		})
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			b, err := json.Marshal(state.items)
			if err != nil {
				t.Fatalf("marshal fake motivations list: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			body, _ := io.ReadAll(r.Body)
			state.nextID++
			state.items = append(state.items, motivationListItem{
				ID:        state.nextID,
				Text:      strings.TrimSpace(string(body)),
				CreatedAt: "2024-01-01T00:00:00Z",
			})
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			if len(state.items) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			item := state.items[state.cursor%len(state.items)]
			state.cursor++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, item.Text)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/motivation/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/motivation/")
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			idx := -1
			for i, item := range state.items {
				if item.ID == id {
					idx = i
					break
				}
			}
			if idx == -1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			state.deletedIDs = append(state.deletedIDs, id)
			state.items = append(state.items[:idx], state.items[idx+1:]...)
			if state.cursor > idx {
				state.cursor--
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, state
}

func TestRunRetrievableExisting_PassesAndCleansUpWithCorrectID(t *testing.T) {
	srv, state := newFakeRotationServer(t, 0)

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-existing"
	if err := runRetrievableExisting(context.Background(), env, 4, time.Millisecond); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.items) != 0 {
		t.Errorf("expected empty collection after cleanup, got %d entries: %+v", len(state.items), state.items)
	}
	if len(state.deletedIDs) != 1 {
		t.Fatalf("expected exactly one cleanup DELETE, got %d", len(state.deletedIDs))
	}
	if state.deletedIDs[0] != 1 {
		t.Errorf("cleanup DELETE used id=%d, want the id assigned to the posted motivation (1)", state.deletedIDs[0])
	}
}

// TestRunRetrievableExisting_RegressionLargeCollectionWorstCasePosition is
// the regression test for the fixed-rotation bug: with 25 pre-existing
// entries, the posted motivation is appended at index 25 (the last slot
// of a 26-entry rotation), and with the rotation cursor starting at 0 it
// is only served on the 26th GET /motivation call. The old hardcoded
// budget of 20 attempts could never observe it here and would fail
// against this perfectly healthy setup; the derived n+1 budget (25+1=26,
// plus safetyBuffer=0 to pin the exact boundary) must succeed.
func TestRunRetrievableExisting_RegressionLargeCollectionWorstCasePosition(t *testing.T) {
	srv, state := newFakeRotationServer(t, 25)

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-existing-large"
	if err := runRetrievableExisting(context.Background(), env, 0, time.Millisecond); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.items) != 25 {
		t.Errorf("expected 25 entries after cleanup, got %d: %+v", len(state.items), state.items)
	}
	want := "uat-retrievable-existing-" + env.RunID
	for _, item := range state.items {
		if item.Text == want {
			t.Errorf("cleanup did not delete %q", want)
		}
	}
}

func TestCheckSubmittedMotivationRetrievableExisting_TaggedNonDestructive(t *testing.T) {
	c := checkSubmittedMotivationRetrievableExisting()
	if c.Kind&nonDestructive == 0 || c.Kind&destructive != 0 {
		t.Errorf("retrievable existing check should be tagged nonDestructive only, got kind=%d", c.Kind)
	}
}

func TestRunRetrievableExisting_FailsWhenSubmittedTextNeverAppearsButCleansUpAnyway(t *testing.T) {
	var mu sync.Mutex
	var items []motivationListItem
	var nextID int64
	var deletedIDs []int64

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			b, _ := json.Marshal(items)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			body, _ := io.ReadAll(r.Body)
			nextID++
			items = append(items, motivationListItem{
				ID: nextID, Text: strings.TrimSpace(string(body)), CreatedAt: "2024-01-01T00:00:00Z",
			})
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "never the submitted text")
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/motivation/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/motivation/")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			deletedIDs = append(deletedIDs, id)
			for i, it := range items {
				if it.ID == id {
					items = append(items[:i], items[i+1:]...)
					break
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-existing-fail"
	// n=0 before POST, safetyBuffer=2 -> attempts = 0+1+2 = 3.
	err := runRetrievableExisting(context.Background(), env, 2, time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	want := "uat-retrievable-existing-" + env.RunID
	for _, sub := range []string{"GET", "/motivation", want, "3 attempts"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deletedIDs) != 1 {
		t.Fatalf("expected cleanup DELETE to fire exactly once even on poll failure, got %d", len(deletedIDs))
	}
	if len(items) != 0 {
		t.Errorf("expected cleanup to remove the row, got %d remaining: %+v", len(items), items)
	}
}

func TestRunRetrievableExisting_FailsWhenInitialMotivationsListFails(t *testing.T) {
	var postCalled bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			postCalled = true
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-list-fail"
	err := runRetrievableExisting(context.Background(), env, 2, time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "/motivations") {
		t.Errorf("expected error to mention /motivations, got: %s", err.Error())
	}
	if postCalled {
		t.Error("expected POST /motivation to never be called when the initial GET /motivations fails")
	}
}

func TestRunRetrievableExisting_CleanupFailureSurfacedWhenPollSucceeds(t *testing.T) {
	var posted bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			if posted {
				_, _ = io.WriteString(w, `[{"id":1,"text":"uat-retrievable-existing-test-run-cleanup-fail","created_at":"2024-01-01T00:00:00Z"}]`)
				return
			}
			_, _ = io.WriteString(w, "[]")
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			posted = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "uat-retrievable-existing-test-run-cleanup-fail")
		case r.Method == http.MethodDelete && r.URL.Path == "/motivation/1":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-cleanup-fail"
	err := runRetrievableExisting(context.Background(), env, 2, time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, sub := range []string{"cleanup", "500"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
	// The poll itself succeeded (the payload was observed), so the
	// surfaced error must be purely about the cleanup failure, not a
	// poll-not-observed message masking it or being masked by it.
	if strings.Contains(msg, "not observed") {
		t.Errorf("expected pure cleanup failure language, got poll-failure language too: %s", msg)
	}
}

func TestRunRetrievableExisting_PollAndCleanupBothFail(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"id":1,"text":"uat-retrievable-existing-test-run-both-fail","created_at":"2024-01-01T00:00:00Z"}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "never matches")
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-both-fail"
	err := runRetrievableExisting(context.Background(), env, 0, time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	// Both failures must be visible: the poll failure is the primary,
	// actionable error and must not be replaced by the cleanup failure,
	// but the cleanup failure must not be silently dropped either.
	for _, sub := range []string{"not observed", "cleanup failed", "500"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
}

func TestCheckMultipleMotivationsRetrievable_PassesWhenAllSubmittedTextsCycle(t *testing.T) {
	var submitted []string
	var gets atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			submitted = append(submitted, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			n := int(gets.Add(1)) - 1
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, submitted[n%len(submitted)])
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-multi"
	c := checkMultipleMotivationsRetrievable()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(submitted) != multipleRetrievableSubmissions {
		t.Errorf("expected %d POSTs, got %d (submitted=%q)",
			multipleRetrievableSubmissions, len(submitted), submitted)
	}
	for i, s := range submitted {
		want := fmt.Sprintf("uat multi %s #%d", env.RunID, i+1)
		if s != want {
			t.Errorf("submitted[%d]=%q, want %q", i, s, want)
		}
	}
}

func TestCheckMultipleMotivationsRetrievable_TaggedDestructive(t *testing.T) {
	c := checkMultipleMotivationsRetrievable()
	if c.Kind&destructive == 0 {
		t.Errorf("multiple motivations retrievable check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckMultipleMotivationsRetrievable_FailsWhenSubmittedTextNeverObserved(t *testing.T) {
	var submitted []string
	var gets atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			submitted = append(submitted, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			// Only ever return the first two submitted texts.
			n := int(gets.Add(1)) - 1
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, submitted[n%2])
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-multi-missing"
	err := runMultipleMotivationsRetrievable(context.Background(), env, 3, 6)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	missing := fmt.Sprintf("uat multi %s #3", env.RunID)
	for _, sub := range []string{"GET", "/motivation", "6 attempts", missing} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
}

func TestCheckMultipleMotivationsRetrievable_FailsOnUnknownGETBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "rogue")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-multi-rogue"
	err := runMultipleMotivationsRetrievable(context.Background(), env, 2, 4)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, sub := range []string{"GET", "/motivation", "rogue", "attempt 1"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
}

func TestCheckMultipleMotivationsRetrievable_FailsOnNon200GET(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-multi-500"
	err := runMultipleMotivationsRetrievable(context.Background(), env, 2, 4)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, sub := range []string{"GET", "/motivation", "500", "200", "attempt 1"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
}

func TestCheckRepeatedGETAvailability_PassesWhenAllGETsReturnKnownBody(t *testing.T) {
	var submitted []string
	var gets atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			submitted = append(submitted, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			n := int(gets.Add(1)) - 1
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, submitted[n%len(submitted)])
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-avail"
	c := checkRepeatedGETAvailability()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(submitted) != repeatedGETAvailabilitySubmissions {
		t.Errorf("expected %d POSTs, got %d (submitted=%q)",
			repeatedGETAvailabilitySubmissions, len(submitted), submitted)
	}
	for i, s := range submitted {
		want := fmt.Sprintf("uat avail %s #%d", env.RunID, i+1)
		if s != want {
			t.Errorf("submitted[%d]=%q, want %q", i, s, want)
		}
	}
	if got := int(gets.Load()); got != repeatedGETAvailabilityAttempts {
		t.Errorf("expected %d GET attempts, got %d",
			repeatedGETAvailabilityAttempts, got)
	}
}

func TestCheckRepeatedGETAvailability_TaggedDestructive(t *testing.T) {
	c := checkRepeatedGETAvailability()
	if c.Kind&destructive == 0 {
		t.Errorf("repeated GET availability check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckRepeatedGETAvailability_FailsOnNon200GET(t *testing.T) {
	var submitted []string
	var gets atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			submitted = append(submitted, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			n := int(gets.Add(1))
			if n == 5 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "No motivations found")
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, submitted[(n-1)%len(submitted)])
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-avail-404"
	err := runRepeatedGETAvailability(context.Background(), env, 3, 7)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, sub := range []string{"GET", "/motivation", "attempt 5", "404", "200"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
}

func TestCheckRepeatedGETAvailability_FailsOnUnknownGETBody(t *testing.T) {
	var submitted []string
	var gets atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			submitted = append(submitted, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			n := int(gets.Add(1))
			w.WriteHeader(http.StatusOK)
			if n == 3 {
				_, _ = io.WriteString(w, "rogue body")
				return
			}
			_, _ = io.WriteString(w, submitted[(n-1)%len(submitted)])
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-avail-rogue"
	err := runRepeatedGETAvailability(context.Background(), env, 2, 5)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, sub := range []string{"GET", "/motivation", "attempt 3", "rogue body"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
}

func TestCheckRepeatedGETAvailability_FailsOnWrongPOSTStatus(t *testing.T) {
	var posts atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/motivation" {
			n := int(posts.Add(1))
			if n == 2 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-avail-postfail"
	err := runRepeatedGETAvailability(context.Background(), env, 3, 5)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, sub := range []string{"POST", "/motivation", "submission #2", "201", "500"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("expected error to mention %q, got: %s", sub, msg)
		}
	}
}

// checkNames returns the ordered list of check names from a []Check.
func checkNames(checks []Check) []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name
	}
	return out
}

func TestBuildExistingServiceSuite_ContainsExpectedChecksInOrder(t *testing.T) {
	suite := buildExistingServiceSuite()
	wantOrder := []string{
		"landing page describes API",
		"empty motivation POST is rejected",
		"whitespace motivation POST is rejected",
		"unsupported methods are rejected with 405",
		"unknown route returns 404",
		"DELETE unknown motivation id returns 404",
		"DELETE unparseable motivation id returns 400",
		"submitted motivation is eventually retrievable (existing service)",
	}
	gotOrder := checkNames(suite)
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("len mismatch: got=%v want=%v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("position %d: got=%q want=%q", i, gotOrder[i], wantOrder[i])
		}
	}

	// After selection with default cfg in modeExisting, all destructive
	// entries must be filtered out. The current suite contains none,
	// so the selected list must equal the input list.
	selected := selectChecks(modeExisting, config{}, suite)
	for _, c := range selected {
		if c.Kind&destructive != 0 {
			t.Errorf("existing-service selected suite still contains destructive check %q", c.Name)
		}
	}
	if len(selected) != len(suite) {
		t.Errorf("expected selectChecks to keep all entries; got %d, want %d", len(selected), len(suite))
	}
}

func TestSelfManagedGroups_OrderingConstraints(t *testing.T) {
	groups := buildSelfManagedGroups()
	if len(groups) != 10 {
		t.Fatalf("expected 10 groups, got %d", len(groups))
	}

	// Group A
	a := groups[0]
	wantA := []string{
		"landing page describes API",
		"empty motivation POST is rejected",
		"whitespace motivation POST is rejected",
		"unsupported methods are rejected with 405",
		"unknown route returns 404",
		"DELETE unknown motivation id returns 404",
		"DELETE unparseable motivation id returns 400",
		"empty motivation collection returns 404",
		"empty motivation collection PNG returns 404",
		"empty motivations list returns empty JSON array",
		"submitted motivation is trimmed before storage",
	}
	gotA := checkNames(a.checks)
	if len(gotA) != len(wantA) {
		t.Fatalf("group A length mismatch: got=%v want=%v", gotA, wantA)
	}
	for i := range wantA {
		if gotA[i] != wantA[i] {
			t.Errorf("group A position %d: got=%q want=%q", i, gotA[i], wantA[i])
		}
	}
	// emptyCollection (T13), pngNone (T19), and the empty-list check
	// (T22) must precede the trimmed-submission check (T15), which is
	// the only state-mutating POST in group A.
	idxEmpty := indexOfName(gotA, "empty motivation collection returns 404")
	idxPNGNone := indexOfName(gotA, "empty motivation collection PNG returns 404")
	idxListEmpty := indexOfName(gotA, "empty motivations list returns empty JSON array")
	idxTrimmed := indexOfName(gotA, "submitted motivation is trimmed before storage")
	if !(idxEmpty < idxTrimmed && idxPNGNone < idxTrimmed && idxListEmpty < idxTrimmed) {
		t.Errorf("group A: empty/pngNone/listEmpty must precede trimmed POST; idxEmpty=%d idxPNGNone=%d idxListEmpty=%d idxTrimmed=%d",
			idxEmpty, idxPNGNone, idxListEmpty, idxTrimmed)
	}
	// T24 and T25 are nonDestructive checks duplicated from the
	// existing-service suite (see buildExistingServiceSuite) so they
	// also run in self-managed/CI mode; they must be present in group A
	// alongside the other nonDestructive checks (T11, T12) rather than
	// only in existing-service mode, and (being stateless) must precede
	// the trimmed-submission POST like the rest of group A's
	// nonDestructive checks.
	idxDeleteUnknown := indexOfName(gotA, "DELETE unknown motivation id returns 404")
	idxDeleteInvalid := indexOfName(gotA, "DELETE unparseable motivation id returns 400")
	if idxDeleteUnknown < 0 {
		t.Errorf("group A: missing T24 (DELETE unknown motivation id returns 404)")
	}
	if idxDeleteInvalid < 0 {
		t.Errorf("group A: missing T25 (DELETE unparseable motivation id returns 400)")
	}
	if !(idxDeleteUnknown < idxTrimmed && idxDeleteInvalid < idxTrimmed) {
		t.Errorf("group A: T24/T25 must precede trimmed POST; idxDeleteUnknown=%d idxDeleteInvalid=%d idxTrimmed=%d",
			idxDeleteUnknown, idxDeleteInvalid, idxTrimmed)
	}

	// Group B: T14-isolated alone (single-entry state).
	b := groups[1]
	wantB := []string{"submitted motivation is retrievable (isolated)"}
	if got := checkNames(b.checks); !equalStrings(got, wantB) {
		t.Errorf("group B: got=%v want=%v", got, wantB)
	}

	// Group C: T23 needs solo state (asserts its own POST is the only
	// list entry).
	c := groups[2]
	wantC := []string{"motivations list reflects a single POST (isolated)"}
	if got := checkNames(c.checks); !equalStrings(got, wantC) {
		t.Errorf("group C: got=%v want=%v", got, wantC)
	}

	// Group D: T10 + T18 share a DB (neither asserts solo state).
	d := groups[3]
	wantD := []string{
		"valid motivation POST is accepted",
		"PNG render success",
	}
	if got := checkNames(d.checks); !equalStrings(got, wantD) {
		t.Errorf("group D: got=%v want=%v", got, wantD)
	}

	// Group E: T16 needs solo state.
	e := groups[4]
	wantE := []string{"multiple submitted motivations are retrievable (isolated)"}
	if got := checkNames(e.checks); !equalStrings(got, wantE) {
		t.Errorf("group E: got=%v want=%v", got, wantE)
	}

	// Group F: T17 needs solo state.
	f := groups[5]
	wantF := []string{"repeated GET /motivation remains available (isolated)"}
	if got := checkNames(f.checks); !equalStrings(got, wantF) {
		t.Errorf("group F: got=%v want=%v", got, wantF)
	}

	// Group G: T26 needs solo state (asserts exactly two entries, then one).
	g := groups[6]
	wantG := []string{"DELETE removes a motivation from the list (isolated)"}
	if got := checkNames(g.checks); !equalStrings(got, wantG) {
		t.Errorf("group G: got=%v want=%v", got, wantG)
	}

	// Group H: T27 needs solo state (regression test for queue eviction).
	h := groups[7]
	wantH := []string{"deleted motivation stops being served by GET /motivation (isolated)"}
	if got := checkNames(h.checks); !equalStrings(got, wantH) {
		t.Errorf("group H: got=%v want=%v", got, wantH)
	}

	// Group I: render unreachable.
	i := groups[8]
	wantI := []string{"PNG render fails when render service is unreachable"}
	if got := checkNames(i.checks); !equalStrings(got, wantI) {
		t.Errorf("group I: got=%v want=%v", got, wantI)
	}

	// Group J: render non-OK.
	j := groups[9]
	wantJ := []string{"PNG render fails when render service returns non-OK"}
	if got := checkNames(j.checks); !equalStrings(got, wantJ) {
		t.Errorf("group J: got=%v want=%v", got, wantJ)
	}
}

func indexOfName(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// noopSetup is a renderSetup that returns a placeholder URL and a
// no-op cleanup, suitable for unit tests that do not need a real
// render server.
func noopSetup() (string, func(), error) {
	return "http://127.0.0.1:1/render", func() {}, nil
}

func TestRunGroups_AllPassReturnsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	groups := []selfManagedGroup{
		{name: "g1", checks: []Check{{Name: "c1", Run: func(ctx context.Context, e *Env) error { return nil }}}, setup: noopSetup},
		{name: "g2", checks: []Check{{Name: "c2", Run: func(ctx context.Context, e *Env) error { return nil }}}, setup: noopSetup},
	}
	var calls int
	runOne := func(ctx context.Context, cfg config, extraEnv []string, checks []Check, stdout, stderr io.Writer) int {
		calls++
		// Expect RENDER_SERVICE_URL injected into extraEnv.
		found := false
		for _, kv := range extraEnv {
			if strings.HasPrefix(kv, "RENDER_SERVICE_URL=") {
				found = true
			}
		}
		if !found {
			t.Errorf("runOne call %d missing RENDER_SERVICE_URL in extraEnv=%v", calls, extraEnv)
		}
		return exitOK
	}
	code := runGroups(context.Background(), config{}, groups, &stdout, &stderr, runOne)
	if code != exitOK {
		t.Errorf("expected exitOK, got %d; stderr=%q", code, stderr.String())
	}
	if calls != 2 {
		t.Errorf("expected runOne called 2 times, got %d", calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "===== group g1 =====") || !strings.Contains(out, "===== group g2 =====") {
		t.Errorf("expected group headers in stdout, got: %s", out)
	}
}

func TestRunGroups_AnyFailureReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	groups := []selfManagedGroup{
		{name: "g1", checks: []Check{{Name: "c1"}}, setup: noopSetup},
		{name: "g2", checks: []Check{{Name: "c2"}}, setup: noopSetup},
		{name: "g3", checks: []Check{{Name: "c3"}}, setup: noopSetup},
	}
	var calls int
	runOne := func(ctx context.Context, cfg config, extraEnv []string, checks []Check, stdout, stderr io.Writer) int {
		calls++
		if calls == 2 {
			return exitBehaviorFailure
		}
		return exitOK
	}
	code := runGroups(context.Background(), config{}, groups, &stdout, &stderr, runOne)
	if code != exitBehaviorFailure {
		t.Errorf("expected exitBehaviorFailure, got %d", code)
	}
	// All three groups should still have been attempted.
	if calls != 3 {
		t.Errorf("expected runOne called 3 times (no early exit), got %d", calls)
	}
}

func TestRunGroups_StopsOnContextCancelBetweenGroups(t *testing.T) {
	var stdout, stderr bytes.Buffer
	groups := []selfManagedGroup{
		{name: "g1", checks: []Check{{Name: "c1"}}, setup: noopSetup},
		{name: "g2", checks: []Check{{Name: "c2"}}, setup: noopSetup},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	runOne := func(ctx context.Context, cfg config, extraEnv []string, checks []Check, stdout, stderr io.Writer) int {
		calls++
		cancel()
		return exitOK
	}
	code := runGroups(ctx, config{}, groups, &stdout, &stderr, runOne)
	if code != exitBehaviorFailure {
		t.Errorf("expected exitBehaviorFailure on ctx cancel, got %d", code)
	}
	if calls != 1 {
		t.Errorf("expected runOne called 1 time before ctx cancel detected, got %d", calls)
	}
}

func TestPickUnreachableAddr(t *testing.T) {
	rawURL, err := pickUnreachableAddr()
	if err != nil {
		t.Fatalf("pickUnreachableAddr error: %v", err)
	}
	if !strings.HasSuffix(rawURL, "/render") {
		t.Errorf("expected URL to end with /render, got %q", rawURL)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error: %v", rawURL, err)
	}
	// Attempt a TCP dial: it must fail because we closed the listener.
	conn, err := net.DialTimeout("tcp", parsed.Host, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Errorf("expected dial to %s to fail (port should be closed)", parsed.Host)
	}
}

func TestNewFailingRender(t *testing.T) {
	srv, urlStr := newFailingRender(http.StatusInternalServerError)
	defer srv.Close()
	if !strings.HasSuffix(urlStr, "/render") {
		t.Errorf("expected URL to end with /render, got %q", urlStr)
	}
	resp, err := http.Get(urlStr + "?text=anything")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}
}

// --- decodeMotivationList ---

func TestDecodeMotivationList_ParsesArray(t *testing.T) {
	items, err := decodeMotivationList([]byte(`[{"id":1,"text":"a","created_at":"2020-01-01"},{"id":2,"text":"b","created_at":"2020-01-02"}]`))
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != 1 || items[0].Text != "a" || items[0].CreatedAt != "2020-01-01" {
		t.Errorf("unexpected item[0]: %+v", items[0])
	}
}

func TestDecodeMotivationList_ParsesEmptyArray(t *testing.T) {
	items, err := decodeMotivationList([]byte(`[]`))
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

func TestDecodeMotivationList_ErrorsOnMalformedJSON(t *testing.T) {
	_, err := decodeMotivationList([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- checkMotivationsListEmpty (T22) ---

func TestCheckMotivationsListEmpty_PassesOn200EmptyArray(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/motivations" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "[]")
	})
	if err := runCheckAgainst(t, h, checkMotivationsListEmpty); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckMotivationsListEmpty_TaggedDestructive(t *testing.T) {
	c := checkMotivationsListEmpty()
	if c.Kind&destructive == 0 {
		t.Errorf("motivations list empty check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckMotivationsListEmpty_FailsWhenStatusNot200(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "[]")
	})
	err := runCheckAgainst(t, h, checkMotivationsListEmpty)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected status detail, got: %s", err)
	}
}

func TestCheckMotivationsListEmpty_FailsWhenBodyIsNull(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "null")
	})
	err := runCheckAgainst(t, h, checkMotivationsListEmpty)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "null") {
		t.Errorf("expected error to mention null body, got: %s", err)
	}
}

func TestCheckMotivationsListEmpty_FailsWhenArrayNonEmpty(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `[{"id":1,"text":"leftover","created_at":"2020-01-01"}]`)
	})
	err := runCheckAgainst(t, h, checkMotivationsListEmpty)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "1 entries") {
		t.Errorf("expected entry count detail, got: %s", err)
	}
}

func TestCheckMotivationsListEmpty_FailsOnMalformedBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not json")
	})
	err := runCheckAgainst(t, h, checkMotivationsListEmpty)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- checkMotivationsListAfterPost (T23) ---

func TestCheckMotivationsListAfterPost_PassesWithSingleMatchingEntry(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":42,"text":%q,"created_at":"2024-01-01T00:00:00Z"}]`, stashed)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-list-after-post"
	c := checkMotivationsListAfterPost()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	want := "uat-list-after-post-" + env.RunID
	if stashed != want {
		t.Errorf("emulated app stashed %q, want %q", stashed, want)
	}
}

func TestCheckMotivationsListAfterPost_TaggedDestructive(t *testing.T) {
	c := checkMotivationsListAfterPost()
	if c.Kind&destructive == 0 {
		t.Errorf("list-after-post check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckMotivationsListAfterPost_FailsWhenEntryCountWrong(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "[]")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkMotivationsListAfterPost)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exactly 1 entry") {
		t.Errorf("expected entry-count detail, got: %s", err)
	}
}

func TestCheckMotivationsListAfterPost_FailsWhenTextMismatch(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"id":1,"text":"wrong text","created_at":"2024-01-01"}]`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkMotivationsListAfterPost)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "wrong text") {
		t.Errorf("expected text mismatch detail, got: %s", err)
	}
}

func TestCheckMotivationsListAfterPost_FailsWhenIDNotPositive(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":0,"text":%q,"created_at":"2024-01-01"}]`, stashed)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkMotivationsListAfterPost)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("expected id detail, got: %s", err)
	}
}

func TestCheckMotivationsListAfterPost_FailsWhenCreatedAtEmpty(t *testing.T) {
	var stashed string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			stashed = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":7,"text":%q,"created_at":""}]`, stashed)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkMotivationsListAfterPost)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "created_at") {
		t.Errorf("expected created_at detail, got: %s", err)
	}
}

// --- checkDeleteUnknownID (T24) ---

func TestCheckDeleteUnknownID_PassesWhen404AndExpectedBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/motivation/9223372036854775807" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "Motivation not found")
	})
	if err := runCheckAgainst(t, h, checkDeleteUnknownID); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckDeleteUnknownID_TaggedNonDestructive(t *testing.T) {
	c := checkDeleteUnknownID()
	if c.Kind&nonDestructive == 0 || c.Kind&destructive != 0 {
		t.Errorf("delete-unknown-id check should be tagged nonDestructive only, got kind=%d", c.Kind)
	}
}

func TestCheckDeleteUnknownID_FailsOnWrongStatus(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	err := runCheckAgainst(t, h, checkDeleteUnknownID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected status detail, got: %s", err)
	}
}

func TestCheckDeleteUnknownID_FailsOnWrongBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "nope")
	})
	err := runCheckAgainst(t, h, checkDeleteUnknownID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Motivation not found") {
		t.Errorf("expected body detail, got: %s", err)
	}
}

// --- checkDeleteInvalidID (T25) ---

func TestCheckDeleteInvalidID_PassesWhenAllVariantsReturn400(t *testing.T) {
	var seenPaths []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Invalid motivation id")
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-invalid-id"
	c := checkDeleteInvalidID()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(seenPaths) < 3 {
		t.Errorf("expected at least 3 variants exercised, got %d: %v", len(seenPaths), seenPaths)
	}
	joined := strings.Join(seenPaths, " ")
	for _, want := range []string{"not-a-number", "1.5", env.RunID} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected one of the exercised paths to contain %q, got %v", want, seenPaths)
		}
	}
}

func TestCheckDeleteInvalidID_TaggedNonDestructive(t *testing.T) {
	c := checkDeleteInvalidID()
	if c.Kind&nonDestructive == 0 || c.Kind&destructive != 0 {
		t.Errorf("delete-invalid-id check should be tagged nonDestructive only, got kind=%d", c.Kind)
	}
}

func TestCheckDeleteInvalidID_FailsOnWrongStatusForOneVariant(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "1.5") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Invalid motivation id")
	})
	err := runCheckAgainst(t, h, checkDeleteInvalidID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "1.5") {
		t.Errorf("expected error to identify offending id, got: %s", err)
	}
}

func TestCheckDeleteInvalidID_FailsOnWrongBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "nope")
	})
	err := runCheckAgainst(t, h, checkDeleteInvalidID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Invalid motivation id") {
		t.Errorf("expected body detail, got: %s", err)
	}
}

// --- checkDeleteRemovesFromList (T26) ---

func TestCheckDeleteRemovesFromList_PassesWhenSecondSurvives(t *testing.T) {
	type fakeEntry struct {
		ID        int64  `json:"id"`
		Text      string `json:"text"`
		CreatedAt string `json:"created_at"`
	}
	var mu sync.Mutex
	var items []fakeEntry
	var nextID int64 = 1
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			items = append(items, fakeEntry{ID: nextID, Text: string(b), CreatedAt: "2024-01-01T00:00:00Z"})
			nextID++
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			buf, err := json.Marshal(items)
			if err != nil {
				t.Fatalf("marshal items: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buf)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/motivation/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/motivation/")
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "Invalid motivation id")
				return
			}
			found := -1
			for i, it := range items {
				if it.ID == id {
					found = i
					break
				}
			}
			if found == -1 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "Motivation not found")
				return
			}
			items = append(items[:found], items[found+1:]...)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-delrmlist"
	c := checkDeleteRemovesFromList()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckDeleteRemovesFromList_TaggedDestructive(t *testing.T) {
	c := checkDeleteRemovesFromList()
	if c.Kind&destructive == 0 {
		t.Errorf("delete-removes-from-list check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckDeleteRemovesFromList_FailsWhenInitialListCountWrong(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "[]")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeleteRemovesFromList)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "2 entries before delete") {
		t.Errorf("expected entry-count detail, got: %s", err)
	}
}

func TestCheckDeleteRemovesFromList_FailsWhenDeleteStatusWrong(t *testing.T) {
	var texts []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			texts = append(texts, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":11,"text":%q,"created_at":"2024-01-01"},{"id":22,"text":%q,"created_at":"2024-01-01"}]`, texts[0], texts[1])
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeleteRemovesFromList)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "204") {
		t.Errorf("expected status detail, got: %s", err)
	}
}

func TestCheckDeleteRemovesFromList_FailsWhenFinalListCountWrong(t *testing.T) {
	var texts []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			texts = append(texts, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			// Always returns both entries, simulating a delete that
			// didn't take effect.
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":11,"text":%q,"created_at":"2024-01-01"},{"id":22,"text":%q,"created_at":"2024-01-01"}]`, texts[0], texts[1])
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeleteRemovesFromList)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "1 entry after delete") {
		t.Errorf("expected final-count detail, got: %s", err)
	}
}

func TestCheckDeleteRemovesFromList_FailsWhenSurvivorTextWrong(t *testing.T) {
	var texts []string
	var getCalls int
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			texts = append(texts, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			getCalls++
			w.WriteHeader(http.StatusOK)
			if getCalls == 1 {
				fmt.Fprintf(w, `[{"id":11,"text":%q,"created_at":"2024-01-01"},{"id":22,"text":%q,"created_at":"2024-01-01"}]`, texts[0], texts[1])
				return
			}
			// After delete, return a single entry with unexpected text.
			_, _ = io.WriteString(w, `[{"id":22,"text":"corrupted","created_at":"2024-01-01"}]`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeleteRemovesFromList)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "remaining entry text") {
		t.Errorf("expected survivor-text detail, got: %s", err)
	}
}

// --- checkDeletedMotivationNotServed (T27) ---

func TestCheckDeletedMotivationNotServed_PassesWhenDeletedNeverServedAndSurvivorSeen(t *testing.T) {
	type qEntry struct {
		id   int64
		text string
	}
	var mu sync.Mutex
	var queue []qEntry
	var nextID int64 = 1
	pos := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			queue = append(queue, qEntry{id: nextID, text: string(b)})
			nextID++
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			var sb strings.Builder
			sb.WriteString("[")
			for i, e := range queue {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"id":%d,"text":%q,"created_at":"2024-01-01"}`, e.id, e.text)
			}
			sb.WriteString("]")
			_, _ = io.WriteString(w, sb.String())
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/motivation/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/motivation/")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			idx := -1
			for i, e := range queue {
				if e.id == id {
					idx = i
					break
				}
			}
			if idx == -1 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "Motivation not found")
				return
			}
			queue = append(queue[:idx], queue[idx+1:]...)
			if len(queue) == 0 {
				pos = 0
			} else {
				pos %= len(queue)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			if len(queue) == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "No motivations found")
				return
			}
			e := queue[pos]
			pos = (pos + 1) % len(queue)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, e.text)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	env := newTestEnv(srv.URL, &bytes.Buffer{}, false)
	env.RunID = "test-run-notserved"
	c := checkDeletedMotivationNotServed()
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckDeletedMotivationNotServed_TaggedDestructive(t *testing.T) {
	c := checkDeletedMotivationNotServed()
	if c.Kind&destructive == 0 {
		t.Errorf("deleted-not-served check should be tagged destructive, got kind=%d", c.Kind)
	}
}

func TestCheckDeletedMotivationNotServed_FailsWhenVictimNotFoundInList(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "[]")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeletedMotivationNotServed)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "could not locate victim") {
		t.Errorf("expected locate-victim detail, got: %s", err)
	}
}

func TestCheckDeletedMotivationNotServed_FailsWhenDeletedTextStillServed(t *testing.T) {
	var texts []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			texts = append(texts, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":1,"text":%q,"created_at":"2024-01-01"},{"id":2,"text":%q,"created_at":"2024-01-01"}]`, texts[0], texts[1])
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			// Regression: keeps serving the deleted (victim) text.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, texts[0])
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeletedMotivationNotServed)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "eviction from rotation regressed") {
		t.Errorf("expected regression detail, got: %s", err)
	}
}

func TestCheckDeletedMotivationNotServed_FailsWhenSurvivorNeverServed(t *testing.T) {
	var texts []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			texts = append(texts, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":1,"text":%q,"created_at":"2024-01-01"},{"id":2,"text":%q,"created_at":"2024-01-01"}]`, texts[0], texts[1])
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "No motivations found")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeletedMotivationNotServed)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected status detail from GET /motivation, got: %s", err)
	}
}

func TestCheckDeletedMotivationNotServed_FailsOnUnexpectedBody(t *testing.T) {
	var texts []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/motivation":
			b, _ := io.ReadAll(r.Body)
			texts = append(texts, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "Motivation added successfully")
		case r.Method == http.MethodGet && r.URL.Path == "/motivations":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"id":1,"text":%q,"created_at":"2024-01-01"},{"id":2,"text":%q,"created_at":"2024-01-01"}]`, texts[0], texts[1])
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/motivation":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "rogue text")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	err := runCheckAgainst(t, h, checkDeletedMotivationNotServed)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rogue text") {
		t.Errorf("expected unexpected-body detail, got: %s", err)
	}
}
