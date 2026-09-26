package handler

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

// OwnerHostReadiness is the server's cached view of which owners'
// "*.<owner>.<base>" certificates are ready (owner_hosts, written by the
// owner-hosts reconciler). HostModel asks it on every address it builds,
// so it answers from memory and refreshes on a timer: an owner whose
// certificate has just become ready waits at most one interval before their
// sites move to their own hosts, and in the meantime keeps being served at
// the owner-host path.
type OwnerHostReadiness struct {
	database *sql.DB
	mu       sync.RWMutex
	ready    map[string]bool
}

// NewOwnerHostReadiness loads the ready set once and refreshes it every
// interval until ctx ends. A failed load keeps the previous set (initially
// empty, which serves everything at the owner-host path: safe, never a TLS
// error).
func NewOwnerHostReadiness(ctx context.Context, database *sql.DB, interval time.Duration) *OwnerHostReadiness {
	o := &OwnerHostReadiness{database: database, ready: map[string]bool{}}
	o.refresh(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.refresh(ctx)
			}
		}
	}()
	return o
}

func (o *OwnerHostReadiness) refresh(ctx context.Context) {
	labels, err := db.ReadyOwnerLabels(ctx, o.database)
	if err != nil {
		log.Printf("owner hosts: load ready owners: %v", err)
		return
	}
	next := make(map[string]bool, len(labels))
	for _, l := range labels {
		next[l] = true
	}
	o.mu.Lock()
	o.ready = next
	o.mu.Unlock()
}

// Ready reports whether ownerLabel's certificate is ready.
func (o *OwnerHostReadiness) Ready(ownerLabel string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.ready[ownerLabel]
}
