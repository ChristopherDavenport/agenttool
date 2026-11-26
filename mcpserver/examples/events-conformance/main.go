// Command events-conformance serves MCP Events (SEP-3415), poll and
// webhook, over Streamable HTTP, for the server scenarios of the
// conformance suite (modelcontextprotocol/conformance#521 and #541) to
// run against. Push is not served: events/stream is answered with -32026.
//
// It serves three event types, which the scenarios discover and name
// nothing of: conformance.counter, emit-only, one tick every two
// seconds, by poll and webhook; conformance.hooks, emit-only, webhook
// only (no poll, which the poll-mode row probes); and conformance.alerts,
// emit-only, poll only, filtered by minimum severity. Every request is made by one fixed
// principal, "mcp-conformance", which the webhook mode needs and which
// stands for what an embedding application's authentication would say.
//
// # Relaxations, for this example alone
//
// A server that delivers to whatever callback a client names must refuse
// loopback and plain http, and mcpserver does. The suite's receiver
// listens on loopback over http, so this server relaxes the guard in two
// ways a deployment must not copy:
//
//   - The events_conformance_allow_callback_origin tool, which the suite
//     calls with its receiver's origin, trusts that origin with
//     the module-internal testhook.TrustWebhookOrigin, which is not public
//     API: an embedder that knows its callback origins when it builds the
//     server sets WebhookOptions.TrustedOrigins instead.
//   - The suite subscribes some callbacks to https://conformance.invalid,
//     which exists nowhere. That origin is trusted, a resolver sends it to
//     the loopback, and a stub there (-placeholder, default 127.0.0.1:443,
//     a port below 1024) answers the verification challenge and accepts
//     every delivery. The stub's certificate is made at start and is the
//     only root of the delivery client.
//
// The suite calls the tool after it has graded the SSRF rules, so those
// are graded against the guard as it ships.
//
// # Running the suite
//
// Build the referee (a checkout of conformance#541, which includes #521)
// without running its install scripts, then run each scenario against the
// server. REFEREE is that checkout:
//
//	(cd $REFEREE && npm ci --ignore-scripts && npm run build)
//	go run ./examples/events-conformance -addr 127.0.0.1:3000 &
//	for s in events-discovery events-poll events-push events-webhook events-webhook-delivery; do
//	  node $REFEREE/dist/index.js server --url http://127.0.0.1:3000/mcp \
//	    --scenario $s --spec-version 2026-07-28 --force --timeout 60000
//	done
//
// The stub wants port 443. Where that is not allowed to you, run both
// the server and the referee in a namespace of their own, where you may
// bind it and which changes nothing on the host:
//
//	unshare -Urn sh -c 'ip link set lo up; go run ./examples/events-conformance & ...'
//
// or give -placeholder another address, which has the webhook scenario's
// placeholder subscriptions fail verification.
//
// The suite's controls (events_conformance_*) served are the origin
// allowance, the catalog, subscription existence and state, and a
// subscribe on another principal's behalf, which the server sends to its
// own handler with a header only it can make, and a gap or a termination
// of one webhook subscription by id ([mcpserver.Events.GapWebhook] and
// [mcpserver.Events.TerminateWebhook], which check no principal: a tool
// that offers them to clients is for a test server alone). The ones for
// restarts are not served, since mcpserver has no way to rebuild itself,
// so the rows that need them report that they could not be tested.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/agenttool/mcpserver"
	"github.com/ChristopherDavenport/agenttool/mcpserver/internal/testhook"
)

const (
	counterType = "conformance.counter"
	alertsType  = "conformance.alerts"
	hooksType   = "conformance.hooks"

	// principal is who every request is from.
	principal = "mcp-conformance"
	// placeholderHost is the host the suite's callbacks name that
	// resolves nowhere.
	placeholderHost = "conformance.invalid"
)

type originArgs struct {
	Origin string `json:"origin" desc:"A webhook callback origin to trust, scheme://host[:port]"`
}

