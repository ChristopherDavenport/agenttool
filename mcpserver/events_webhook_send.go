package mcpserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The categories of a failed delivery, which are all a client is told:
// the SEP's lastError and the reason of a callback endpoint error. They
// never carry what the endpoint said, so a callback cannot be used to
// read what the server can reach.
const (
	failConnection = "connection_refused"
	failTimeout    = "timeout"
	failTLS        = "tls_error"
	fail4xx        = "http_4xx"
	fail5xx        = "http_5xx"
	failChallenge  = "challenge_failed"
)

const (
	defaultSendTimeout = 5 * time.Second
	maxResponseBytes   = 64 << 10
	maxURLLength       = 2048
)

// webhookRequest is one POST to a callback.
type webhookRequest struct {
	URL    string
	Header http.Header
	Body   []byte
	// Trusted says the URL's origin is one the embedder listed, so the
	// scheme and address rules do not apply to it.
	Trusted bool
}

// webhookResponse is what the callback answered: its status and the
// start of its body, at most 64 KiB.
type webhookResponse struct {
	Status int
	Body   []byte
}

// sendError is a failed attempt at the transport level, with one of the
// categories above. The cause stays in the server's logs.
type sendError struct {
	Category string
	Err      error
}

func (e *sendError) Error() string { return "mcpserver: webhook: " + e.Category + ": " + e.Err.Error() }
func (e *sendError) Unwrap() error { return e.Err }

// webhookSender makes the POST of a delivery. The default is a [guard];
// a test replaces it.
type webhookSender func(ctx context.Context, r webhookRequest) (webhookResponse, error)

// guard is the hardened HTTP client of a webhook: it resolves the
// callback's host at every send, refuses a host with any address in a
// special-purpose block, and dials one of the addresses it checked, with
// the host name still the one the TLS handshake and the Host header
// carry. It follows no redirect, takes no proxy and keeps no connection,
// so no pooled socket outlives its check. A trusted origin skips the
// scheme and address rules.
type guard struct {
	resolve   func(ctx context.Context, host string) ([]netip.Addr, error)
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	tls       *tls.Config
	mu        sync.RWMutex
	trusted   map[string]bool
	timeout   time.Duration
	maxAddrs  int
	userAgent string
}

func newGuard(o *WebhookOptions) (*guard, error) {
	g := &guard{
		resolve: o.Resolver, tls: o.TLSConfig, trusted: map[string]bool{}, timeout: o.Timeout,
		maxAddrs: 4, userAgent: "mcpserver-events",
	}
	if g.resolve == nil {
		g.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if g.timeout <= 0 {
		g.timeout = defaultSendTimeout
	}
	for _, raw := range o.TrustedOrigins {
		if err := g.trust(raw); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// trust adds the origin raw, "scheme://host[:port]", to those whose
// callbacks skip the scheme and address rules.
func (g *guard) trust(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" {
		return fmt.Errorf("mcpserver: events: trusted origin %q is not scheme://host[:port]", raw)
	}
	g.mu.Lock()
	g.trusted[originOf(u)] = true
	g.mu.Unlock()
	return nil
}

// originOf is the scheme://host:port of u, host in lower case, the port
// filled in.
func originOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (g *guard) isTrusted(u *url.URL) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.trusted[originOf(u)]
}

// checkURL is the rules of a callback URL that need no lookup: at most
// 2048 bytes, absolute, https (http for a trusted origin), a host, no
// user info, and a host that is not a special-purpose address literal.
// It is the subscribe-time check; send applies the same rules, and the
// address check on every name, at delivery.
func (g *guard) checkURL(raw string) (*url.URL, error) {
	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("delivery.url is longer than %d bytes", maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return nil, errors.New("delivery.url must be an absolute URL")
	}
	trusted := g.isTrusted(u)
	if u.Scheme != "https" && (u.Scheme != "http" || !trusted) {
		return nil, errors.New("delivery.url must use https")
	}
	if u.User != nil {
		return nil, errors.New("delivery.url must not carry user info")
	}
	if u.Hostname() == "" {
		return nil, errors.New("delivery.url needs a host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, errors.New("delivery.url has a bad port")
		}
	}
	if !trusted {
		if a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(u.Hostname(), "["), "]")); err == nil && isSpecialAddr(a) {
			return nil, errors.New("delivery.url is not a globally routable address")
		}
	}
	return u, nil
}

