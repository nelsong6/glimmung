package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/publicids"
)

// ErrOutputsMissing is returned when a skipped phase has no captured outputs on the prior run.
var ErrOutputsMissing = errors.New("outputs missing")

// RunForResume holds the prior-run fields needed by the resume dispatch flow.
type RunForResume struct {
	ID               string
	Project          string
	Workflow         string
	State            string
	IssueID          string
	IssueRepo        string
	IssueNumber      int
	ValidationURL    *string
	Budget           budget.Config
	RootRunID        *string
	RunDisplayNumber *string
	IsCycle          bool
	Attempts         []AttemptForResume
}

// AttemptForResume holds the minimal attempt fields needed for resume.
type AttemptForResume struct {
	Phase        string
	PhaseOutputs map[string]string
}

// CreateResumedRunRequest holds all parameters for creating a resumed run.
type CreateResumedRunRequest struct {
	PriorRun          RunForResume
	Workflow          Workflow
	EntrypointPhase   string
	PhaseInputs       map[string]string
	LaunchMetadata    map[string]any
	IssueLockHolderID string
	TriggerSource     map[string]any
}

// ResumeRunRequest is the body for POST …/resume.
type ResumeRunRequest struct {
	EntrypointPhase    string            `json:"entrypoint_phase"`
	EntrypointJobID    *string           `json:"entrypoint_job_id,omitempty"`
	EntrypointStepSlug *string           `json:"entrypoint_step_slug,omitempty"`
	InputOverrides     map[string]string `json:"input_overrides,omitempty"`
	ArtifactRefs       map[string]string `json:"artifact_refs,omitempty"`
	Context            map[string]any    `json:"context,omitempty"`
	TriggerSource      map[string]any    `json:"trigger_source,omitempty"`
}

// PublicResumeResult is the response for POST …/resume.
type PublicResumeResult struct {
	State       string  `json:"state"`
	NewRunRef   *string `json:"new_run_ref,omitempty"`
	PriorRunRef *string `json:"prior_run_ref,omitempty"`
	Lease       *string `json:"lease,omitempty"`
	Host        *string `json:"host,omitempty"`
	Detail      *string `json:"detail,omitempty"`
}

// RunResumeStore provides all store operations needed by the resume handler.
type RunResumeStore interface {
	ReadRunByNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, error)
	ReadRunForResume(ctx context.Context, project, runID string) (RunForResume, error)
	GetWorkflowByName(ctx context.Context, project, name string) (*Workflow, error)
	ClaimIssueLock(ctx context.Context, project string, issueNumber int, holderID string, ttlSeconds int) error
	ReleaseIssueLock(ctx context.Context, project string, issueNumber int, holderID string)
	CreateResumedRun(ctx context.Context, req CreateResumedRunRequest) (CreatedRun, error)
	SubstitutePhaseInputs(phase PhaseSpec, priorOutputs map[string]map[string]string) (map[string]string, error)
	CollectPriorOutputs(attempts []AttemptForResume) map[string]map[string]string
}