type catalogArgs struct {
	Op   string `json:"op" desc:"add, remove, retype or extend" enum:"add,remove,retype,extend"`
	Name string `json:"name,omitempty" desc:"A type this control added, for remove, retype and extend"`
}

type idArgs struct {
	ID string `json:"id" desc:"A webhook subscription id"`
}

type tenantArgs struct {
	Principal string `json:"principal"`
	Name      string `json:"name"`
	URL       string `json:"url"`
}

// principalHeader carries an override of the principal, with the nonce
// that only this process knows, so that nothing outside it can use one.
const principalHeader = "X-Conformance-Principal"

func main() {
	addr := flag.String("addr", "127.0.0.1:3000", "address to serve /mcp on")
	placeholder := flag.String("placeholder", "127.0.0.1:443", "address of the stub for https://"+placeholderHost+"; empty for none")
	pageSize := flag.Int("list-page-size", 2, "event types in one events/list page; the pagination row needs a second page")
	flag.Parse()
	if err := run(*addr, *placeholder, *pageSize); err != nil {
		log.Fatal(err)
	}
}

func run(addr, placeholder string, pageSize int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var ev *mcpserver.Events
	var handler http.Handler
	store := mcpserver.NewMemoryWebhookStore()
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	prefix := hex.EncodeToString(nonce[:]) + ":"
	cat := &catalog{minted: map[string]*mintedType{}}

	allow := agenttool.New("events_conformance_allow_callback_origin",
		"Trusts one webhook callback origin for the rest of the process.",
		func(_ context.Context, a originArgs) (string, error) {
			if err := testhook.TrustWebhookOrigin(ev, a.Origin); err != nil {
				return "", err
			}
			return "ok", nil
		})
	catalogTool := agenttool.New("events_conformance_catalog",
		`Adds an event type (answers "ok: <name>"), or removes, retypes or extends one it added.`,
		func(_ context.Context, a catalogArgs) (string, error) { return cat.do(ev, a) })
	existsTool := agenttool.New("events_conformance_subscription_exists",
		`Answers "true" while a principal's webhook subscription to a callback is registered, else "false".`,
		func(ctx context.Context, a tenantArgs) (string, error) {
			recs, err := store.ListByName(ctx, a.Name)
			if err != nil {
				return "", err
			}
			for _, r := range recs {
				if r.Principal == a.Principal && r.URL == a.URL && r.Expires.After(time.Now()) {
					return "true", nil
				}
			}
			return "false", nil
		})
	stateTool := agenttool.New("events_conformance_subscription_state",
		`Answers "active" or "absent" for a webhook subscription id.`,
		func(ctx context.Context, a idArgs) (string, error) {
			r, ok, err := store.Get(ctx, a.ID)
			if err != nil {
				return "", err
			}
			if ok && r.Expires.After(time.Now()) {
				return "active", nil
			}
			return "absent", nil
		})
	gapTool := agenttool.New("events_conformance_webhook_gap",
		"Sends one webhook subscription a gap envelope; answers \"ok\".",
		func(_ context.Context, a idArgs) (string, error) {
			if err := ev.GapWebhook(a.ID); err != nil {
				return "", err
			}
			return "ok", nil
		})
	terminateTool := agenttool.New("events_conformance_webhook_terminate",
		"Ends one webhook subscription with a terminated envelope; answers \"ok\".",
		func(_ context.Context, a idArgs) (string, error) {
			if err := ev.TerminateWebhook(a.ID, nil); err != nil {
				return "", err
			}
			return "ok", nil
		})
	subscribeAs := agenttool.New("events_conformance_subscribe_as",
		"Subscribes a webhook on another principal's behalf; answers the subscription id.",
		func(_ context.Context, a tenantArgs) (string, error) {
			id, err := subscribeAsPrincipal(handler, prefix, a)
			if err != nil {
				log.Printf("subscribe_as: %v", err)
			}
			return id, err
		})
	srv, err := mcpserver.NewServer("agenttool-events-conformance", "0.0.0", allow, catalogTool, existsTool, stateTool, gapTool, terminateTool, subscribeAs)
	if err != nil {
		return err
	}

	wh := &mcpserver.WebhookOptions{
		Principal: func(_ context.Context, extra *sdk.RequestExtra, _ *sdk.ServerSession) string {
			if extra != nil && extra.Header != nil {
				if v, ok := strings.CutPrefix(extra.Header.Get(principalHeader), prefix); ok {
					return v
				}
			}
			return principal
		},
		Store: store,
	}
	if placeholder != "" {
		cert, pool, err := selfSigned(placeholderHost)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", placeholder)
		if err != nil {
			return fmt.Errorf("the placeholder stub: %w (see -placeholder)", err)
		}
		go serveStub(ln, cert)
		defer ln.Close()
		wh.TrustedOrigins = []string{"https://" + placeholderHost}
		wh.TLSConfig = &tls.Config{RootCAs: pool}
		wh.Resolver = func(ctx context.Context, host string) ([]netip.Addr, error) {
			if host == placeholderHost {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	ev, err = mcpserver.AddEvents(srv, mcpserver.EventsOptions{Webhook: wh, ListPageSize: pageSize})
	if err != nil {
		return err
	}
	defer ev.Close()

	counter, err := ev.Register(mcpserver.EventDef{
		Name:          counterType,
		Description:   "A counter that ticks every two seconds.",
		EmitOnly:      true,
		InputSchema:   json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}}}`),
		PayloadSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
	})
	if err != nil {
		return err
	}
	alerts, err := ev.Register(mcpserver.EventDef{
		Name:          alertsType,
		Description:   "Alerts every three seconds, filtered by minimum severity; poll only.",
		EmitOnly:      true,
		Delivery:      []string{mcpserver.DeliveryPoll},
		InputSchema:   json.RawMessage(`{"type":"object","properties":{"minSeverity":{"type":"integer","minimum":0,"maximum":3}}}`),
		PayloadSchema: json.RawMessage(`{"type":"object","properties":{"severity":{"type":"integer"},"message":{"type":"string"}},"required":["severity","message"]}`),
		Match: func(_ mcpserver.EventContext, e mcpserver.Event, args json.RawMessage) bool {
			var d struct{ Severity int }
			var a struct{ MinSeverity int }
			_ = json.Unmarshal(e.Data, &d)
			_ = json.Unmarshal(args, &a)
			return d.Severity >= a.MinSeverity
		},
		Transform: func(_ mcpserver.EventContext, e mcpserver.Event, args json.RawMessage) (mcpserver.Event, error) {
			var d struct {
				Severity int    `json:"severity"`
				Message  string `json:"message"`
			}
			var a struct{ MinSeverity int }
			_ = json.Unmarshal(e.Data, &d)
			_ = json.Unmarshal(args, &a)
			d.Message = fmt.Sprintf("[min %d] %s", a.MinSeverity, d.Message)
			e.Data, _ = json.Marshal(d)
			return e, nil
		},
	})
	if err != nil {
		return err
	}
	hooks, err := ev.Register(mcpserver.EventDef{
		Name:          hooksType,
		Description:   "A hook every four seconds; webhook only.",
		EmitOnly:      true,
		Delivery:      []string{mcpserver.DeliveryWebhook},
		PayloadSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
	})
	if err != nil {
		return err
	}
	go tick(ctx, 4*time.Second, func(n int) error {
		return hooks.Emit(mcpserver.Event{Data: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))})
	})
	go tick(ctx, 2*time.Second, func(n int) error {
		return counter.Emit(mcpserver.Event{Data: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))})
	})
	go tick(ctx, 3*time.Second, func(n int) error {
		return alerts.Emit(mcpserver.Event{Data: json.RawMessage(fmt.Sprintf(`{"severity":%d,"message":"alert %d"}`, n%4, n))})
	})

	mux := http.NewServeMux()
	handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{Stateless: true})
	mux.Handle("/mcp", handler)
	hs := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hs.Shutdown(sc)
	}()
	log.Printf("events-conformance: serving http://%s/mcp", addr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// tick calls f with 1, 2, ... every d until ctx ends.
func tick(ctx context.Context, d time.Duration, f func(n int) error) {
	t := time.NewTicker(d)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f(n); err != nil {
				log.Printf("emit: %v", err)
			}
		}
	}
}

// serveStub answers on ln as https://conformance.invalid would: it
// echoes a verification challenge and accepts anything else.
func serveStub(ln net.Listener, cert tls.Certificate) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Type, Challenge string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.Type == "verification" {
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": body.Challenge})
			return
		}
		_, _ = w.Write([]byte("{}"))
	})
	s := &http.Server{Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	_ = s.ServeTLS(ln, "", "")
}

// selfSigned makes a certificate for host and the pool that trusts it.
func selfSigned(host string) (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool, nil
}

// mintedType is an event type the catalog control added, and the
// revision of its payload schema.
type mintedType struct {
	t        *mcpserver.EventType
	revision int
}

// catalog is the event types events_conformance_catalog added.
type catalog struct {
	mu     sync.Mutex
	count  int
	minted map[string]*mintedType
}

func mintDef(name string, revision int, input string) mcpserver.EventDef {
	return mcpserver.EventDef{
		Name:          name,
		Description:   fmt.Sprintf("Added by events_conformance_catalog (payload revision %d).", revision),
		EmitOnly:      true,
		Delivery:      []string{mcpserver.DeliveryPoll},
		InputSchema:   json.RawMessage(input),
		PayloadSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"revision":{"const":%d}},"required":["revision"]}`, revision)),
	}
}

