package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
)

// png1x1 is a minimal, valid 1x1 PNG used as the default success body
// for the fake render service. It is a real PNG decodable with
// image/png so PNG-validity assertions in checks have something
// meaningful to verify.
var png1x1 = []byte{
	// PNG signature.
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	// IHDR chunk: 1x1, 8-bit depth, RGBA color type.
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89,
	// IDAT chunk: single transparent pixel.
	0x00, 0x00, 0x00, 0x11, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x62, 0x62, 0x60, 0x60, 0x60,
	0x00, 0x04, 0x00, 0x00, 0xff, 0xff, 0x00, 0x0f,
	0x00, 0x03, 0xfe, 0x8f, 0xeb, 0xcf,
	// IEND chunk.
	0x00, 0x00,
	0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42,
	0x60, 0x82,
}

// fakeRender is an in-test stand-in for the external render service.
// It serves GET /render?text=... so the application under test can
// point RENDER_SERVICE_URL at it. Recorded text values, response
// status, and response body are all safe for concurrent access.
//
// Default response is HTTP 200 with Content-Type image/png and the
// png1x1 fixture as the body. Call SetResponse to simulate failures
// such as 503 Service Unavailable.
type fakeRender struct {
	server *httptest.Server

	mu     sync.Mutex
	texts  []string
	status int
	body   []byte
}

// newFakeRender starts an httptest.Server preconfigured to serve the
// default PNG fixture. Callers MUST call Close to release the listener.
func newFakeRender() *fakeRender {
	fr := &fakeRender{
		status: http.StatusOK,
		body:   png1x1,
	}
	fr.server = httptest.NewServer(http.HandlerFunc(fr.handle))
	return fr
}

func (f *fakeRender) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/render" {
		http.NotFound(w, r)
		return
	}
	text := r.URL.Query().Get("text")

	f.mu.Lock()
	f.texts = append(f.texts, text)
	status := f.status
	body := f.body
	f.mu.Unlock()

	if status == http.StatusOK {
		w.Header().Set("Content-Type", "image/png")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// URL returns the base URL of the fake server (no trailing slash).
// The application's render call appends /render?text=... so callers
// typically pass URL() directly as RENDER_SERVICE_URL.
func (f *fakeRender) URL() string {
	return f.server.URL
}

// Close shuts the underlying httptest.Server down.
func (f *fakeRender) Close() {
	f.server.Close()
}

// Texts returns a copy of the recorded text query parameters in the
// order they were received.
func (f *fakeRender) Texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.texts))
	copy(out, f.texts)
	return out
}

// SetResponse configures the status code and body returned by
// subsequent requests. When status is 200, the Content-Type
// image/png header is set automatically; for other statuses, no
// Content-Type is set, leaving Go's default content-sniffing
// behaviour in place.
func (f *fakeRender) SetResponse(status int, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
	f.body = body
}
