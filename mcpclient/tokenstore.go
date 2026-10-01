package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// TokenKey names one stored grant: the server it is for and the user
// it was granted by.
//
// Endpoint is the URL the transport connects to. It is not the
// protected-resource metadata's resource, which the SDK learns only
// when it authorizes, after the stored grant is needed. Subject is a
// name the host chooses for its user, and empty in a harness with one.
type TokenKey struct {
	Endpoint string
	Subject  string
}

// TokenRecord is a stored grant: the token and what refreshing it
// needs. The SDK discovers the token endpoint and registers the client
// while it authorizes, so after a restart a bare token could be used
// until it expired and then never refreshed. ClientSecret is a secret
// as much as the token is; a client registered dynamically has one.
type TokenRecord struct {
	Token        *oauth2.Token
	TokenURL     string
	AuthStyle    oauth2.AuthStyle
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// TokenStore keeps OAuth grants for [StoreTokens]. Load returns nil
// and no error when nothing is stored under the key.
//
// A store is called with secrets and must keep them as such; mcpclient
// never logs or records what passes through it. Save is called on
// every refresh that changes the token, from whichever goroutine the
// transport is sending on. A store shared by several processes needs
// its own locking: a provider that rotates refresh tokens accepts
// each once, so two processes refreshing from the same record leave
// one of them with a grant the provider refuses.
type TokenStore interface {
	Load(ctx context.Context, key TokenKey) (*TokenRecord, error)
	Save(ctx context.Context, key TokenKey, rec *TokenRecord) error
}

// StoreTokens wires store into cfg, an SDK authorization-code handler
// configuration, so the grant under key survives a restart. Call it
// before [auth.NewAuthorizationCodeHandler].
//
// It loads the grant under key and, when one is stored, sets
// cfg.InitialTokenSource to a source that refreshes it, so the
// connection starts authorized. It sets cfg.NewTokenSource so the
// grant the handler obtains when it authorizes is saved, and then
// saved again whenever a refresh changes it. The SDK builds that
// source once and every later refresh happens inside it, so saving
// only the token it starts from would lose each refresh token the
// provider rotates, and the next restart would fail to refresh.
//
// A Save that fails cannot fail the refresh: by then the provider has
// replaced the refresh token, and returning an error would discard the
// only one that works. The error goes to onSaveError, which is
// required, and the save is tried again on the next request.
//
// Both sources refresh through cfg.Client, as the SDK's own does. A
// token that has expired with no refresh token is passed on rather
// than refused, so the server answers 401 and the handler authorizes
// again; refusing it would fail the request instead.
//
// It returns an error when Load fails, when onSaveError is nil, or
// when cfg already sets either hook.
func StoreTokens(ctx context.Context, cfg *auth.AuthorizationCodeHandlerConfig, store TokenStore, key TokenKey, onSaveError func(error)) error {
	if cfg == nil || store == nil {
		return errors.New("mcpclient: StoreTokens needs a config and a store")
	}
	if onSaveError == nil {
		return errors.New("mcpclient: StoreTokens needs onSaveError, or a failed save would lose a rotated refresh token unseen")
	}
	if cfg.NewTokenSource != nil || cfg.InitialTokenSource != nil {
		return errors.New("mcpclient: StoreTokens sets NewTokenSource and InitialTokenSource, and the config already sets one")
	}
	// Saves happen on refreshes long after this call returns.
	saveCtx := context.WithoutCancel(ctx)
	rec, err := store.Load(ctx, key)
	if err != nil {
		return err
	}
	if usable(rec) {
		rec = cloneRecord(rec)
		cfg.InitialTokenSource = &storedSource{
			ctx: saveCtx, store: store, key: key, rec: *rec, onErr: onSaveError,
			cur: rec.Token, last: rec.Token,
			// The handler fills in cfg.Client after this returns, so the
			// refreshing source is built on first use.
			open: func() oauth2.TokenSource {
				return configOf(rec).TokenSource(clientContext(cfg.Client), rec.Token)
			},
		}
	}
	cfg.NewTokenSource = func(refreshCtx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		s := &storedSource{
			ctx: saveCtx, store: store, key: key, rec: recordOf(oc), onErr: onSaveError,
			src: oc.TokenSource(refreshCtx, tok), cur: tok,
		}
		s.mu.Lock()
		s.save(tok)
		s.mu.Unlock()
		return s, nil
	}
	return nil
}

// usable reports whether rec can authorize a request now or after a
// refresh. An expired token with no way to refresh would only cost a
// request that the server refuses.
func usable(rec *TokenRecord) bool {
	if rec == nil || rec.Token == nil {
		return false
	}
	return rec.Token.Valid() || (rec.Token.RefreshToken != "" && rec.TokenURL != "")
}

// storedSource is a token source that saves each token that differs
// from the last one saved.
type storedSource struct {
	ctx   context.Context
	store TokenStore
	key   TokenKey
	rec   TokenRecord // everything but the token
	onErr func(error)

	mu   sync.Mutex
	src  oauth2.TokenSource
	open func() oauth2.TokenSource
	cur  *oauth2.Token // the token returned last
	last *oauth2.Token // the last token saved, or loaded
}

func (s *storedSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && !s.cur.Valid() && s.cur.RefreshToken == "" {
		// Nothing to refresh with: let the server refuse it, which
		// sends the handler to authorize, rather than fail the request.
		return s.cur, nil
	}
	if s.src == nil {
		s.src = s.open()
	}
	tok, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.cur = tok
	if !sameToken(tok, s.last) {
		s.save(tok)
	}
	return tok, nil
}

// save stores tok and, when the store takes it, remembers it as the
// last saved. A token the store refused stays unsaved, so the next
// call tries again. s.mu is held.
func (s *storedSource) save(tok *oauth2.Token) {
	rec := s.rec
	rec.Token = tok
	if err := s.store.Save(s.ctx, s.key, cloneRecord(&rec)); err != nil {
		s.onErr(err)
		return
	}
	s.last = tok
}

func sameToken(a, b *oauth2.Token) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AccessToken == b.AccessToken &&
		a.RefreshToken == b.RefreshToken &&
		a.TokenType == b.TokenType &&
		a.Expiry.Equal(b.Expiry)
}

