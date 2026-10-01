package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// rotatingProvider is a token endpoint that rotates the refresh token
// on every refresh and accepts each refresh token once. Its tokens
// expire in a second, inside oauth2's ten-second early-expiry window,
// so every Token call refreshes.
type rotatingProvider struct {
	mu      sync.Mutex
	n       int
	current string
	srv     *httptest.Server
}

func newRotatingProvider(t *testing.T) *rotatingProvider {
	p := &rotatingProvider{current: "r0"}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != p.current {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		p.n++
		p.current = fmt.Sprintf("r%d", p.n)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("a%d", p.n),
			"refresh_token": p.current,
			"token_type":    "Bearer",
			"expires_in":    1,
		})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *rotatingProvider) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     "registered",
		ClientSecret: "s3cret",
		Endpoint:     oauth2.Endpoint{TokenURL: p.srv.URL, AuthStyle: oauth2.AuthStyleInParams},
		Scopes:       []string{"issues"},
	}
}

var testKey = TokenKey{Endpoint: "https://mcp.example/mcp", Subject: "alice"}

func failOnSave(t *testing.T) func(error) {
	return func(err error) { t.Errorf("save: %v", err) }
}

func storedRefresh(t *testing.T, s TokenStore) string {
	t.Helper()
	rec, err := s.Load(context.Background(), testKey)
	if err != nil || rec == nil || rec.Token == nil {
		t.Fatalf("Load = %v, %v", rec, err)
	}
	return rec.Token.RefreshToken
}

