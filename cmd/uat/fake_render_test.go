package main

import (
	"bytes"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestFakeRenderStartupAndClose ensures the helper boots a real HTTP
// server reachable on a local URL and that Close shuts it down.
func TestFakeRenderStartupAndClose(t *testing.T) {
	fr := newFakeRender()
	if fr == nil {
		t.Fatal("newFakeRender returned nil")
	}
	if fr.URL() == "" {
		t.Fatal("newFakeRender URL is empty")
	}
	if !strings.HasPrefix(fr.URL(), "http://") {
		t.Fatalf("expected http:// URL, got %q", fr.URL())
	}
	resp, err := http.Get(fr.URL() + "/render?text=ping")
	if err != nil {
		t.Fatalf("request to fake render: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	fr.Close()
	if _, err := http.Get(fr.URL() + "/render?text=after-close"); err == nil {
		t.Fatal("expected error after Close, got nil")
	}
}

// TestFakeRenderDefaultPNG checks the default success response: 200
// status, image/png Content-Type, and the embedded PNG fixture.
func TestFakeRenderDefaultPNG(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()

	resp, err := http.Get(fr.URL() + "/render?text=hello")
	if err != nil {
		t.Fatalf("GET /render: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type: want image/png, got %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, png1x1) {
		t.Fatalf("body does not equal png1x1 fixture (len=%d vs %d)", len(body), len(png1x1))
	}
}

// TestFakeRenderPNGFixtureIsValid decodes the embedded fixture using
// image/png to prove it is a real 1x1 PNG.
func TestFakeRenderPNGFixtureIsValid(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(png1x1))
	if err != nil {
		t.Fatalf("decode png1x1: %v", err)
	}
	if img.Bounds().Dx() != 1 || img.Bounds().Dy() != 1 {
		t.Fatalf("png1x1 bounds: want 1x1, got %dx%d",
			img.Bounds().Dx(), img.Bounds().Dy())
	}
}

// TestFakeRenderRecordsText asserts the helper records the text query
// parameter from every request in order.
func TestFakeRenderRecordsText(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()

	want := []string{"alpha", "beta gamma", ""}
	for _, txt := range want {
		req, err := http.NewRequest(http.MethodGet, fr.URL()+"/render", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		q := req.URL.Query()
		q.Set("text", txt)
		req.URL.RawQuery = q.Encode()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	got := fr.Texts()
	if len(got) != len(want) {
		t.Fatalf("recorded texts: want %d, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recorded texts[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

// TestFakeRenderTextsReturnsCopy ensures the slice returned by Texts is
// independent of the helper's internal state.
func TestFakeRenderTextsReturnsCopy(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()

	resp, err := http.Get(fr.URL() + "/render?text=one")
	if err != nil {
		t.Fatalf("GET /render: %v", err)
	}
	resp.Body.Close()

	snap := fr.Texts()
	if len(snap) != 1 {
		t.Fatalf("snap len: want 1, got %d", len(snap))
	}
	snap[0] = "MUTATED"

	again := fr.Texts()
	if again[0] != "one" {
		t.Fatalf("internal state mutated through returned slice: got %q", again[0])
	}
}

// TestFakeRenderConfigurableStatusAndBody verifies SetResponse changes
// what subsequent requests receive.
func TestFakeRenderConfigurableStatusAndBody(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()

	fr.SetResponse(http.StatusServiceUnavailable, []byte("render down"))

	resp, err := http.Get(fr.URL() + "/render?text=x")
	if err != nil {
		t.Fatalf("GET /render: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "render down" {
		t.Fatalf("body: want %q, got %q", "render down", string(body))
	}
}

// TestFakeRenderNonRenderPathReturns404 keeps the helper scoped to the
// /render endpoint matched by the application's default render URL
// shape.
func TestFakeRenderNonRenderPathReturns404(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()

	resp, err := http.Get(fr.URL() + "/other")
	if err != nil {
		t.Fatalf("GET /other: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-/render path: want 404, got %d", resp.StatusCode)
	}
	if got := fr.Texts(); len(got) != 0 {
		t.Fatalf("non-/render path should not record text, got %v", got)
	}
}

// TestFakeRenderConcurrentRequests exercises the helper under
// concurrent load so go test -race can flag any unsynchronized access
// to recorded state.
func TestFakeRenderConcurrentRequests(t *testing.T) {
	fr := newFakeRender()
	defer fr.Close()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(fr.URL() + "/render?text=t")
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	if got := len(fr.Texts()); got != n {
		t.Fatalf("recorded count: want %d, got %d", n, got)
	}
}
