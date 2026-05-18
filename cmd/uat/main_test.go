package main

import (
	"bytes"
	"strings"
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