const plainInput = `{"type":"object"}`

func (c *catalog) do(ev *mcpserver.Events, a catalogArgs) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.Op == "add" {
		c.count++
		name := fmt.Sprintf("conformance.minted.%d", c.count)
		t, err := ev.Register(mintDef(name, 1, plainInput))
		if err != nil {
			return "", err
		}
		c.minted[name] = &mintedType{t: t, revision: 1}
		return "ok: " + name, nil
	}
	m := c.minted[a.Name]
	if m == nil {
		return "", fmt.Errorf("%s needs the name of a type this control added", a.Op)
	}
	switch a.Op {
	case "remove":
		m.t.Remove()
		delete(c.minted, a.Name)
	case "retype":
		t, err := ev.Register(mintDef(a.Name, m.revision+1, plainInput))
		if err != nil {
			return "", err
		}
		m.t, m.revision = t, m.revision+1
	case "extend":
		// A change to the input schema that only adds a property.
		t, err := ev.Register(mintDef(a.Name, m.revision, `{"type":"object","properties":{"note":{"type":"string"}}}`))
		if err != nil {
			return "", err
		}
		m.t = t
	default:
		return "", fmt.Errorf("unknown op %q", a.Op)
	}
	return "ok", nil
}

var subIDPattern = regexp.MustCompile(`"id"\s*:\s*"(sub_[^"]+)"`)

// subscribeAsPrincipal sends events/subscribe to h as the principal a
// names, with the header only this process can make, and answers the
// subscription id.
func subscribeAsPrincipal(h http.Handler, prefix string, a tenantArgs) (string, error) {
	var sec [32]byte
	_, _ = rand.Read(sec[:])
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "events/subscribe",
		"params": map[string]any{
			"name": a.Name, "arguments": map[string]any{},
			"delivery": map[string]any{"mode": "webhook", "url": a.URL, "secret": "whsec_" + base64.StdEncoding.EncodeToString(sec[:])},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "events-conformance-subscribe-as", "version": "1.0.0"},
			},
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", "events/subscribe")
	r.Header.Set("Mcp-Name", a.Name)
	r.Header.Set(principalHeader, prefix+a.Principal)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	m := subIDPattern.FindStringSubmatch(w.Body.String())
	if m == nil {
		return "", fmt.Errorf("no subscription id in the answer: %s", w.Body.String())
	}
	return m[1], nil
}
