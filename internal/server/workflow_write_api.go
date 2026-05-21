package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nelsong6/glimmung/internal/domain/budget"
	"github.com/nelsong6/glimmung/internal/domain/phaserefs"
)

const workflowKindNativeK8sJob = "k8s_job"

type WorkflowRegisterStore interface {
	UpsertWorkflow(ctx context.Context, req WorkflowRegister) (Workflow, error)
}

type WorkflowDeleteStore interface {
	DeleteWorkflow(ctx context.Context, project string, name string) (Workflow, error)
}

type WorkflowRegister struct {
	Project             string         `json:"project"`
	Name                string         `json:"name"`
	Phases              []PhaseSpec    `json:"phases"`
	PR                  PrPrimitive    `json:"pr"`
	Budget              budget.Config  `json:"budget"`
	DefaultRequirements map[string]any `json:"default_requirements"`
	Metadata            map[string]any `json:"metadata"`
}

type WorkflowPatchStore interface {
	PatchWorkflow(ctx context.Context, project string, name string, req WorkflowPatchRequest) (Workflow, error)
}

type WorkflowPatchRequest struct {
	PREnabled   *bool    `json:"pr_enabled"`
	BudgetTotal *float64 `json:"budget_total"`
}

func registerWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(WorkflowRegisterStore)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow writer not configured")
			return
		}
		var req WorkflowRegister
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		project, ok, err := lookupProject(r.Context(), store, req.Project)
		if err != nil {
			writeInternalError(w, r, err, "read project failed")
			return
		}
		if !ok {
			writeProblem(w, http.StatusBadRequest, "project "+req.Project+" does not exist; register it first")
			return
		}
		normalizeWorkflowRegisterForProject(&req, project)
		if err := ValidateWorkflowRegister(req); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		workflow, err := writer.UpsertWorkflow(r.Context(), req)
		if validationErr, ok := err.(ValidationError); ok {
			writeProblem(w, http.StatusBadRequest, validationErr.Message)
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "register workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func patchWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		patcher, ok := store.(WorkflowPatchStore)
		if !ok || patcher == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow patcher not configured")
			return
		}
		var req WorkflowPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}

		project := r.PathValue("project")
		name := r.PathValue("name")
		workflow, err := patcher.PatchWorkflow(r.Context(), project, name, req)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "patch workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func deleteWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(WorkflowDeleteStore)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow writer not configured")
			return
		}
		project := r.PathValue("project")
		name := r.PathValue("name")
		workflow, err := writer.DeleteWorkflow(r.Context(), project, name)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "delete workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func normalizeWorkflowRegister(req *WorkflowRegister) {
	normalizeWorkflowRegisterWithDefaultKind(req, workflowKindNativeK8sJob)
}

func normalizeWorkflowRegisterForProject(req *WorkflowRegister, project Project) {
	normalizeWorkflowRegisterWithDefaultKind(req, workflowKindNativeK8sJob)
}

func normalizeWorkflowRegisterWithDefaultKind(req *WorkflowRegister, defaultKind string) {
	if strings.TrimSpace(defaultKind) == "" {
		defaultKind = workflowKindNativeK8sJob
	}
	if req.Budget.Total == 0 {
		req.Budget = budget.DefaultConfig()
	}
	req.DefaultRequirements = mapOrEmpty(req.DefaultRequirements)
	req.Metadata = mapOrEmpty(req.Metadata)
	for i := range req.Phases {
		req.Phases[i].Kind = strings.TrimSpace(req.Phases[i].Kind)
		if req.Phases[i].Kind == "" {
			req.Phases[i].Kind = defaultKind
		}
		if req.Phases[i].WorkflowRef == "" {
			req.Phases[i].WorkflowRef = "main"
		}
		if req.Phases[i].Inputs == nil {
			req.Phases[i].Inputs = map[string]string{}
		}
		req.Phases[i].Outputs = sliceOrEmpty(req.Phases[i].Outputs)
		req.Phases[i].DependsOn = sliceOrEmpty(req.Phases[i].DependsOn)
		req.Phases[i].Jobs = sliceOrEmpty(req.Phases[i].Jobs)
		for j := range req.Phases[i].Jobs {
			job := &req.Phases[i].Jobs[j]
			job.Command = sliceOrEmpty(job.Command)
			job.Args = sliceOrEmpty(job.Args)
			if job.Env == nil {
				job.Env = map[string]string{}
			}
			job.Steps = sliceOrEmpty(job.Steps)
			job.ExtraCheckouts = sliceOrEmpty(job.ExtraCheckouts)
			for k := range job.Steps {
				step := &job.Steps[k]
				step.Type = strings.TrimSpace(step.Type)
				if step.Type == "" && strings.TrimSpace(step.Run) != "" {
					step.Type = "run"
				}
				if step.Env == nil {
					step.Env = map[string]string{}
				}
			}
		}
	}
}

func lookupProject(ctx context.Context, store ReadStore, name string) (Project, bool, error) {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return Project{}, false, err
	}
	for _, project := range projects {
		if firstNonEmpty(project.Name, project.ID) == name {
			return project, true, nil
		}
	}
	return Project{}, false, nil
}

