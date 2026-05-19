package main

import (
	"bytes"
	"context"
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
	"strings"
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
	body := "Welcome to the Random Motivation API\nGET /motivation\nPOST /motivation\nGET /motivations.png\n"
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
	// Missing "GET /motivations.png".
	body := "Welcome to the Random Motivation API\nGET /motivation\nPOST /motivation\n"
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