func recordOf(oc *oauth2.Config) TokenRecord {
	return TokenRecord{
		TokenURL:     oc.Endpoint.TokenURL,
		AuthStyle:    oc.Endpoint.AuthStyle,
		ClientID:     oc.ClientID,
		ClientSecret: oc.ClientSecret,
		Scopes:       slices.Clone(oc.Scopes),
	}
}

func configOf(rec *TokenRecord) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     rec.ClientID,
		ClientSecret: rec.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: rec.TokenURL, AuthStyle: rec.AuthStyle},
		Scopes:       slices.Clone(rec.Scopes),
	}
}

// clientContext is the context the SDK refreshes on: a background
// context, since the source outlives any request, carrying the
// handler's HTTP client.
func clientContext(c *http.Client) context.Context {
	if c == nil {
		c = http.DefaultClient
	}
	return context.WithValue(context.Background(), oauth2.HTTPClient, c)
}

func cloneRecord(rec *TokenRecord) *TokenRecord {
	if rec == nil {
		return nil
	}
	c := *rec
	if rec.Token != nil {
		t := *rec.Token
		c.Token = &t
	}
	c.Scopes = slices.Clone(rec.Scopes)
	return &c
}

// MemoryTokenStore is a [TokenStore] that holds grants in memory, for
// tests and for a process that need not outlive its grants. The zero
// value is ready to use.
type MemoryTokenStore struct {
	mu sync.Mutex
	m  map[TokenKey]*TokenRecord
}

func (s *MemoryTokenStore) Load(_ context.Context, key TokenKey) (*TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRecord(s.m[key]), nil
}

func (s *MemoryTokenStore) Save(_ context.Context, key TokenKey, rec *TokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[TokenKey]*TokenRecord)
	}
	s.m[key] = cloneRecord(rec)
	return nil
}
