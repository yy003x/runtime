package session

import (
	"fmt"
	"time"
)

// NewMaintenanceService composes the Session query and recovery control plane
// without loading Profile, Provider, or runtime configuration. The returned
// service is intentionally not execution-capable.
func NewMaintenanceService(store *Store) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	service := &Service{store: store, now: time.Now}
	if err := service.cleanupInvocationManifests(); err != nil {
		return nil, fmt.Errorf("clean private invocation manifests: %w", err)
	}
	if err := service.reconcileStaleSessions(); err != nil {
		return nil, fmt.Errorf("reconcile Session store: %w", err)
	}
	return service, nil
}
