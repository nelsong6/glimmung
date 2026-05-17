package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/domain/hotswap"
)

// applyHotSwapTimeoutDefault is the server-side default when the caller
// doesn't specify timeout_seconds. The build-and-swap operation should
// complete in 30-90s in practice; 120s gives buffer for network jitter
// and slow image pulls on the first cold start.
const applyHotSwapTimeoutDefault = 120 * time.Second

// applyHotSwapTimeoutMax is the hard server cap. The caller can ask for
// less; they can't ask for more. This keeps a single bad caller from
// holding a request open indefinitely.
const applyHotSwapTimeoutMax = 600 * time.Second

// applyHotSwapPerformer is the function-typed seam the test harness
// uses to inject a stub. Production wires this to ApplyHotSwap with a
// real httpK8sJobClient.
type applyHotSwapPerformer func(ctx context.Context, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error)

type TestSlotApplyHotSwapRequest struct {
	Project        string  `json:"project"`
	SlotIndex      *int    `json:"slot_index,omitempty"`
	SlotName       *string `json:"slot_name,omitempty"`
	ArtifactKind   string  `json:"artifact_kind"`
	GitRef         string  `json:"git_ref"`
	TimeoutSeconds *int    `json:"timeout_seconds,omitempty"`
}

type TestSlotApplyHotSwapResult struct {
	Lease string                      `json:"lease"`
	Apply ApplyHotSwapResult          `json:"apply"`
	Entry TestSlotHotSwapHistoryEntry `json:"history_entry"`
}