// The case the store exists for: a provider that rotates refresh
// tokens, a refresh inside the running source, then a restart.
func TestStoreTokensSavesRotatedRefreshTokensAcrossRestart(t *testing.T) {
	p := newRotatingProvider(t)
	store := &MemoryTokenStore{}

	cfg := &auth.AuthorizationCodeHandlerConfig{}
	if err := StoreTokens(context.Background(), cfg, store, testKey, failOnSave(t)); err != nil {
		t.Fatal(err)
	}
	if cfg.InitialTokenSource != nil {
		t.Fatal("InitialTokenSource set with nothing stored")
	}
	exchanged := &oauth2.Token{AccessToken: "a0", RefreshToken: "r0", Expiry: time.Now().Add(time.Second)}
	ts, err := cfg.NewTokenSource(clientContext(nil), p.config(), exchanged)
	if err != nil {
		t.Fatal(err)
	}
	if got := storedRefresh(t, store); got != "r0" {
		t.Fatalf("after the exchange stored %q, want r0", got)
	}
	rec, _ := store.Load(context.Background(), testKey)
	if rec.TokenURL != p.srv.URL || rec.ClientID != "registered" || rec.ClientSecret != "s3cret" || rec.AuthStyle != oauth2.AuthStyleInParams {
		t.Fatalf("stored record %+v lacks what refreshing needs", rec)
	}

	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "r1" || storedRefresh(t, store) != "r1" {
		t.Fatalf("refresh returned %q and stored %q, want r1 both", tok.RefreshToken, storedRefresh(t, store))
	}

	// Restart: a new handler config, the same store, and a client of
	// its own that the restored source must refresh through.
	var viaClient atomic.Int32
	cfg2 := &auth.AuthorizationCodeHandlerConfig{
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			viaClient.Add(1)
			return http.DefaultTransport.RoundTrip(r)
		})},
	}
	if err := StoreTokens(context.Background(), cfg2, store, testKey, failOnSave(t)); err != nil {
		t.Fatal(err)
	}
	if cfg2.InitialTokenSource == nil {
		t.Fatal("InitialTokenSource not set from the stored grant")
	}
	tok, err = cfg2.InitialTokenSource.Token()
	if err != nil {
		t.Fatalf("refresh after restart: %v", err)
	}
	if tok.AccessToken != "a2" || storedRefresh(t, store) != "r2" {
		t.Fatalf("after restart got %q and stored %q, want a2 and r2", tok.AccessToken, storedRefresh(t, store))
	}
	if viaClient.Load() == 0 {
		t.Fatal("restored source did not refresh through the configured client")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// flakyStore fails the saves it is told to.
type flakyStore struct {
	MemoryTokenStore
	fail atomic.Int32
}

func (s *flakyStore) Save(ctx context.Context, key TokenKey, rec *TokenRecord) error {
	if s.fail.Add(-1) >= 0 {
		return errors.New("disk full")
	}
	return s.MemoryTokenStore.Save(ctx, key, rec)
}

func TestStoreTokensReportsAFailedSaveAndRetries(t *testing.T) {
	p := newRotatingProvider(t)
	store := &flakyStore{}
	var reported []error
	cfg := &auth.AuthorizationCodeHandlerConfig{}
	if err := StoreTokens(context.Background(), cfg, store, testKey, func(err error) { reported = append(reported, err) }); err != nil {
		t.Fatal(err)
	}
	ts, err := cfg.NewTokenSource(clientContext(nil), p.config(), &oauth2.Token{AccessToken: "a0", RefreshToken: "r0", Expiry: time.Now().Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	store.fail.Store(1)
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("a failed save failed the refresh: %v", err)
	}
	if tok.RefreshToken != "r1" || len(reported) != 1 {
		t.Fatalf("got %q with %d reported errors, want r1 and 1", tok.RefreshToken, len(reported))
	}
	if got := storedRefresh(t, store); got != "r0" {
		t.Fatalf("stored %q, want r0 while the save fails", got)
	}
	if _, err := ts.Token(); err != nil {
		t.Fatal(err)
	}
	if got := storedRefresh(t, store); got != "r2" {
		t.Fatalf("stored %q after the store recovered, want r2", got)
	}
}

func TestStoreTokensRefusesAMisconfiguredCall(t *testing.T) {
	ctx := context.Background()
	store := &MemoryTokenStore{}
	if err := StoreTokens(ctx, &auth.AuthorizationCodeHandlerConfig{}, store, testKey, nil); err == nil {
		t.Error("nil onSaveError accepted")
	}
	preset := &auth.AuthorizationCodeHandlerConfig{
		NewTokenSource: func(context.Context, *oauth2.Config, *oauth2.Token) (oauth2.TokenSource, error) { return nil, nil },
	}
	if err := StoreTokens(ctx, preset, store, testKey, func(error) {}); err == nil {
		t.Error("a config with NewTokenSource already set accepted")
	}
	failing := &loadErrStore{}
	if err := StoreTokens(ctx, &auth.AuthorizationCodeHandlerConfig{}, failing, testKey, func(error) {}); err == nil {
		t.Error("a failed Load not returned")
	}
}

type loadErrStore struct{ MemoryTokenStore }

func (*loadErrStore) Load(context.Context, TokenKey) (*TokenRecord, error) {
	return nil, errors.New("locked")
}

// An expired token with no refresh token is passed on so the server
// answers 401 and the handler authorizes again; the SDK fails the
// request on any other token-source error.
func TestStoreTokensPassesOnAnUnrefreshableToken(t *testing.T) {
	store := &MemoryTokenStore{}
	expired := &oauth2.Token{AccessToken: "old", Expiry: time.Now().Add(-time.Hour)}
	_ = store.Save(context.Background(), testKey, &TokenRecord{Token: expired})

	cfg := &auth.AuthorizationCodeHandlerConfig{}
	if err := StoreTokens(context.Background(), cfg, store, testKey, failOnSave(t)); err != nil {
		t.Fatal(err)
	}
	if cfg.InitialTokenSource != nil {
		t.Fatal("an expired grant with no refresh token was installed")
	}
	ts, err := cfg.NewTokenSource(clientContext(nil), &oauth2.Config{}, expired)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Token()
	if err != nil || tok.AccessToken != "old" {
		t.Fatalf("Token = %v, %v; want the expired token and no error", tok, err)
	}
}

// The handler takes the restored source as its token source, so the
// connection starts authorized.
func TestStoreTokensInstallsTheRestoredGrantOnTheHandler(t *testing.T) {
	store := &MemoryTokenStore{}
	_ = store.Save(context.Background(), testKey, &TokenRecord{
		Token: &oauth2.Token{AccessToken: "live", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
	})
	cfg := &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: "my-harness"},
		RedirectURL:         "http://127.0.0.1:8765/callback",
		AuthorizationCodeFetcher: func(context.Context, *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return nil, errors.New("asked to consent with a live grant stored")
		},
	}
	if err := StoreTokens(context.Background(), cfg, store, testKey, failOnSave(t)); err != nil {
		t.Fatal(err)
	}
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := h.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("TokenSource = %v, %v", ts, err)
	}
	tok, err := ts.Token()
	if err != nil || tok.AccessToken != "live" {
		t.Fatalf("Token = %v, %v; want the stored token", tok, err)
	}
}

func TestMemoryTokenStoreCopies(t *testing.T) {
	ctx := context.Background()
	s := &MemoryTokenStore{}
	if rec, err := s.Load(ctx, testKey); rec != nil || err != nil {
		t.Fatalf("empty Load = %v, %v; want nil, nil", rec, err)
	}
	in := &TokenRecord{Token: &oauth2.Token{AccessToken: "a"}, Scopes: []string{"x"}}
	_ = s.Save(ctx, testKey, in)
	in.Token.AccessToken, in.Scopes[0] = "changed", "changed"
	out, _ := s.Load(ctx, testKey)
	if out.Token.AccessToken != "a" || out.Scopes[0] != "x" {
		t.Fatalf("store shares memory with its caller: %+v", out)
	}
	if other, _ := s.Load(ctx, TokenKey{Endpoint: testKey.Endpoint}); other != nil {
		t.Fatal("a grant for one subject loaded for another")
	}
}
