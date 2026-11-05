package upstream

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// chunkSize is the copy buffer per in-flight stream: large enough to keep
// syscalls cheap, small enough that a token reaches the client as soon as the
// upstream wrote it.
const chunkSize = 32 << 10

// Relay copies resp.Body to w until EOF, flushing after every chunk when flush
// is set.
//
// It aborts when the upstream goes silent for stream_idle_timeout, or when the
// client stops reading for stream_write_timeout: either way the request is
// already dead, and holding the upstream connection open only costs money.
// resp.Body is left open for the caller's defer.
func (c *Client) Relay(w http.ResponseWriter, resp *http.Response, flush bool) (int64, error) {
	cfg := c.Options()
	rc := http.NewResponseController(w)

	// A write deadline left behind would poison a keep-alive connection reused
	// by a later request, so clear it on the way out.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	// Closing the body is what unblocks a read stuck on a silent upstream.
	// Cancelling the request context would also work, but the body is the thing
	// that is actually blocked.
	var stalled atomic.Bool
	pulse := func() {}
	if idle := cfg.StreamIdleTimeout.Std(); idle > 0 {
		timer := time.AfterFunc(idle, func() {
			stalled.Store(true)
			resp.Body.Close()
		})
		defer timer.Stop()
		// Only this goroutine resets the timer and the callback never touches
		// it, so there is no concurrent Reset. A callback that already fired
		// only sets a flag and closes the body, both idempotent.
		pulse = func() { timer.Reset(idle) }
	}

	writeTimeout := cfg.StreamWriteTimeout.Std()
	buf := make([]byte, chunkSize)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			pulse()

			if writeTimeout > 0 {
				// The deadline is what stops a client that opened a stream and
				// then stopped reading from pinning this goroutine forever.
				_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			}
			written, werr := w.Write(buf[:n])
			total += int64(written)
			if werr != nil {
				return total, fmt.Errorf("%w: %v", ErrWriteTimeout, werr)
			}
			if flush {
				if ferr := rc.Flush(); ferr != nil {
					return total, fmt.Errorf("%w: %v", ErrWriteTimeout, ferr)
				}
			}
		}
		if rerr != nil {
			switch {
			case stalled.Load():
				return total, ErrIdle
			case errors.Is(rerr, io.EOF):
				return total, nil
			default:
				return total, rerr
			}
		}
	}
}