// applyTestSlotHotSwap is the developer-driven build-and-swap endpoint.
// Sync UX per the ArgoCD `app sync` pattern (researched against
// Google AIP-151; ArgoCD is the closer analog for developer-driven k8s
// deploys). Blocks until the dispatched Job completes or the timeout
// elapses. Records hot-swap history on every outcome.
//
// Caller flow:
//
//  1. POST { project, slot_index|slot_name, artifact_kind, git_ref, timeout_seconds }
//  2. Endpoint resolves the active test-slot lease for project+slot.
//  3. Endpoint reads the project's hot-swap contract from metadata.
//  4. Endpoint validates artifact_kind is supported (v1: agent_runner)
//     and the relevant builder_image is present.
//  5. Endpoint dispatches a build-and-swap Job via ops.ApplyHotSwap,
//     blocks on completion.
//  6. Endpoint appends a hot-swap history entry (success or failure).
//  7. Endpoint returns the structured result.
//
// Hot-swap history is appended on EVERY outcome — durable state lives
// in the system, not in the request body. A caller that disconnects
// mid-request can re-query the lease history to see the result.
func applyTestSlotHotSwap(store ReadStore, performer applyHotSwapPerformer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(TestSlotHotSwapHistoryStore)
		stateStore, hasState := store.(StateStore)
		if !ok || writer == nil || !hasState || stateStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot hot-swap history store not configured")
			return
		}
		var req TestSlotApplyHotSwapRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Project = strings.TrimSpace(req.Project)
		req.ArtifactKind = strings.TrimSpace(req.ArtifactKind)
		req.GitRef = strings.TrimSpace(req.GitRef)
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if req.ArtifactKind == "" {
			writeProblem(w, http.StatusBadRequest, "artifact_kind required")
			return
		}
		if req.GitRef == "" {
			writeProblem(w, http.StatusBadRequest, "git_ref required")
			return
		}

		// Timeout: clamp caller-requested to [1s, applyHotSwapTimeoutMax];
		// default to applyHotSwapTimeoutDefault when unset. The clamping
		// happens here (server-side) so a caller asking for "8 hours"
		// can't hold a connection open beyond the hard cap.
		timeout := applyHotSwapTimeoutDefault
		if req.TimeoutSeconds != nil {
			if *req.TimeoutSeconds <= 0 {
				timeout = applyHotSwapTimeoutDefault
			} else if time.Duration(*req.TimeoutSeconds)*time.Second > applyHotSwapTimeoutMax {
				timeout = applyHotSwapTimeoutMax
			} else {
				timeout = time.Duration(*req.TimeoutSeconds) * time.Second
			}
		}

		// Resolve lease. Reuse the existing helper so the slot-index/name
		// resolution mirrors the record-history endpoint.
		lease, err := resolveTestSlotLease(r, stateStore, TestSlotReturnRequest{
			Project:   req.Project,
			SlotIndex: req.SlotIndex,
			SlotName:  req.SlotName,
		})
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "test slot lease not found")
				return
			}
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		leaseRef := LeasePublicRefFromLease(lease)
		slotName, _ := stringFromMap(lease.Metadata, "native_slot_name")
		if strings.TrimSpace(slotName) == "" {
			writeProblem(w, http.StatusBadRequest, "lease has no native_slot_name (cannot derive target namespace)")
			return
		}

		// Resolve the project + contract. ListProjects matches by name +
		// limit=10 so a small typo doesn't fail silently (the historic
		// pattern in record-history); we filter for an exact name match.
		projects, err := store.ListProjects(r.Context())
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "list projects: "+err.Error())
			return
		}
		var project Project
		for _, p := range projects {
			if p.Name == req.Project {
				project = p
				break
			}
		}
		if project.Name == "" {
			writeProblem(w, http.StatusNotFound, "project not found")
			return
		}
		contract, ok, err := hotswap.FromMetadata(project.Metadata)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid hot-swap contract: "+err.Error())
			return
		}
		if !ok || !contract.Enabled {
			writeProblem(w, http.StatusUnprocessableEntity, "project has no enabled test_slot_hot_swap contract")
			return
		}

		// Backend builder_image is optional at Validate time (existing
		// registered contracts predate the field), so the apply endpoint
		// validates it here at request time when artifact_kind=backend
		// is invoked. AgentRunner builder_image is required at Validate
		// time (the only consumer of agent_runner is the apply endpoint;
		// no legacy CLI fallback).
		if req.ArtifactKind == "backend" {
			if strings.TrimSpace(contract.Backend.BuilderImage) == "" {
				writeProblem(w, http.StatusUnprocessableEntity, "contract.backend.builder_image required for apply endpoint (request-time check; the legacy CLI path doesn't need it)")
				return
			}
		}

		// Target namespace convention: `<slot_name>-sessions`. Tank-operator
		// session pods live in tank-operator-slot-1-sessions; any project
		// that opts into session-pod hot-swap follows the same convention.
		// If a future project needs a different namespace, extend the
		// contract; for v1 the convention is sufficient.
		targetNamespace := slotName + "-sessions"

		// RepoURL: derive from project.github_repo. Form: https://github.com/<repo>.git
		repoURL := ""
		if strings.TrimSpace(project.GitHubRepo) != "" {
			repoURL = "https://github.com/" + strings.TrimSpace(project.GitHubRepo) + ".git"
		}

		// Pod selector: the contract's agent_runner.pod_selector names
		// the DIMENSION (e.g., "tank-operator/session-id"); the caller
		// The pod selector flows into the Job's swap script which
		// resolves target pods at run-time via kubectl inside the
		// bitnami/kubectl container — no kubectl needed in the glimmung
		// pod. (Earlier cut resolved pods up-front in the handler and
		// hit "kubectl: not found" in the glimmung runtime image.)

		ctx := r.Context()
		applyResult, applyErr := performer(ctx, ApplyHotSwapOptions{
			Project:         req.Project,
			ArtifactKind:    req.ArtifactKind,
			GitRef:          req.GitRef,
			RepoURL:         repoURL,
			TargetNamespace: targetNamespace,
			Contract:        contract,
			Timeout:         timeout,
		})

		// Record hot-swap history on EVERY outcome. The history entry's
		// status mirrors applyResult.Outcome — durable state in the
		// system, regardless of whether the request succeeded.
		status := applyResult.Outcome
		if status == "" {
			status = "swap_failed"
		}
		summary := fmt.Sprintf("apply_hot_swap kind=%s git_ref=%s outcome=%s", req.ArtifactKind, req.GitRef, status)
		diagnostics := map[string]any{
			"build_logs_tail": applyResult.BuildLogsTail,
			"swap_logs_tail":  applyResult.SwapLogsTail,
		}
		if applyResult.Error != "" {
			diagnostics["error"] = applyResult.Error
		}
		entry := TestSlotHotSwapHistoryEntry{
			Operation:   "apply_hot_swap",
			Status:      status,
			Summary:     summary,
			Diagnostics: diagnostics,
			Timings:     applyResult.Timings,
			CreatedAt:   time.Now().UTC(),
		}
		if _, histErr := writer.AppendTestSlotHotSwapHistory(ctx, req.Project, leaseRef, entry); histErr != nil {
			// History write failed — log the apply outcome in the body
			// even so. The history failure isn't load-bearing for the
			// caller (they still get the result); it is load-bearing
			// for later operators inspecting the lease. We return 200
			// with the apply result either way.
			diagnostics["history_write_error"] = histErr.Error()
		}

		if applyErr != nil {
			// Apply failed — return 200 with the structured result so
			// the caller (MCP tool wrapper) can present the failure
			// cleanly. The Outcome field encodes the failure mode.
			writeJSON(w, http.StatusOK, TestSlotApplyHotSwapResult{
				Lease: leaseRef,
				Apply: applyResult,
				Entry: entry,
			})
			return
		}
		writeJSON(w, http.StatusOK, TestSlotApplyHotSwapResult{
			Lease: leaseRef,
			Apply: applyResult,
			Entry: entry,
		})
	}
}
