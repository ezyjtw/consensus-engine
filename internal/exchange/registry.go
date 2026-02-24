package exchange

import (
	"context"
	"fmt"
	"sync"
)

// Registry manages Exchange instances by venue name.
// It lazily initialises adapters using a CredentialStore and venue-specific factories.
type Registry struct {
	mu        sync.RWMutex
	exchanges map[string]Exchange
	creds     *CredentialStore
	factories map[string]Factory
}

// Factory creates an Exchange instance for a venue given its credentials.
type Factory func(creds *Credentials) (Exchange, error)

// NewRegistry creates a venue registry that lazily initialises exchange adapters.
func NewRegistry(creds *CredentialStore) *Registry {
	return &Registry{
		exchanges: make(map[string]Exchange),
		creds:     creds,
		factories: make(map[string]Factory),
	}
}

// RegisterFactory registers a factory for a venue name.
// Call this at startup for each supported venue.
func (r *Registry) RegisterFactory(venue string, f Factory) {
	r.mu.Lock()
	r.factories[venue] = f
	r.mu.Unlock()
}

// Get returns the Exchange instance for a venue, creating it if needed.
func (r *Registry) Get(ctx context.Context, venue string) (Exchange, error) {
	r.mu.RLock()
	if ex, ok := r.exchanges[venue]; ok {
		r.mu.RUnlock()
		return ex, nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check.
	if ex, ok := r.exchanges[venue]; ok {
		return ex, nil
	}

	factory, ok := r.factories[venue]
	if !ok {
		return nil, fmt.Errorf("no factory registered for venue %q", venue)
	}

	creds, err := r.creds.Get(ctx, venue)
	if err != nil {
		return nil, fmt.Errorf("getting credentials for %s: %w", venue, err)
	}

	ex, err := factory(creds)
	if err != nil {
		return nil, fmt.Errorf("creating exchange client for %s: %w", venue, err)
	}

	r.exchanges[venue] = ex
	return ex, nil
}

// All returns all currently initialised Exchange instances.
func (r *Registry) All() map[string]Exchange {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Exchange, len(r.exchanges))
	for k, v := range r.exchanges {
		out[k] = v
	}
	return out
}

// Register directly registers a pre-built Exchange instance (useful for tests).
func (r *Registry) Register(venue string, ex Exchange) {
	r.mu.Lock()
	r.exchanges[venue] = ex
	r.mu.Unlock()
}
