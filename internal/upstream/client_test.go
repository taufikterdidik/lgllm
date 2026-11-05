package upstream

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lgllm/internal/config"
)

// testConfig builds an upstream config with short timeouts so tests stay fast.
func testConfig(mutate func(*config.Upstream)) config.Upstream {
	cfg := config.Default().Upstream
	cfg.ConnectTimeout = config.Duration(2 * time.Second)
	cfg.ResponseHeaderTimeout = config.Duration(2 * time.Second)
	cfg.StreamIdleTimeout = config.Duration(5 * time.Second)
	cfg.StreamWriteTimeout = config.Duration(2 * time.Second)
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func TestDoReturnsHeadersAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-x" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := New(testConfig(nil))
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer sk-x")

	resp, err := c.Do(req, 0)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

func TestDoHeaderTimeout(t *testing.T) {
	// A raw listener is used rather than httptest.Server: the point is a peer
	// that accepts the connection and never writes a byte, which a handler-based
	// server cannot easily simulate without blocking its own shutdown.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	url := "http://" + ln.Addr().String()

	c := New(testConfig(nil))
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))

	start := time.Now()
	_, err = c.Do(req, 80*time.Millisecond)
	if err == nil {
		t.Fatal("Do must fail when the upstream never sends headers")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if got := TransportErrorStatus(err); got != http.StatusGatewayTimeout {
		t.Errorf("TransportErrorStatus = %d, want 504", got)
	}
}

func TestDoLongBodyOutlivesHeaderTimeout(t *testing.T) {
	// Headers arrive at once, then the body trickles in well past the header
	// timeout. A context deadline applied to the whole request would abort this
	// healthy stream; the header-only deadline must not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			fl.Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New(testConfig(nil))
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))

	resp, err := c.Do(req, 80*time.Millisecond)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body read failed after headers arrived: %v", err)
	}
	if n := strings.Count(string(body), "data:"); n != 4 {
		t.Errorf("received %d chunks, want 4 (body %q)", n, body)
	}
}

func TestDoDoesNotFollowRedirects(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		_, _ = io.WriteString(w, "leaked")
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	c := New(testConfig(nil))
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	resp, err := c.Do(req, 0)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want the 302 itself", resp.StatusCode)
	}
	if targetHits != 0 {
		t.Fatal("client followed a redirect; the credential would follow it too")
	}
}

// countingWriter records writes and flushes so Relay behaviour is observable.
type countingWriter struct {
	mu       sync.Mutex
	buf      strings.Builder
	writes   int
	flushes  int
	writeErr error
}

func (w *countingWriter) Header() http.Header { return http.Header{} }

func (w *countingWriter) WriteHeader(int) {}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.writes++
	return w.buf.Write(p)
}

func (w *countingWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushes++
}

func (w *countingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestRelayStreamsAndFlushesEveryChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, part := range []string{"data: one\n\n", "data: two\n\n", "data: three\n\n"} {
			_, _ = io.WriteString(w, part)
			fl.Flush()
		}
	}))
	defer srv.Close()

	c := New(testConfig(nil))
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req, 0)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	w := &countingWriter{}
	n, err := c.Relay(w, resp, true)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	got := w.String()
	if !strings.Contains(got, "data: one") || !strings.Contains(got, "data: three") {
		t.Errorf("relayed body = %q", got)
	}
	if int(n) != len(got) {
		t.Errorf("byte count = %d, wrote %d", n, len(got))
	}
	if w.flushes == 0 {
		t.Error("no flush was issued; tokens would be buffered until the end")
	}
}

// blockingBody yields the first read immediately, then blocks until closed.
type blockingBody struct {
	sent   bool
	closed chan struct{}
	once   sync.Once
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, []byte("data: first\n\n")), nil
	}
	<-b.closed
	return 0, errors.New("body closed")
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestRelayAbortsOnIdleUpstream(t *testing.T) {
	cfg := testConfig(func(c *config.Upstream) {
		c.StreamIdleTimeout = config.Duration(120 * time.Millisecond)
	})
	c := New(cfg)
	body := &blockingBody{closed: make(chan struct{})}
	resp := &http.Response{StatusCode: 200, Body: body, Header: http.Header{}}

	w := &countingWriter{}
	start := time.Now()
	_, err := c.Relay(w, resp, true)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrIdle) {
		t.Fatalf("err = %v, want ErrIdle", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("idle detection took %s", elapsed)
	}
	if got := w.String(); !strings.Contains(got, "data: first") {
		t.Errorf("bytes received before the stall were dropped: %q", got)
	}
}

// timeoutError mimics the net.Error a write deadline produces.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestRelayReportsWriteTimeout(t *testing.T) {
	var _ net.Error = timeoutError{}

	c := New(testConfig(nil))
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("data: one\n\n")),
		Header:     http.Header{},
	}

	w := &countingWriter{writeErr: timeoutError{}}
	_, err := c.Relay(w, resp, true)
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("err = %v, want ErrWriteTimeout", err)
	}
}

func TestTransportErrorStatus(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{ErrTimeout, http.StatusGatewayTimeout},
		{timeoutError{}, http.StatusGatewayTimeout},
		{errors.New("connection refused"), http.StatusBadGateway},
		{&net.OpError{Op: "dial", Err: errors.New("no route")}, http.StatusBadGateway},
	}
	for _, tc := range tests {
		if got := TransportErrorStatus(tc.err); got != tc.want {
			t.Errorf("TransportErrorStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestReloadAppliesNewOptions(t *testing.T) {
	c := New(testConfig(nil))
	if got := c.Options().ResponseHeaderTimeout.Std(); got != 2*time.Second {
		t.Fatalf("initial timeout = %s", got)
	}

	c.Reload(testConfig(func(cfg *config.Upstream) {
		cfg.ResponseHeaderTimeout = config.Duration(7 * time.Second)
	}))
	if got := c.Options().ResponseHeaderTimeout.Std(); got != 7*time.Second {
		t.Errorf("timeout after reload = %s, want 7s", got)
	}
}

func TestPerProviderHeaderTimeout(t *testing.T) {
	// A provider may ask for a longer header wait than the global default. Each
	// distinct timeout gets its own client so no request races another's timer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := New(testConfig(func(cfg *config.Upstream) {
		cfg.ResponseHeaderTimeout = config.Duration(10 * time.Millisecond)
	}))

	short, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := c.Do(short, 0); err == nil {
		t.Fatal("default header timeout should have expired")
	}

	long, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(long, time.Second)
	if err != nil {
		t.Fatalf("per-provider timeout not honoured: %v", err)
	}
	resp.Body.Close()
}
