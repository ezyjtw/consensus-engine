package store

import (
	"sync"
	"time"

	"github.com/yourorg/consensus-engine/internal/consensus"
)

type Key struct {
	TenantID string
	Symbol   consensus.Symbol
	Venue    consensus.Venue
}

type Store struct {
	mu      sync.RWMutex
	quotes  map[Key]consensus.Quote
	status  map[Key]consensus.VenueStatus
	staleMs int64
}

func New(staleMs int64) *Store {
	return &Store{
		quotes:  make(map[Key]consensus.Quote),
		status:  make(map[Key]consensus.VenueStatus),
		staleMs: staleMs,
	}
}

func (s *Store) UpsertQuote(q consensus.Quote) {
	k := Key{TenantID: q.TenantID, Symbol: q.Symbol, Venue: q.Venue}
	s.mu.Lock()
	s.quotes[k] = q
	s.mu.Unlock()
}

func (s *Store) LiveQuotes(tenantID string, symbol consensus.Symbol) map[consensus.Venue]consensus.Quote {
	nowMs := time.Now().UnixMilli()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[consensus.Venue]consensus.Quote)
	for k, q := range s.quotes {
		if k.TenantID != tenantID || k.Symbol != symbol {
			continue
		}
		if nowMs-q.FeedHealth.LastMsgTsMs > s.staleMs {
			continue
		}
		out[k.Venue] = q
	}
	return out
}

func (s *Store) GetStatus(tenantID string, symbol consensus.Symbol,
	venue consensus.Venue) consensus.VenueStatus {
	k := Key{TenantID: tenantID, Symbol: symbol, Venue: venue}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.status[k]
	if !ok {
		return consensus.VenueStatus{State: consensus.StateOK}
	}
	return st
}

func (s *Store) SetStatus(tenantID string, symbol consensus.Symbol,
	venue consensus.Venue, vs consensus.VenueStatus) {
	k := Key{TenantID: tenantID, Symbol: symbol, Venue: venue}
	s.mu.Lock()
	s.status[k] = vs
	s.mu.Unlock()
}
