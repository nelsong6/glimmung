package server

import (
	"context"
	"strings"
)

// inspectionLeaseResolver routes ResolveTestSlotLeaseByTankSession at
// the StateStore behind a stable contract so the POST /v1/inspections
// handler stays decoupled from the concrete store type. The matching
// logic mirrors resolveTestSlotLeaseForExtend in test_slot_api.go:
// claimed/pending test-slot leases whose Metadata.tank_session_id
// matches the supplied id.
type inspectionLeaseResolver struct {
	store StateStore
}

func newInspectionLeaseResolver(store StateStore) *inspectionLeaseResolver {
	return &inspectionLeaseResolver{store: store}
}

func (r *inspectionLeaseResolver) ResolveTestSlotLeaseByTankSession(ctx context.Context, project, tankSessionID string) (Lease, error) {
	if r == nil || r.store == nil {
		return Lease{}, ErrNotFound
	}
	target := strings.TrimSpace(tankSessionID)
	if target == "" {
		return Lease{}, ErrNotFound
	}
	project = strings.TrimSpace(project)
	leases, err := r.store.ListLeases(ctx)
	if err != nil {
		return Lease{}, err
	}
	var candidates []Lease
	for _, lease := range leases {
		if project != "" && lease.Project != project {
			continue
		}
		if !boolFromMap(lease.Metadata, "test_slot_checkout") {
			continue
		}
		if lease.State != "claimed" && lease.State != "pending" {
			continue
		}
		if !testSlotLeaseMatchesTankSession(lease, target) {
			continue
		}
		candidates = append(candidates, lease)
	}
	if len(candidates) == 0 {
		return Lease{}, ErrNotFound
	}
	sortLeasesForReturn(candidates)
	return candidates[0], nil
}

