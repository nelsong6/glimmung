package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nelsong6/glimmung/internal/metrics"
)

// inspectionSweepRegistry holds the artifact writer the lease-cleanup
// sweep uses to delete inspection blobs after the matching ledger rows
// are dropped. It is set once at server construction time when a real
// artifact writer is wired through; cleanup paths consult it without
// the cleanup goroutine needing the writer plumbed through every
// signature.
//
// The package-level shape mirrors how other cross-cutting hooks reach
// the cleanup goroutine (the wakeRunQueue family). It is not a global
// mutex; the sweep is short and bounded.
var inspectionSweepRegistry struct {
	mu     sync.RWMutex
	writer ArtifactWriter
}

// SetInspectionSweepArtifactWriter registers the artifact writer the
// lease-cleanup sweep delegates to. Called once at handler
// construction; safe to call again with the same value during tests.
// Passing nil disables the sweep without affecting the rest of cleanup.
func SetInspectionSweepArtifactWriter(writer ArtifactWriter) {
	inspectionSweepRegistry.mu.Lock()
	inspectionSweepRegistry.writer = writer
	inspectionSweepRegistry.mu.Unlock()
}

func inspectionSweepArtifactWriter() ArtifactWriter {
	inspectionSweepRegistry.mu.RLock()
	defer inspectionSweepRegistry.mu.RUnlock()
	return inspectionSweepRegistry.writer
}

// sweepLeaseInspections drops every slot_inspections row tied to the
// given lease and deletes the matching report.json + screenshot.png
// blobs from object storage. Idempotent: a partial sweep that fails on
// blob delete leaves the rows gone (the next sweep finds nothing) but
// surfaces the failure through glimmung_inspections_swept_total{outcome=error}
// so operators can spot orphans.
//
// The function is called as the last step of cleanupTestSlotRuntime
// after the slot teardown succeeded; failure here does not block
// setLeaseSlotCleanupFinished from recording cleanup success because
// orphan blobs are recoverable (the artifact store has its own
// retention policy and operators can re-run a sweep), but the ledger
// row deletion has to succeed for the lifecycle accounting to stay
// consistent.
func sweepLeaseInspections(ctx context.Context, store ReadStore, lease Lease, logf func(string, ...any)) {
	if store == nil {
		return
	}
	leaseID := strings.TrimSpace(lease.ID)
	if leaseID == "" {
		return
	}
	inspectionStore, ok := store.(SlotInspectionStore)
	if !ok || inspectionStore == nil {
		return
	}
	sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := inspectionStore.DeleteSlotInspectionsByLease(sweepCtx, leaseID)
	if err != nil {
		metrics.RecordInspectionSwept(metrics.InspectionSweepPieceRow, metrics.InspectionSweepOutcomeError)
		if logf != nil {
			logf("inspection sweep: ledger delete failed lease=%s: %v", leaseID, err)
		}
		return
	}
	if len(rows) == 0 {
		return
	}
	for range rows {
		metrics.RecordInspectionSwept(metrics.InspectionSweepPieceRow, metrics.InspectionSweepOutcomeOK)
	}
	writer := inspectionSweepArtifactWriter()
	if writer == nil {
		// Ledger gone; blobs remain. Surface as an explicit error so
		// operators can spot the misconfiguration without parsing logs.
		for range rows {
			metrics.RecordInspectionSwept(metrics.InspectionSweepPieceReport, metrics.InspectionSweepOutcomeError)
			metrics.RecordInspectionSwept(metrics.InspectionSweepPieceScreenshot, metrics.InspectionSweepOutcomeError)
		}
		if logf != nil {
			logf("inspection sweep: artifact writer not configured; %d rows swept, blobs orphaned lease=%s", len(rows), leaseID)
		}
		return
	}
	deleteCtx, cancelDelete := context.WithTimeout(ctx, 60*time.Second)
	defer cancelDelete()
	for _, row := range rows {
		if err := writer.Delete(deleteCtx, row.ReportBlobPath); err != nil {
			metrics.RecordInspectionSwept(metrics.InspectionSweepPieceReport, metrics.InspectionSweepOutcomeError)
			if logf != nil {
				logf("inspection sweep: delete report blob failed lease=%s blob=%s: %v", leaseID, row.ReportBlobPath, err)
			}
		} else {
			metrics.RecordInspectionSwept(metrics.InspectionSweepPieceReport, metrics.InspectionSweepOutcomeOK)
		}
		if err := writer.Delete(deleteCtx, row.ScreenshotBlobPath); err != nil {
			metrics.RecordInspectionSwept(metrics.InspectionSweepPieceScreenshot, metrics.InspectionSweepOutcomeError)
			if logf != nil {
				logf("inspection sweep: delete screenshot blob failed lease=%s blob=%s: %v", leaseID, row.ScreenshotBlobPath, err)
			}
		} else {
			metrics.RecordInspectionSwept(metrics.InspectionSweepPieceScreenshot, metrics.InspectionSweepOutcomeOK)
		}
	}
}

