package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CheckKind is a bitmask describing properties of a UAT check that the
// suite selector cares about.
type CheckKind uint8

const (
	// nonDestructive checks are safe to run against any reachable instance.
	nonDestructive CheckKind = 1 << iota
	// destructive checks assume an empty/isolated database or otherwise
	// mutate state in a way unsafe for arbitrary deployed services.
	destructive
	// renderRequired checks need control or explicit configuration of the
	// render service.
	renderRequired
)

// Env carries shared state for an individual UAT run.
type Env struct {
	BaseURL   string
	Client    *http.Client
	RunID     string
	Verbose   bool
	RenderURL string
	Out       io.Writer
	Err       io.Writer
}

// Check is a single UAT behavior verification.
type Check struct {
	Name string
	Kind CheckKind
	Run  func(ctx context.Context, env *Env) error
}

// Result summarizes a UAT run.
type Result struct {
	Total   int
	Passed  int
	Failed  int
	Skipped int
}

// runChecks executes the given checks in declared order, writing one PASS
// or FAIL line per check to env.Out and a final summary line. Errors
// returned by checks are reported as failures; the function itself does
// not return an error.
func runChecks(ctx context.Context, env *Env, checks []Check) Result {
	res := Result{Total: len(checks)}
	for _, c := range checks {
		err := c.Run(ctx, env)
		if err != nil {
			res.Failed++
			fmt.Fprintf(env.Out, "FAIL %s: %s\n", c.Name, err.Error())
			continue
		}
		res.Passed++
		fmt.Fprintf(env.Out, "PASS %s\n", c.Name)
	}
	fmt.Fprintf(env.Out, "summary: total=%d passed=%d failed=%d skipped=%d\n",
		res.Total, res.Passed, res.Failed, res.Skipped)
	return res
}

// newRunID generates a unique identifier for a UAT run. The format
// combines a UTC timestamp with random bytes for collision resistance.
func newRunID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

// doRequest performs an HTTP request against env.BaseURL+pathOrURL, or
// against pathOrURL directly if it is an absolute URL. It returns the
// response, the fully-read response body, and any error.
func doRequest(ctx context.Context, env *Env, method, pathOrURL string, body io.Reader) (*http.Response, []byte, error) {
	url := pathOrURL
	if !strings.HasPrefix(pathOrURL, "http://") && !strings.HasPrefix(pathOrURL, "https://") {
		url = strings.TrimRight(env.BaseURL, "/") + "/" + strings.TrimLeft(pathOrURL, "/")
	}
	if env.Verbose && env.Out != nil {
		fmt.Fprintf(env.Out, "→ %s %s\n", method, url)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, nil, fmt.Errorf("build request %s %s: %w", method, url, err)
	}
	resp, err := env.Client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("%s %s: read body: %w", method, url, err)
	}
	if env.Verbose && env.Out != nil {
		fmt.Fprintf(env.Out, "← %s %s status=%d bytes=%d\n", method, url, resp.StatusCode, len(respBody))
	}
	return resp, respBody, nil
}

// assertStatus returns nil when got == want, otherwise a descriptive
// error that includes method and path for readable CI logs.
func assertStatus(method, path string, got, want int) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("%s %s: status got=%d want=%d", method, path, got, want)
}

// assertBodyEquals returns nil when got == want; otherwise an error that
// includes method, path, the got body, and the wanted body.
func assertBodyEquals(method, path, got, want string) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("%s %s: body got=%q want=%q", method, path, got, want)
}

// assertBodyContains returns nil when got contains substring; otherwise
// an error that includes method, path, the substring, and the got body.
func assertBodyContains(method, path, got, substring string) error {
	if strings.Contains(got, substring) {
		return nil
	}
	return fmt.Errorf("%s %s: body missing %q (got=%q)", method, path, substring, got)
}

// assertContentTypePrefix returns nil when the Content-Type header has
// the given prefix; otherwise an error that includes method and path.
func assertContentTypePrefix(method, path string, headers http.Header, prefix string) error {
	ct := headers.Get("Content-Type")
	if strings.HasPrefix(ct, prefix) {
		return nil
	}
	return fmt.Errorf("%s %s: content-type got=%q want prefix=%q", method, path, ct, prefix)
}
