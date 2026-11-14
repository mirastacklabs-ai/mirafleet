package osb

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTimedFront starts a front with short timeouts over a trivial upstream.
func newTimedFront(t *testing.T, handshake, body time.Duration) (addr string, client *http.Client) {
	t.Helper()
	return newTimedFrontUp(t, handshake, body, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	}))
}

// newTimedFrontUp is newTimedFront over a caller-supplied upstream handler.
func newTimedFrontUp(t *testing.T, handshake, body time.Duration, h http.Handler) (addr string, client *http.Client) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := InitPKI(dir, 30, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	f, err := NewFront(FrontConfig{Listen: "127.0.0.1:0", Upstream: up.Listener.Addr().String(),
		CertFile: p.FrontCert, KeyFile: p.FrontKey, CAFile: p.CAFile})
	if err != nil {
		t.Fatal(err)
	}
	f.handshakeTimeout, f.bodyTimeout = handshake, body
	if addr, err = f.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	client, err = NewHTTPClient(Config{BaseURL: "https://" + addr, CAFile: p.CAFile,
		PinSHA256: p.FrontPinSHA, CertFile: p.ClientCert, KeyFile: p.ClientKey})
	if err != nil {
		t.Fatal(err)
	}
	return addr, client
}

func TestFrontHandshakeTimeout(t *testing.T) {
	addr, _ := newTimedFront(t, 300*time.Millisecond, time.Minute)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = c.Read(make([]byte, 1)) // a silent client must be dropped by the server
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("idle pre-handshake connection was kept: err=%v after %s", err, time.Since(start))
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the server did not close the connection: %v", err)
	}
}

func TestFrontSlowBodyGets408(t *testing.T) {
	addr, client := newTimedFront(t, 10*time.Second, 300*time.Millisecond)
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { pw.Write([]byte("abc")) }() // then stalls
	req, _ := http.NewRequest("POST", "https://"+addr+"/v1/sandboxes", pr)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("no response (bare EOF?): %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(b), "timeout") {
		t.Fatalf("got %d %q, want 408", resp.StatusCode, b)
	}
}

func TestFrontLongResponseNotCut(t *testing.T) {
	// The body deadline must not apply after the body ended.
	addr, client := newTimedFront(t, 10*time.Second, 200*time.Millisecond)
	req, _ := http.NewRequest("POST", "https://"+addr+"/v1/sandboxes", strings.NewReader("hello"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "ok" {
		t.Fatalf("%q", b)
	}
}

// TestFrontStreamingResponseAfterBodyNotCut: a POST with a body answered by an SSE stream that
// lasts several times the body deadline must arrive whole (the deadline ends with the body).
func TestFrontStreamingResponseAfterBodyNotCut(t *testing.T) {
	const events = 8
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < events; i++ {
			io.WriteString(w, "data: tick\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	addr, client := newTimedFrontUp(t, 10*time.Second, 200*time.Millisecond, up)
	start := time.Now()
	req, _ := http.NewRequest("POST", "https://"+addr+"/v1/sandboxes", strings.NewReader(`{"q":1}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream cut after %s: %v", time.Since(start), err)
	}
	if n := strings.Count(string(b), "data: tick"); n != events {
		t.Fatalf("got %d of %d events after %s", n, events, time.Since(start))
	}
	if time.Since(start) < 600*time.Millisecond {
		t.Fatalf("stream finished in %s, shorter than the body deadline test needs", time.Since(start))
	}
}
