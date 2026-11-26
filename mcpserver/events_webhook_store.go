package mcpserver

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"
)

// WebhookRecord is a webhook subscription as a [WebhookStore] holds it.
// It holds the client's signing secrets, so a store that leaves the
// process keeps it as it would a credential.
type WebhookRecord struct {
	// ID is the derived subscription id, "sub_...".
	ID string
	// Principal, URL, Name and Arguments are the subscription's key,
	// with ID derived from them. Arguments is JSON.
	Principal string
	URL       string
	Name      string
	Arguments json.RawMessage
	// Secret is the delivery.secret, "whsec_..."; PreviousSecret is the
	// one it replaced, which deliveries are still signed with until
	// PreviousExpires.
	Secret          string
	PreviousSecret  string
	PreviousExpires time.Time
	// Created and Expires are when the subscription was made and when
	// its grant ends.
	Created time.Time
	Expires time.Time
}

// WebhookStore holds the webhook subscriptions of a server. The default
// is a map in memory, which a restart empties: clients subscribe anew
// when they refresh, and the grants this package makes are short enough
// for that. A store that outlives the process lets a replica read what
// another wrote. Methods may be called at once.
type WebhookStore interface {
	// Get returns the record with id, or false.
	Get(ctx context.Context, id string) (WebhookRecord, bool, error)
	// Put adds or replaces the record under its ID.
	Put(ctx context.Context, r WebhookRecord) error
	// Delete removes the record and says whether it was there: of
	// replicas that delete a record at once, one is told true, and it
	// alone ends the subscription's hooks.
	Delete(ctx context.Context, id string) (bool, error)
	// ListByName returns the records of an event type.
	ListByName(ctx context.Context, name string) ([]WebhookRecord, error)
}

// NewMemoryWebhookStore is the default [WebhookStore], a map.
func NewMemoryWebhookStore() WebhookStore {
	return &memoryWebhookStore{m: map[string]WebhookRecord{}}
}

type memoryWebhookStore struct {
	mu sync.Mutex
	m  map[string]WebhookRecord
}

func (s *memoryWebhookStore) Get(_ context.Context, id string) (WebhookRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	return clone(r), ok, nil
}

func (s *memoryWebhookStore) Put(_ context.Context, r WebhookRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[r.ID] = clone(r)
	return nil
}

func (s *memoryWebhookStore) Delete(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[id]
	delete(s.m, id)
	return ok, nil
}

func (s *memoryWebhookStore) ListByName(_ context.Context, name string) ([]WebhookRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []WebhookRecord
	for _, k := range slices.Sorted(maps.Keys(s.m)) {
		if r := s.m[k]; r.Name == name {
			out = append(out, clone(r))
		}
	}
	return out, nil
}

func clone(r WebhookRecord) WebhookRecord {
	r.Arguments = slices.Clone(r.Arguments)
	return r
}
