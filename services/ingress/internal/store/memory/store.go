// Package memory is a development-only Appender.
//
// It keeps deliveries in process memory so the service can be exercised end to
// end before a Postgres instance exists. It is NOT durable: restarting the
// process loses everything. Because an in-memory store silently drops messages
// if it is ever mistaken for production, Store refuses to be constructed unless
// the caller passes an explicit acknowledgement of that fact.
package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aucxley/chipmank/services/ingress/internal/httpapi"
)

// ErrEphemeralStoreRefused is returned when Store is asked to pretend it is
// durable.
var ErrEphemeralStoreRefused = errors.New(
	"memory: refusing to start; set INGRESS_ALLOW_EPHEMERAL_STORE=1 to acknowledge this store loses data on restart")

type Record struct {
	Delivery  httpapi.Delivery
	StoredAt  time.Time
	ItemCount int
}

type Store struct {
	mu      sync.Mutex
	items   map[string]Record
	order   []string
	ackLost bool
}

// New builds a Store. ackEphemeral must be true, and is only true when the
// operator has explicitly opted in via configuration.
func New(ackEphemeral bool) (*Store, error) {
	if !ackEphemeral {
		return nil, ErrEphemeralStoreRefused
	}
	return &Store{items: map[string]Record{}}, nil
}

func (s *Store) Append(_ context.Context, d httpapi.Delivery) (httpapi.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// This is the same contract the Postgres store must honour: at-most-once
	// per delivery key, decided by the unique index inside the transaction.
	if _, exists := s.items[d.DeliveryKey]; exists {
		return httpapi.OutcomeDuplicate, nil
	}
	s.items[d.DeliveryKey] = Record{Delivery: d, StoredAt: time.Now(), ItemCount: len(d.Items)}
	s.order = append(s.order, d.DeliveryKey)
	return httpapi.OutcomeStored, nil
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// Get returns a stored record by delivery key.
func (s *Store) Get(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	return r, ok
}

func (s *Store) String() string {
	return fmt.Sprintf("memory.Store(%d deliveries)", s.Len())
}
