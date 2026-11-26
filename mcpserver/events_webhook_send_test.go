package mcpserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// canary is a listener that counts the connections made to it: a
// refusal that was not made by the address check shows as a connection.
type canary struct {
	ln    net.Listener
	conns atomic.Int64
}

func newCanary(t *testing.T) *canary {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener: " + err.Error())
	}
	c := &canary{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.conns.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

func (c *canary) port() string { _, p, _ := net.SplitHostPort(c.ln.Addr().String()); return p }

func guardFor(t *testing.T, o WebhookOptions) *guard {
	t.Helper()
	g, err := newGuard(&o)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func category(err error) string {
	var se *sendError
	if errors.As(err, &se) {
		return se.Category
	}
	if err == nil {
		return "<nil>"
	}
	return "<other: " + err.Error() + ">"
}

func post(url string) webhookRequest {
	return webhookRequest{URL: url, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{}`)}
}

func fixed(addrs ...string) func(context.Context, string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, a := range addrs {
		out = append(out, netip.MustParseAddr(a))
	}
	return func(context.Context, string) ([]netip.Addr, error) { return out, nil }
}

// TestEventsWebhookCheckURL holds events:webhook:subscribe-url-https-required,
// events:webhook:subscribe-url-non-https-rejected and
// events:webhook:ssrf-validate-callback-url: the rules of a callback URL
// that need no lookup.
func TestEventsWebhookCheckURL(t *testing.T) {
	g := guardFor(t, WebhookOptions{TrustedOrigins: []string{"http://localhost:8080", "https://Hooks.Internal", "http://[::1]:9000"}})
	tests := []struct {
		url string
		ok  bool
	}{
		{"https://hooks.example.com/x", true},
		{"https://hooks.example.com:8443/x?a=b#f", true},
		{"https://8.8.8.8/x", true},
		{"https://[2606:4700:4700::1111]:443/x", true},
		{"HTTPS://HOOKS.EXAMPLE.COM/x", true},
		{"http://hooks.example.com/x", false},
		{"http://localhost:8081/x", false},
		{"http://localhost:8080/x", true},
		{"http://LOCALHOST:8080/x", true},
		{"https://localhost:8080/x", true}, // a name: the address check is at delivery, and this origin is not trusted
		{"https://hooks.internal/x", true},
		{"https://hooks.internal:444/x", true}, // not trusted, a host name that is public as far as the URL says
		{"http://hooks.internal/x", false},
		{"http://[::1]:9000/x", true},
		{"https://[::1]:9000/x", false},
		{"https://127.0.0.1/x", false},
		{"https://127.1/x", true}, // not an address literal to Go; the name lookup is the check at delivery
		{"https://0x7f.0.0.1/x", true},
		{"https://[::]/x", false},
		{"https://[::ffff:7f00:1]/x", false},
		{"https://[64:ff9b::7f00:1]/x", false},
		{"https://user:pw@hooks.example.com/", false},
		{"https://@hooks.example.com/", false},
		{"https://hooks.example.com:0/", false},
		{"https://hooks.example.com:65536/", false},
		{"https://hooks.example.com:abc/", false},
		{"https:///path", false},
		{"https://", false},
		{"hooks.example.com", false},
		{"//hooks.example.com/x", false},
		{"file:///etc/passwd", false},
		{"gopher://hooks.example.com/", false},
		{"https:hooks.example.com", false},
		{"", false},
		{"https://hooks.example.com/" + strings.Repeat("a", maxURLLength), false},
		{"https://exa mple.com/", false},
	}
	for _, tc := range tests {
		_, err := g.checkURL(tc.url)
		if (err == nil) != tc.ok {
			t.Errorf("checkURL(%q) = %v, ok %v", shorten(tc.url), err, tc.ok)
		}
	}
	for _, bad := range []string{"example.com", "ftp://x", "https://x/path", "https://u@x", "https://x?q=1", "", "https://"} {
		if _, err := newGuard(&WebhookOptions{TrustedOrigins: []string{bad}}); err == nil {
			t.Errorf("trusted origin %q was accepted", bad)
		}
	}
}

func shorten(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

// TestEventsWebhookGuardRefuses holds events:webhook:ssrf-reject-non-routable:
// a callback whose host resolves to any special-purpose address is not
// connected to, whatever else it resolves to.
func TestEventsWebhookGuardRefuses(t *testing.T) {
	c := newCanary(t)
	tests := []struct {
		name    string
		url     string
		resolve func(context.Context, string) ([]netip.Addr, error)
	}{
		{"literal", "https://127.0.0.1:" + c.port() + "/x", nil},
		{"name to loopback", "https://evil.example.com:" + c.port() + "/x", fixed("127.0.0.1")},
		{"name to private", "https://evil.example.com/x", fixed("10.0.0.5")},
		{"name to metadata", "https://evil.example.com/x", fixed("169.254.169.254")},
		{"name to ipv6 loopback", "https://evil.example.com/x", fixed("::1")},
		{"name to mapped loopback", "https://evil.example.com/x", fixed("::ffff:127.0.0.1")},
		{"name to unique local", "https://evil.example.com/x", fixed("fd00::1")},
		{"public then private", "https://evil.example.com/x", fixed("93.184.216.34", "10.0.0.5")},
		{"private then public", "https://evil.example.com/x", fixed("10.0.0.5", "93.184.216.34")},
		{"public and link local v6", "https://evil.example.com/x", fixed("93.184.216.34", "fe80::1")},
		{"no address", "https://evil.example.com/x", fixed()},
		{"lookup fails", "https://evil.example.com/x", func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("nxdomain") }},
		{"http", "http://93.184.216.34/x", nil},
		{"userinfo", "https://u:p@93.184.216.34/x", nil},
	}
	for _, tc := range tests {
		g := guardFor(t, WebhookOptions{Resolver: tc.resolve})
		g.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
			t.Errorf("%s: dialed %s", tc.name, addr)
			return nil, errors.New("dialed")
		}
		_, err := g.send(context.Background(), post(tc.url))
		if category(err) != failConnection {
			t.Errorf("%s: %v, want connection_refused", tc.name, err)
		}
	}
	// And with the real dialer, nothing reaches the canary.
	for _, tc := range tests {
		g := guardFor(t, WebhookOptions{Resolver: tc.resolve, Timeout: time.Second})
		_, _ = g.send(context.Background(), post(tc.url))
	}
	time.Sleep(50 * time.Millisecond)
	if n := c.conns.Load(); n != 0 {
		t.Errorf("%d connections reached the canary", n)
	}
}

// TestEventsWebhookGuardPinsAndResolvesEverySend holds
// events:webhook:ssrf-validate-at-delivery-time: the host is resolved at
// every send and the connection goes to the address that was checked,
// so a name that answers public and then private is refused the second
// time, and never dialed by name.
func TestEventsWebhookGuardPinsAndResolvesEverySend(t *testing.T) {
	var mu sync.Mutex
	answers := [][]netip.Addr{{netip.MustParseAddr("93.184.216.34")}, {netip.MustParseAddr("127.0.0.1")}, {netip.MustParseAddr("2606:2800:220:1::1"), netip.MustParseAddr("93.184.216.34")}}
	lookups := 0
	var dialed []string
	g := guardFor(t, WebhookOptions{Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		if host != "rebind.example.com" {
			t.Errorf("resolved %q", host)
		}
		a := answers[min(lookups, len(answers)-1)]
		lookups++
		return a, nil
	}})
	g.dial = func(_ context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, network+" "+addr)
		mu.Unlock()
		return nil, errors.New("no route")
	}
	for i := 0; i < 3; i++ {
		_, err := g.send(context.Background(), post("https://rebind.example.com:8443/x"))
		if category(err) != failConnection {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if lookups != 3 {
		t.Errorf("%d lookups for 3 sends", lookups)
	}
	want := []string{"tcp 93.184.216.34:8443", "tcp [2606:2800:220:1::1]:8443", "tcp 93.184.216.34:8443"}
	if fmt.Sprint(dialed) != fmt.Sprint(want) {
		t.Errorf("dialed %q, want %q: the second answer was private and never dialed, the third tried its addresses in order", dialed, want)
	}
}

// TestEventsWebhookControl holds events:webhook:ssrf-reject-non-routable
// at the socket: whatever resolved the address, an untrusted dialer
// refuses a special one.
func TestEventsWebhookControl(t *testing.T) {
	c := newCanary(t)
	d := &net.Dialer{Control: control(false)}
	for _, addr := range []string{"127.0.0.1:" + c.port(), "[::1]:9", "10.0.0.1:80", "[::ffff:127.0.0.1]:" + c.port(), "169.254.169.254:80", "0.0.0.0:80"} {
		conn, err := d.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			conn.Close()
			t.Errorf("dialed %s", addr)
		}
	}
	if n := c.conns.Load(); n != 0 {
		t.Errorf("%d connections", n)
	}
	for _, bad := range []string{"no-port", "example.com:80", ""} {
		if err := control(false)("tcp", bad, nil); err == nil {
			t.Errorf("control accepted %q", bad)
		}
	}
	// A trusted origin may reach loopback, which is what it is for.
	conn, err := (&net.Dialer{Control: control(true)}).DialContext(context.Background(), "tcp", "127.0.0.1:"+c.port())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

// tlsServer starts an HTTPS server on loopback whose certificate is for
// example.com, and returns it with the TLS configuration that trusts it.
func tlsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *tls.Config, string) {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.EnableHTTP2 = false
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	return s, &tls.Config{RootCAs: pool}, port
}

// TestEventsWebhookGuardSends holds events:webhook:ssrf-no-redirects,
// events:webhook:delivery-post-json and go:events:webhook:send-limits:
// the hardened client delivers to a trusted origin as a POST with the
// name still the Host and the SNI, follows no redirect, takes no proxy,
// and bounds the time and the response it reads.
func TestEventsWebhookGuardSends(t *testing.T) {
	type seen struct {
		method, host, sni, ctype, ua, body string
	}
	var got atomic.Pointer[seen]
	var hits atomic.Int64
	srv, cfg, port := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		got.Store(&seen{r.Method, r.Host, r.TLS.ServerName, r.Header.Get("Content-Type"), r.Header.Get("User-Agent"), string(b)})
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://elsewhere.example.com/", http.StatusFound)
		case "/big":
			w.Write([]byte(strings.Repeat("x", 1<<20)))
		case "/slow":
			time.Sleep(400 * time.Millisecond)
		case "/stall":
			w.Header().Set("Content-Length", "100")
			w.Write([]byte("start"))
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
		case "/teapot":
			w.WriteHeader(418)
			w.Write([]byte("short and stout"))
		}
	})
	origin := "https://example.com:" + port
	proxy := newCanary(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:"+proxy.port())
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:"+proxy.port())
	t.Setenv("NO_PROXY", "")
	var resolved atomic.Value
	g := guardFor(t, WebhookOptions{
		TrustedOrigins: []string{origin}, TLSConfig: cfg, Timeout: 200 * time.Millisecond,
		Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			resolved.Store(host)
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	})

	resp, err := g.send(context.Background(), post(origin+"/ok"))
	if err != nil || resp.Status != 200 {
		t.Fatalf("send = %+v, %v", resp, err)
	}
	if s := got.Load(); s.method != "POST" || s.host != "example.com:"+port || s.sni != "example.com" || s.ctype != "application/json" || s.body != "{}" || s.ua == "" {
		t.Errorf("the server saw %+v", s)
	}
	if resolved.Load() != "example.com" {
		t.Errorf("resolved %v", resolved.Load())
	}
	if n := proxy.conns.Load(); n != 0 {
		t.Errorf("%d connections went to the environment's proxy", n)
	}

	before := hits.Load()
	resp, err = g.send(context.Background(), post(origin+"/redirect"))
	if err != nil || resp.Status != http.StatusFound || hits.Load() != before+1 {
		t.Errorf("redirect: %+v, %v, %d hits: a redirect is a response, not followed", resp, err, hits.Load()-before)
	}
	resp, err = g.send(context.Background(), post(origin+"/teapot"))
	if err != nil || resp.Status != 418 || string(resp.Body) != "short and stout" {
		t.Errorf("teapot: %+v, %v", resp, err)
	}
	big := guardFor(t, WebhookOptions{TrustedOrigins: []string{origin}, TLSConfig: cfg, Resolver: fixed("127.0.0.1")})
	resp, err = big.send(context.Background(), post(origin+"/big"))
	if err != nil || resp.Status != 200 || len(resp.Body) != maxResponseBytes {
		t.Errorf("a megabyte answer: %d bytes, %v, want %d", len(resp.Body), err, maxResponseBytes)
	}
	start := time.Now()
	_, err = g.send(context.Background(), post(origin+"/slow"))
	if category(err) != failTimeout || time.Since(start) > 350*time.Millisecond {
		t.Errorf("slow: %v after %v", err, time.Since(start))
	}
	start = time.Now()
	_, err = g.send(context.Background(), post(origin+"/stall"))
	if category(err) != failTimeout || time.Since(start) > 350*time.Millisecond {
		t.Errorf("a body that stalls: %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = g.send(ctx, post(origin+"/ok")); err == nil {
		t.Error("a cancelled context sent")
	}

	// Without the origin trusted, the same server is refused, since it is
	// on loopback.
	before = hits.Load()
	strict := guardFor(t, WebhookOptions{TLSConfig: cfg, Resolver: fixed("127.0.0.1")})
	if _, err = strict.send(context.Background(), post(origin+"/ok")); category(err) != failConnection || hits.Load() != before {
		t.Errorf("untrusted loopback: %v, %d hits", err, hits.Load()-before)
	}
	_ = srv
}

// TestEventsWebhookGuardTLSAndRefusal covers the transport categories.
func TestEventsWebhookGuardTLSAndRefusal(t *testing.T) {
	_, cfg, port := tlsServer(t, func(http.ResponseWriter, *http.Request) {})
	tests := []struct {
		name string
		g    *guard
		url  string
		want string
	}{
		{"unknown authority", guardFor(t, WebhookOptions{TrustedOrigins: []string{"https://example.com:" + port}, Resolver: fixed("127.0.0.1")}), "https://example.com:" + port, failTLS},
		{"wrong name", guardFor(t, WebhookOptions{TrustedOrigins: []string{"https://other.example.org:" + port}, TLSConfig: cfg, Resolver: fixed("127.0.0.1")}), "https://other.example.org:" + port, failTLS},
		{"not tls at all", guardFor(t, WebhookOptions{TrustedOrigins: []string{"https://example.com:1"}, Resolver: fixed("127.0.0.1")}), "https://example.com:1", failConnection},
	}
	plain := newCanary(t)
	tests = append(tests, struct {
		name string
		g    *guard
		url  string
		want string
	}{"a server that hangs up on the handshake", guardFor(t, WebhookOptions{TrustedOrigins: []string{"https://example.com:" + plain.port()}, Resolver: fixed("127.0.0.1")}), "https://example.com:" + plain.port(), failConnection})
	for _, tc := range tests {
		_, err := tc.g.send(context.Background(), post(tc.url))
		if category(err) != tc.want {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.want)
		}
	}
	for _, e := range []string{"x509: certificate signed by unknown authority", "remote error: tls: bad certificate"} {
		if classify(context.Background(), errors.New(e)) != failTLS {
			t.Errorf("%q is not a tls error", e)
		}
	}
	if classify(context.Background(), fmt.Errorf("dial: %w", context.DeadlineExceeded)) != failTimeout {
		t.Error("a deadline is not a timeout")
	}
	if statusCategory(404) != fail4xx || statusCategory(399) != fail4xx || statusCategory(500) != fail5xx {
		t.Error("status categories")
	}
}

// TestEventsWebhookRebindingEndToEnd holds
// events:webhook:ssrf-validate-at-delivery-time and
// events:webhook:verification-uses-ssrf-hardened-path through a server:
// a callback whose name answers public at subscribe time is verified and
// delivered to at the pinned address, and when the name answers private,
// the next delivery is refused without a connection.
func TestEventsWebhookRebindingEndToEnd(t *testing.T) {
	var hits atomic.Int64
	srv, cfg, port := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var b struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &b)
		if b.Type == "verification" {
			fmt.Fprintf(w, `{"challenge":%q}`, b.Challenge)
		}
	})
	var mu sync.Mutex
	answer := netip.MustParseAddr("93.184.216.34")
	var dialed []string
	timers := &fakeTimers{clock: newClock()}
	h := newEventsHarnessWithClock(t, timers.clock, func(o *EventsOptions) {
		o.Webhook = &WebhookOptions{
			Principal: func(context.Context, *sdk.RequestExtra, *sdk.ServerSession) string { return "alice" },
			TLSConfig: cfg,
			AfterFunc: timers.after,
			Resolver: func(context.Context, string) ([]netip.Addr, error) {
				mu.Lock()
				defer mu.Unlock()
				return []netip.Addr{answer}, nil
			},
		}
	})
	t.Cleanup(h.events.Close)
	h.events.hooks.guard.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		// The public address stands in for the real server.
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	et := h.emitter("count", nil)
	url := "https://example.com:" + port + "/hook"
	raw, err := h.call(EventsSubscribeMethod, subParams("count", url, secretA, ""))
	if err != nil {
		t.Fatalf("subscribe: %v %s", err, raw)
	}
	emit(t, et, "e1", 1)
	waitFor(t, func() bool { return hits.Load() == 2 })
	waitFor(t, h.events.hooks.idleForTest)
	mu.Lock()
	if len(dialed) != 2 || dialed[0] != "93.184.216.34:"+port || dialed[1] != dialed[0] {
		t.Errorf("dialed %v: the verification and the delivery go to the checked address", dialed)
	}
	answer = netip.MustParseAddr("10.0.0.5")
	mu.Unlock()
	emit(t, et, "e2", 2)
	waitFor(t, h.events.hooks.idleForTest)
	if hits.Load() != 2 {
		t.Errorf("%d requests reached the server, the rebound one among them", hits.Load())
	}
	mu.Lock()
	n := len(dialed)
	mu.Unlock()
	if n != 2 {
		t.Errorf("%d dials; the private answer was dialed", n)
	}
	h.events.hooks.mu.Lock()
	var status *deliveryStatus
	for tr := range h.events.hooks.all {
		status = tr.status()
	}
	h.events.hooks.mu.Unlock()
	if status == nil || status.LastError == nil || *status.LastError != failConnection {
		t.Errorf("status %+v", status)
	}

	// A name that is private at subscribe time is refused at verification.
	h2 := newEventsHarness(t, func(o *EventsOptions) {
		o.Webhook = &WebhookOptions{
			Principal: func(context.Context, *sdk.RequestExtra, *sdk.ServerSession) string { return "alice" },
			Resolver:  fixed("127.0.0.1"),
		}
	})
	t.Cleanup(h2.events.Close)
	h2.emitter("count", nil)
	c := newCanary(t)
	_, jerr := h2.call(EventsSubscribeMethod, subParams("count", "https://evil.example.com:"+c.port()+"/x", secretA, ""))
	if jerr == nil || jerr.Code != EventsCodeCallbackEndpoint || !strings.Contains(string(jerr.Data), failConnection) {
		t.Errorf("a name that resolves to loopback = %v", jerr)
	}
	if _, jerr = h2.call(EventsSubscribeMethod, subParams("count", "https://127.0.0.1:"+c.port()+"/x", secretA, "")); jerr == nil || jerr.Code != EventsCodeInvalidParams {
		t.Errorf("a loopback literal = %v", jerr)
	}
	time.Sleep(50 * time.Millisecond)
	if c.conns.Load() != 0 {
		t.Error("the canary was connected to")
	}
	_ = url
}

// TestEventsWebhookTrustOrigin holds go:events:webhook:trust-origin-at-runtime:
// an origin added to a running server is trusted from then on, a malformed
// one is refused, and a server without the webhook mode refuses all.
func TestEventsWebhookTrustOrigin(t *testing.T) {
	g := guardFor(t, WebhookOptions{})
	if _, err := g.checkURL("http://127.0.0.1:9/x"); err == nil {
		t.Fatal("an untrusted http callback was accepted")
	}
	if err := g.trust("http://127.0.0.1:9"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.checkURL("http://127.0.0.1:9/x"); err != nil {
		t.Fatalf("a trusted origin was refused: %v", err)
	}
	if _, err := g.checkURL("http://127.0.0.1:10/x"); err == nil {
		t.Fatal("another port was trusted")
	}
	if err := g.trust("http://127.0.0.1:9/path"); err == nil {
		t.Fatal("an origin with a path was accepted")
	}

	h := newWebhookHarness(t, nil)
	if err := h.events.trustWebhookOrigin("http://127.0.0.1:12345"); err != nil {
		t.Fatal(err)
	}
	if err := (&Events{}).trustWebhookOrigin("http://127.0.0.1:1"); err == nil {
		t.Fatal("a server without webhooks trusted an origin")
	}
}
