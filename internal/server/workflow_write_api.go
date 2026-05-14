package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/nelsong6/glimmung/internal/domain/budget"
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
	TriggerLabel        *string        `json:"trigger_label"`
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
			writeProblem(w, http.StatusInternalServerError, "read project failed")
			return
		}
		if !ok {
			writeProblem(w, http.StatusBadRequest, "project "+req.Project+" does not exist; register it first")
			return
		}
		normalizeWorkflowRegisterForProject(&req, project)
		if err := validateWorkflowAllowedForProject(project, req); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateMandatoryPhases(req); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		workflow, err := writer.UpsertWorkflow(r.Context(), req)
		if validationErr, ok := err.(ValidationError); ok {
			writeProblem(w, http.StatusBadRequest, validationErr.Message)
			return
		}
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "register workflow failed")
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
			writeProblem(w, http.StatusInternalServerError, "patch workflow failed")
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
			writeProblem(w, http.StatusInternalServerError, "delete workflow failed")
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

func validateMandatoryPhases(req WorkflowRegister) error {
	hasEntry := false
	hasTesting := false
	hasCleanup := false
	for _, phase := range req.Phases {
		if len(phase.DependsOn) == 0 {
			hasEntry = true
		}
		if phase.Verify || phase.EvidenceVerificationGate {
			hasTesting = true
		}
		if phase.Always {
			hasCleanup = true
		}
	}
	if hasEntry && hasTesting && hasCleanup {
		return nil
	}
	return ValidationError{Message: "workflow " + req.Name + " is missing required phases"}
}
