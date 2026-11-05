// Package upstream owns the outbound HTTP path to providers.
//
// One rule shapes it: a chat completion can stream for minutes, so there is no
// whole-request timeout. Only the wait for response headers is bounded. After
// that the body is paced by an idle timer, because a stream that has stopped
// producing bytes looks alive while the caller keeps paying for it.
package upstream

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"lgllm/internal/config"
)

var (
	// ErrTimeout means the upstream never got far enough to answer: the TCP
	// connect, the TLS handshake or the response headers took too long.
	ErrTimeout = errors.New("upstream timeout")
	// ErrIdle means the upstream body stopped producing bytes.
	ErrIdle = errors.New("upstream stream idle timeout")
	// ErrWriteTimeout means the client stopped reading.
	ErrWriteTimeout = errors.New("client write timeout")
)

// Client is an HTTP client tuned for proxying LLM traffic.
//
// A transport fixes its timeouts at construction, so a provider that overrides
// the response-header timeout gets its own client (and pool). Provider
// timeouts are usually uniform, so this normally means exactly one pool.
type Client struct {
	mu        sync.Mutex
	cfg       config.Upstream
	byTimeout map[time.Duration]*http.Client
}

// New builds a client from the initial configuration.
func New(cfg config.Upstream) *Client {
	c := &Client{byTimeout: make(map[time.Duration]*http.Client)}
	c.Reload(cfg)
	return c
}

// Options returns the active transport configuration.
func (c *Client) Options() config.Upstream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// Reload applies new timeout and pool settings.
//
// A transport fixes its timeouts at construction, so the whole set is rebuilt.
// Idle connections of the old clients are closed; in-flight requests keep using
// them until they finish.
func (c *Client) Reload(cfg config.Upstream) {
	c.mu.Lock()
	old := c.allLocked()
	c.cfg = cfg
	c.byTimeout = make(map[time.Duration]*http.Client)
	c.mu.Unlock()

	for _, hc := range old {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}

// Do sends req and waits for the response headers.
//
// headerTimeout <= 0 uses the configured default. The transport's
// ResponseHeaderTimeout stops at the headers, so a long stream is never cut
// short; the caller drives the body and must always close resp.Body.
//
// A timeout is wrapped in ErrTimeout so callers can answer 504 instead of
// guessing from the error string. The underlying message is kept for the log.
func (c *Client) Do(req *http.Request, headerTimeout time.Duration) (*http.Response, error) {
	if headerTimeout <= 0 {
		headerTimeout = c.Options().ResponseHeaderTimeout.Std()
	}
	resp, err := c.clientFor(headerTimeout).Do(req)
	if err != nil && isTimeout(err) {
		return nil, fmt.Errorf("%w after %s: %v", ErrTimeout, headerTimeout, err)
	}
	return resp, err
}

// isTimeout reports whether err is a deadline expiry.
func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// CloseIdleConnections releases every pooled connection, used on shutdown.
func (c *Client) CloseIdleConnections() {
	c.mu.Lock()
	all := c.allLocked()
	c.mu.Unlock()
	for _, hc := range all {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}

// allLocked returns every live client. Callers must hold c.mu.
func (c *Client) allLocked() []*http.Client {
	out := make([]*http.Client, 0, len(c.byTimeout))
	for _, hc := range c.byTimeout {
		out = append(out, hc)
	}
	return out
}

func (c *Client) clientFor(headerTimeout time.Duration) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()

	if headerTimeout <= 0 {
		headerTimeout = c.cfg.ResponseHeaderTimeout.Std()
	}
	if hc, ok := c.byTimeout[headerTimeout]; ok {
		return hc
	}
	hc := c.newClient(headerTimeout)
	c.byTimeout[headerTimeout] = hc
	return hc
}

// newClient builds a client whose transport bounds only the response-header
// wait. The outer http.Client deliberately has no Timeout: that would cap the
// whole stream.
func (c *Client) newClient(headerTimeout time.Duration) *http.Client {
	cfg := c.cfg
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,

		// One value covers TCP connect and TLS handshake: a slow handshake is
		// as useless to the caller as a refused connection.
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout.Std(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: cfg.ConnectTimeout.Std(),

		// Stops at the response headers, never touching the body, so it is safe
		// for streams that run for minutes.
		ResponseHeaderTimeout: headerTimeout,

		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,

		// HTTP/2 multiplexes concurrent streams to one host, which is what the
		// major providers prefer.
		ForceAttemptHTTP2: true,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Provider APIs do not redirect. Following one silently could move
			// the request, and its credential, to a host the config never
			// named.
			return http.ErrUseLastResponse
		},
	}
}

// TransportErrorStatus maps a transport failure to the status the caller sees.
//
// A timeout is 504 (the upstream did not answer in time); anything else is 502
// (the connection, TLS handshake or DNS lookup failed).
func TransportErrorStatus(err error) int {
	if errors.Is(err, ErrTimeout) || isTimeout(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