func validateWorkflowAllowedForProject(project Project, req WorkflowRegister) error {
	for _, phase := range req.Phases {
		if err := validateNativeWorkflowKind(phase.Kind); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWorkflowRegister enforces the persisted workflow graph contract.
func ValidateWorkflowRegister(req WorkflowRegister) error {
	if len(req.Phases) == 0 {
		return ValidationError{Message: "workflow " + req.Name + " is missing required phases: prepare, testing, cleanup"}
	}
	if err := validateWorkflowAllowedForProject(Project{}, req); err != nil {
		return err
	}
	phaseRefs := make([]phaserefs.Phase, 0, len(req.Phases))
	phaseNames := map[string]int{}
	hasTesting := false
	hasCleanup := false
	for i, phase := range req.Phases {
		name := strings.TrimSpace(phase.Name)
		if name == "" {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase[%d] is missing name", req.Name, i)}
		}
		if prev, ok := phaseNames[name]; ok {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q duplicates phase[%d]", req.Name, name, prev)}
		}
		phaseNames[name] = i
		if phase.Verify {
			hasTesting = true
		}
		if phase.Always {
			hasCleanup = true
			if len(phase.Inputs) > 0 {
				return ValidationError{Message: fmt.Sprintf("workflow %s always phase %q cannot declare inputs; cleanup must be abort-safe", req.Name, name)}
			}
		}
		if len(phase.Jobs) > 0 {
			seenJobs := map[string]int{}
			for j, job := range phase.Jobs {
				jobID := strings.TrimSpace(job.ID)
				if jobID == "" {
					return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job[%d] is missing id", req.Name, name, j)}
				}
				if prev, ok := seenJobs[jobID]; ok {
					return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q duplicates job[%d]", req.Name, name, jobID, prev)}
				}
				seenJobs[jobID] = j
				if err := validateNativeJobSpec(req.Name, name, j, job); err != nil {
					return err
				}
			}
		}
		if i == 0 {
			if len(phase.DependsOn) != 0 {
				return ValidationError{Message: fmt.Sprintf("workflow %s entry phase %q must not declare depends_on", req.Name, name)}
			}
		} else {
			if len(phase.DependsOn) != 1 {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q must declare exactly one depends_on entry", req.Name, name)}
			}
			want := req.Phases[i-1].Name
			if phase.DependsOn[0] != want {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q depends_on must be [%q]", req.Name, name, want)}
			}
		}
		phaseRefs = append(phaseRefs, phaserefs.Phase{
			Name:    name,
			Inputs:  phase.Inputs,
			Outputs: phase.Outputs,
		})
	}
	if !hasTesting || !hasCleanup {
		missing := make([]string, 0, 2)
		if !hasTesting {
			missing = append(missing, "testing")
		}
		if !hasCleanup {
			missing = append(missing, "cleanup")
		}
		return ValidationError{Message: "workflow " + req.Name + " is missing required phases: " + strings.Join(missing, ", ")}
	}
	if err := phaserefs.Validate(phaseRefs); err != nil {
		return ValidationError{Message: err.Error()}
	}
	return nil
}

func validateNativeJobSpec(workflowName, phaseName string, jobIndex int, job NativeJobSpec) error {
	if job.Managed {
		if len(job.Command) > 0 || len(job.Args) > 0 {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q is managed and cannot declare command or args", workflowName, phaseName, job.ID)}
		}
	}
	seenSteps := map[string]int{}
	for i, step := range job.Steps {
		slug := strings.TrimSpace(step.Slug)
		if slug == "" {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step[%d] is missing slug", workflowName, phaseName, job.ID, i)}
		}
		if prev, ok := seenSteps[slug]; ok {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q duplicates step[%d]", workflowName, phaseName, job.ID, slug, prev)}
		}
		seenSteps[slug] = i
		if !job.Managed {
			continue
		}
		stepType := strings.TrimSpace(step.Type)
		if stepType == "" {
			stepType = "run"
		}
		if stepType != "run" {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q uses unsupported type %q", workflowName, phaseName, job.ID, slug, stepType)}
		}
		if strings.TrimSpace(step.Run) == "" {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q is missing run", workflowName, phaseName, job.ID, slug)}
		}
	}
	return nil
}

func workflowPhaseKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return workflowKindNativeK8sJob
	}
	return kind
}

func validateNativeWorkflowKind(kind string) error {
	if workflowPhaseKind(kind) == workflowKindNativeK8sJob {
		return nil
	}
	return ValidationError{Message: "workflow phases must use kind='k8s_job'"}
}

func projectRequiresNativeWorkflows(project Project) bool {
	metadata := project.Metadata
	if boolFromMap(metadata, "native_webapp") || boolFromMap(metadata, "nativeWebapp") {
		return true
	}
	kind := strings.ToLower(firstNonEmpty(
		stringValue(metadata["app_kind"]),
		stringValue(metadata["appKind"]),
		stringValue(metadata["app_type"]),
		stringValue(metadata["appType"]),
		stringValue(metadata["kind"]),
	))
	return isNativeWebappKind(kind)
}

func isNativeWebappKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "native_webapp", "native-webapp", "native webapp",
		"native_web_app", "native-web-app", "native web app":
		return true
	default:
		return false
	}
}
