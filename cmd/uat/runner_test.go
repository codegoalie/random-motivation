package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestEnv(baseURL string, w io.Writer, verbose bool) *Env {
	return &Env{
		BaseURL: baseURL,
		Client:  &http.Client{},
		RunID:   "test-run",
		Verbose: verbose,
		Out:     w,
		Err:     w,
	}
}

func TestRunChecks_AllPassReturnsOK(t *testing.T) {
	var out bytes.Buffer
	env := newTestEnv("http://example.invalid", &out, false)

	checks := []Check{
		{Name: "first", Kind: nonDestructive, Run: func(ctx context.Context, e *Env) error { return nil }},
		{Name: "second", Kind: nonDestructive, Run: func(ctx context.Context, e *Env) error { return nil }},
	}
	res := runChecks(context.Background(), env, checks)
	if res.Failed != 0 {
		t.Errorf("expected 0 failures, got %d", res.Failed)
	}
	if res.Passed != 2 {
		t.Errorf("expected 2 passes, got %d", res.Passed)
	}
	if res.Total != 2 {
		t.Errorf("expected total 2, got %d", res.Total)
	}
	output := out.String()
	if !strings.Contains(output, "PASS first") || !strings.Contains(output, "PASS second") {
		t.Errorf("expected PASS lines, got: %s", output)
	}
}

func TestRunChecks_FailureRecordedAndReason(t *testing.T) {
	var out bytes.Buffer
	env := newTestEnv("http://example.invalid", &out, false)

	checks := []Check{
		{Name: "ok-check", Kind: nonDestructive, Run: func(ctx context.Context, e *Env) error { return nil }},
		{Name: "bad-check", Kind: nonDestructive, Run: func(ctx context.Context, e *Env) error { return errors.New("boom") }},
	}
	res := runChecks(context.Background(), env, checks)
	if res.Failed != 1 {
		t.Errorf("expected 1 failure, got %d", res.Failed)
	}
	if res.Passed != 1 {
		t.Errorf("expected 1 pass, got %d", res.Passed)
	}
	output := out.String()
	if !strings.Contains(output, "FAIL bad-check") {
		t.Errorf("expected FAIL line for bad-check, got: %s", output)
	}
	if !strings.Contains(output, "boom") {
		t.Errorf("expected failure reason 'boom' in output, got: %s", output)
	}
}

func TestRunChecks_SummaryCounts(t *testing.T) {
	var out bytes.Buffer
	env := newTestEnv("http://example.invalid", &out, false)

	checks := []Check{
		{Name: "a", Kind: nonDestructive, Run: func(ctx context.Context, e *Env) error { return nil }},
		{Name: "b", Kind: nonDestructive, Run: func(ctx context.Context, e *Env) error { return nil }},
		{Name: "c", Kind: nonDestructive, Run: func(ctx context.Context, e *Env) error { return errors.New("nope") }},
	}
	res := runChecks(context.Background(), env, checks)
	if res.Total != 3 || res.Passed != 2 || res.Failed != 1 {
		t.Errorf("counts wrong: total=%d passed=%d failed=%d", res.Total, res.Passed, res.Failed)
	}
	output := out.String()
	// Summary should include the totals.
	if !strings.Contains(output, "total=3") || !strings.Contains(output, "passed=2") || !strings.Contains(output, "failed=1") {
		t.Errorf("summary missing counts: %s", output)
	}
}

func TestAssertStatus_IncludesMethodAndPath(t *testing.T) {
	err := assertStatus("GET", "/motivation", 500, 200)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivation", "500", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q, got: %s", want, msg)
		}
	}
}

func TestAssertStatus_NilWhenEqual(t *testing.T) {
	if err := assertStatus("GET", "/motivation", 200, 200); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestAssertBodyEquals_IncludesMethodAndPath(t *testing.T) {
	err := assertBodyEquals("POST", "/motivation", "got-body", "want-body")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"POST", "/motivation", "got-body", "want-body"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q, got: %s", want, msg)
		}
	}
}

func TestAssertBodyContains_FailsWhenMissing(t *testing.T) {
	err := assertBodyContains("GET", "/", "hello world", "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/", "missing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q, got: %s", want, msg)
		}
	}
}

func TestAssertBodyContains_PassesWhenPresent(t *testing.T) {
	if err := assertBodyContains("GET", "/", "hello world", "world"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestAssertContentTypePrefix_IncludesMethodAndPath(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	err := assertContentTypePrefix("GET", "/motivations.png", h, "image/png")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/motivations.png", "image/png", "text/html"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q, got: %s", want, msg)
		}
	}
}

func TestAssertContentTypePrefix_PassesOnPrefixMatch(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "image/png")
	if err := assertContentTypePrefix("GET", "/motivations.png", h, "image/png"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestDoRequest_JoinsBaseURLWithPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var out bytes.Buffer
	env := newTestEnv(srv.URL, &out, false)
	resp, body, err := doRequest(context.Background(), env, "GET", "/motivation", nil)
	if err != nil {
		t.Fatalf("doRequest err: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
	if gotPath != "/motivation" {
		t.Errorf("server saw path %q", gotPath)
	}
}

func TestDoRequest_PreservesAbsoluteURL(t *testing.T) {
	var hitMarker string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitMarker = "other"
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	var out bytes.Buffer
	env := newTestEnv("http://base.invalid", &out, false)
	resp, _, err := doRequest(context.Background(), env, "GET", other.URL+"/abs", nil)
	if err != nil {
		t.Fatalf("doRequest err: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if hitMarker != "other" {
		t.Errorf("expected request to hit other server")
	}
}

func TestDoRequest_VerbosePrintsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var out bytes.Buffer
	env := newTestEnv(srv.URL, &out, true)
	_, _, err := doRequest(context.Background(), env, "GET", "/x", nil)
	if err != nil {
		t.Fatalf("doRequest err: %v", err)
	}
	logged := out.String()
	if !strings.Contains(logged, "GET") || !strings.Contains(logged, "/x") {
		t.Errorf("expected verbose log to mention method and path, got: %s", logged)
	}
}

func TestNewRunID_NonEmptyAndUnique(t *testing.T) {
	a := newRunID()
	b := newRunID()
	if a == "" || b == "" {
		t.Fatal("runID should not be empty")
	}
	if a == b {
		t.Errorf("expected unique runIDs, got %q twice", a)
	}
}
