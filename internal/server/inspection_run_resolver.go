package server

import (
	"context"
	"strings"
)

// inspectionRunResolver wraps the existing run-replay read path so the
// POST /v1/inspections handler can validate a caller-supplied run_id
// without duplicating run-lookup logic. The resolver returns the
// project the run actually belongs to so the handler can reject
// cross-project run_id values explicitly.
type inspectionRunResolver struct {
	store inspectionRunReadStore
}

// inspectionRunReadStore is the minimal store surface the resolver
// needs. The runtime store wrapper already satisfies it.
type inspectionRunReadStore interface {
	ReadRunForReplay(ctx context.Context, project, runID string) (RunReplayData, error)
}

func newInspectionRunResolver(store inspectionRunReadStore) *inspectionRunResolver {
	return &inspectionRunResolver{store: store}
}

// ResolveInspectionRunProject reads the run identified by runID for the
// caller-supplied project and returns the run's recorded project name.
// Run-not-found is forwarded as ErrNotFound; the handler maps that to a
// 404. The wrapper trims whitespace; empty inputs fall through as
// ErrNotFound so the handler treats them as "no such run."
func (r *inspectionRunResolver) ResolveInspectionRunProject(ctx context.Context, project, runID string) (string, error) {
	project = strings.TrimSpace(project)
	runID = strings.TrimSpace(runID)
	if project == "" || runID == "" {
		return "", ErrNotFound
	}
	if r == nil || r.store == nil {
		return "", ErrNotFound
	}
	run, err := r.store.ReadRunForReplay(ctx, project, runID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(run.Project) == "" {
		return "", ErrNotFound
	}
	return run.Project, nil
}
