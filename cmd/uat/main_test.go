package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
	code := run(ctx, cfg, []Check{}, &stdout, &stderr)
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
	code := run(ctx, cfg, []Check{}, &stdout, &stderr)
	if code != exitBehaviorFailure {
		t.Fatalf("expected exitBehaviorFailure for readiness timeout, got %d", code)
	}
}