// resumeRunHandler handles POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/resume (admin-only).
func resumeRunHandler(store ReadStore, nativeLauncher NativeLauncher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resumeStore, ok := store.(RunResumeStore)
		if !ok || resumeStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "resume store not configured")
			return
		}

		project := r.PathValue("project")
		issueNumberStr := r.PathValue("issue_number")
		runNumberStr := r.PathValue("run_number")
		if project == "" || issueNumberStr == "" || runNumberStr == "" {
			writeProblem(w, http.StatusBadRequest, "project, issue_number, and run_number required")
			return
		}
		issueNumber, err := strconv.Atoi(issueNumberStr)
		if err != nil || issueNumber <= 0 {
			writeProblem(w, http.StatusBadRequest, "invalid issue_number")
			return
		}

		var req ResumeRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.EntrypointPhase == "" {
			writeProblem(w, http.StatusBadRequest, "entrypoint_phase required")
			return
		}

		ctx := r.Context()

		// 1. Resolve run by display number.
		priorRunID, err := resumeStore.ReadRunByNumber(ctx, project, issueNumber, runNumberStr)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, fmt.Sprintf("run %s not found for %s#%d", runNumberStr, project, issueNumber))
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run by number failed")
			return
		}

		// 2. Read prior run.
		priorRun, err := resumeStore.ReadRunForResume(ctx, project, priorRunID)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, fmt.Sprintf("run %s not found", priorRunID))
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read prior run failed")
			return
		}
		if priorRun.State == "in_progress" {
			writeProblem(w, http.StatusConflict, "refusing to resume from an in-progress run; abort the prior run first")
			return
		}

		// 3. Read workflow.
		wf, err := resumeStore.GetWorkflowByName(ctx, project, priorRun.Workflow)
		if err != nil {
			writeInternalError(w, r, err, "read workflow failed")
			return
		}
		if wf == nil {
			writeProblem(w, http.StatusNotFound, fmt.Sprintf("workflow %s/%q no longer registered", project, priorRun.Workflow))
			return
		}

		// 4. Validate entrypoint phase.
		entrypointIndex := -1
		for i, p := range wf.Phases {
			if p.Name == req.EntrypointPhase {
				entrypointIndex = i
				break
			}
		}
		if entrypointIndex < 0 {
			writeProblem(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"entrypoint_phase %q not on workflow %s/%q (phases: %v)",
				req.EntrypointPhase, project, wf.Name, phaseNames(wf.Phases),
			))
			return
		}
		entryPhase := wf.Phases[entrypointIndex]
		phaseKind := workflowPhaseKind(entryPhase.Kind)
		if err := validateNativeWorkflowKind(phaseKind); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if nativeLauncher == nil {
			writeProblem(w, http.StatusServiceUnavailable, "native launcher not configured")
			return
		}

		// 5. Validate step-boundary params (k8s_job only).
		if req.EntrypointJobID != nil || req.EntrypointStepSlug != nil {
			if phaseKind != "k8s_job" {
				writeProblem(w, http.StatusUnprocessableEntity, "step-boundary resume is only valid for k8s_job phases")
				return
			}
			if req.EntrypointJobID != nil {
				jobFound := false
				for _, j := range entryPhase.Jobs {
					if j.ID == *req.EntrypointJobID {
						jobFound = true
						if req.EntrypointStepSlug != nil {
							stepFound := false
							for _, s := range j.Steps {
								if s.Slug == *req.EntrypointStepSlug {
									stepFound = true
									break
								}
							}
							if !stepFound {
								writeProblem(w, http.StatusUnprocessableEntity, fmt.Sprintf(
									"entrypoint_step_slug %q not on job %q", *req.EntrypointStepSlug, *req.EntrypointJobID,
								))
								return
							}
						}
						break
					}
				}
				if !jobFound {
					writeProblem(w, http.StatusUnprocessableEntity, fmt.Sprintf(
						"entrypoint_job_id %q not on phase %q", *req.EntrypointJobID, req.EntrypointPhase,
					))
					return
				}
			}
		}

		// 6. Claim issue lock.
		holderID := newDispatchID()
		if err := resumeStore.ClaimIssueLock(ctx, project, issueNumber, holderID, defaultIssueLockTTLSeconds); err != nil {
			if errors.Is(err, ErrAlreadyRunning) {
				writeProblem(w, http.StatusConflict, err.Error())
				return
			}
			writeInternalError(w, r, err, "claim issue lock failed")
			return
		}

		triggerSource := req.TriggerSource
		if triggerSource == nil {
			triggerSource = map[string]any{}
		}
		if _, ok := triggerSource["kind"]; !ok {
			triggerSource["kind"] = "resume_via_admin_api"
		}
		if _, ok := triggerSource["resumed_from_run_id"]; !ok {
			triggerSource["resumed_from_run_id"] = priorRunID
		}

		// 7. Resolve entrypoint inputs before creating the queued run so
		// admission can launch from durable run state alone.
		issueNum := issueNumber
		issueRef := publicids.IssueRef(project, &issueNum)
		priorRef := publicids.RunRef(project, &issueNum, runNumberStr)
		priorOutputs := resumeStore.CollectPriorOutputs(priorRun.Attempts)
		substituted, subErr := resumeStore.SubstitutePhaseInputs(entryPhase, priorOutputs)
		if subErr != nil {
			resumeStore.ReleaseIssueLock(ctx, project, issueNumber, holderID)
			writeProblem(w, http.StatusUnprocessableEntity, "input substitution failed: "+subErr.Error())
			return
		}
		// Apply input overrides on top of substituted inputs.
		for k, v := range req.InputOverrides {
			substituted[k] = v
		}

		metadata := map[string]any{
			"issue_body":           "",
			"issue_ref":            issueRef,
			"issue_repo":           priorRun.IssueRepo,
			"issue_title":          "",
			"issue_lock_holder_id": holderID,
			"attempt_index":        strconv.Itoa(entrypointIndex),
			"phase_name":           req.EntrypointPhase,
			"issue_number":         strconv.Itoa(issueNumber),
			"phase_inputs":         substituted,
			"native_k8s":           true,
		}
		if req.EntrypointJobID != nil {
			metadata["entrypoint_job_id"] = *req.EntrypointJobID
		}
		if req.EntrypointStepSlug != nil {
			metadata["entrypoint_step_slug"] = *req.EntrypointStepSlug
		}
		if len(req.ArtifactRefs) > 0 {
			metadata["artifact_refs"] = req.ArtifactRefs
		}
		if len(req.Context) > 0 {
			metadata["context"] = req.Context
		}

		// 8. Create the resumed run as queued. The project-run reconciler
		// performs capacity admission and native launch from the run snapshot.
		newRun, err := resumeStore.CreateResumedRun(ctx, CreateResumedRunRequest{
			PriorRun:          priorRun,
			Workflow:          *wf,
			EntrypointPhase:   req.EntrypointPhase,
			PhaseInputs:       substituted,
			LaunchMetadata:    metadata,
			IssueLockHolderID: holderID,
			TriggerSource:     triggerSource,
		})
		if err != nil {
			resumeStore.ReleaseIssueLock(ctx, project, issueNumber, holderID)
			if errors.Is(err, ErrOutputsMissing) {
				writeProblem(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			writeInternalError(w, r, err, "create resumed run failed")
			return
		}

		runRef := publicids.RunRef(project, &issueNum, newRun.RunDisplay)
		newRef := runRef
		result := PublicResumeResult{
			State:       "queued",
			NewRunRef:   &newRef,
			PriorRunRef: &priorRef,
			Detail:      stringPtr("run queued for project admission"),
		}
		wakeRunProjectReconciler(ctx, resumeStore, nativeLauncher, project)

		writeJSON(w, http.StatusOK, result)
	}
}

// phaseNames extracts phase names for error messages.
func phaseNames(phases []PhaseSpec) []string {
	names := make([]string, len(phases))
	for i, p := range phases {
		names[i] = p.Name
	}
	return names
}