// addresses resolves u's host and returns the addresses to dial. An
// untrusted host is refused when any address is special, or when none
// resolves: a host with one public and one private address is the shape
// of a rebinding attack, and an all-or-nothing rule has no race.
func (g *guard) addresses(ctx context.Context, u *url.URL, trusted bool) ([]netip.Addr, error) {
	host := u.Hostname()
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		rctx, cancel := context.WithTimeout(ctx, g.timeout)
		defer cancel()
		addrs, err = g.resolve(rctx, host)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, &sendError{Category: failTimeout, Err: err}
			}
			return nil, &sendError{Category: failConnection, Err: err}
		}
	}
	if len(addrs) == 0 {
		return nil, &sendError{Category: failConnection, Err: errors.New("the host has no address")}
	}
	if !trusted {
		for _, a := range addrs {
			if isSpecialAddr(a) {
				return nil, &sendError{Category: failConnection, Err: fmt.Errorf("the host resolves to the special-purpose address %s", a.WithZone(""))}
			}
		}
	}
	if len(addrs) > g.maxAddrs {
		addrs = addrs[:g.maxAddrs]
	}
	return addrs, nil
}

// control re-checks the address a socket is about to connect to, which
// is the last line: whatever resolved it, an untrusted socket does not
// connect to a special-purpose address.
func control(trusted bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if trusted {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("mcpserver: webhook: dial address %q: %w", address, err)
		}
		a, err := netip.ParseAddr(host)
		if err != nil || isSpecialAddr(a) {
			return fmt.Errorf("mcpserver: webhook: dial to %q refused", address)
		}
		return nil
	}
}

func (g *guard) send(ctx context.Context, r webhookRequest) (webhookResponse, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return webhookResponse{}, &sendError{Category: failConnection, Err: err}
	}
	trusted := g.isTrusted(u)
	if _, err := g.checkURL(r.URL); err != nil {
		return webhookResponse{}, &sendError{Category: failConnection, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	addrs, err := g.addresses(ctx, u, trusted)
	if err != nil {
		return webhookResponse{}, err
	}

	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	dialer := &net.Dialer{Control: control(trusted)}
	dial := g.dial
	if dial == nil {
		dial = dialer.DialContext
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// The address the transport asks for is the name; the
			// addresses checked above are the ones dialed.
			var last error
			for _, a := range addrs {
				c, err := dial(ctx, network, net.JoinHostPort(a.WithZone("").String(), port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		TLSClientConfig:        g.tls.Clone(),
		TLSHandshakeTimeout:    g.timeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(r.Body))
	if err != nil {
		return webhookResponse{}, &sendError{Category: failConnection, Err: err}
	}
	req.Header = r.Header.Clone()
	req.Header.Set("User-Agent", g.userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return webhookResponse{}, &sendError{Category: classify(ctx, err), Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil && ctx.Err() != nil {
		return webhookResponse{}, &sendError{Category: failTimeout, Err: err}
	}
	return webhookResponse{Status: resp.StatusCode, Body: body}, nil
}

// classify names the category of a transport error.
func classify(ctx context.Context, err error) string {
	var (
		cv  *tls.CertificateVerificationError
		rh  tls.RecordHeaderError
		al  tls.AlertError
		ua  x509.UnknownAuthorityError
		hn  x509.HostnameError
		ci  x509.CertificateInvalidError
		ne  net.Error
		msg = err.Error()
	)
	switch {
	case errors.As(err, &cv), errors.As(err, &rh), errors.As(err, &al), errors.As(err, &ua), errors.As(err, &hn), errors.As(err, &ci),
		strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return failTLS
	case errors.Is(err, context.DeadlineExceeded), ctx.Err() == context.DeadlineExceeded, errors.As(err, &ne) && ne.Timeout():
		return failTimeout
	}
	return failConnection
}

// statusCategory is the failure category of a status that is not 2xx.
func statusCategory(status int) string {
	if status >= 500 {
		return fail5xx
	}
	return fail4xx
}
