package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeWorkflowWriteStore struct {
	fakeReadStore
	workflow Workflow
	project  string
	name     string
	upsert   WorkflowRegister
	patchReq WorkflowPatchRequest
	err      error
}

func (s *fakeWorkflowWriteStore) UpsertWorkflow(_ context.Context, req WorkflowRegister) (Workflow, error) {
	s.upsert = req
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func (s *fakeWorkflowWriteStore) DeleteWorkflow(_ context.Context, project string, name string) (Workflow, error) {
	s.project = project
	s.name = name
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func (s *fakeWorkflowWriteStore) PatchWorkflow(_ context.Context, project string, name string, req WorkflowPatchRequest) (Workflow, error) {
	s.project = project
	s.name = name
	s.patchReq = req
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func TestRegisterWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestRegisterWorkflowUpsertsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}},
		workflow: Workflow{
			ID:        "agent-run",
			Project:   "ambience",
			Name:      "agent-run",
			CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
		},
	}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"prep"},{"name":"verify","verify":true,"depends_on":["prep"]},{"name":"cleanup_early","run_on":"always","purpose":"teardown","skip_when_preserve_test_env":true,"depends_on":["verify"]},{"name":"touchpoint","run_on":"success","purpose":"review_touchpoint","depends_on":["cleanup_early"],"jobs":[{"id":"pr-touchpoint","primitive":"pr_touchpoint"}]},{"name":"touchpoint_gate","kind":"touchpoint_gate","depends_on":["touchpoint"],"jobs":[{"id":"pr-merge","primitive":"pr_merge"}]},{"name":"cleanup_final","run_on":"always","purpose":"teardown","depends_on":["touchpoint_gate"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upsert.Project != "ambience" || store.upsert.Name != "agent-run" {
		t.Fatalf("upsert=%#v", store.upsert)
	}
	if len(store.upsert.Phases) != 6 {
		t.Fatalf("phases=%#v", store.upsert.Phases)
	}
	if store.upsert.Phases[0].Kind != "k8s_job" || store.upsert.Phases[0].WorkflowRef != "main" {
		t.Fatalf("phase defaults=%#v", store.upsert.Phases[0])
	}
}

func TestRegisterWorkflowRequiresProject(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, fakeAdminAuthenticator{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestRegisterWorkflowDefaultsBlankKindToK8sJob(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{
		ID:       "glimmung",
		Name:     "glimmung",
		Metadata: map[string]any{"app_type": "native_web_app"},
	}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"glimmung","name":"agent-run","phases":[{"name":"prep"},{"name":"verify","verify":true,"depends_on":["prep"]},{"name":"cleanup_early","run_on":"always","purpose":"teardown","skip_when_preserve_test_env":true,"depends_on":["verify"]},{"name":"touchpoint","run_on":"success","purpose":"review_touchpoint","depends_on":["cleanup_early"],"jobs":[{"id":"pr-touchpoint","primitive":"pr_touchpoint"}]},{"name":"touchpoint_gate","kind":"touchpoint_gate","depends_on":["touchpoint"],"jobs":[{"id":"pr-merge","primitive":"pr_merge"}]},{"name":"cleanup_final","run_on":"always","purpose":"teardown","depends_on":["touchpoint_gate"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upsert.Phases[0].Kind != "k8s_job" || store.upsert.Phases[1].Kind != "k8s_job" || store.upsert.Phases[2].Kind != "k8s_job" {
		t.Fatalf("phase kinds=%#v", store.upsert.Phases)
	}
}

func TestRegisterWorkflowAcceptsParallelJobsInsideStrictPhase(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"prep","jobs":[{"id":"prep"}]},{"name":"work","depends_on":["prep"],"jobs":[{"id":"test-plan"},{"id":"implement"}]},{"name":"verify","verify":true,"depends_on":["work"],"jobs":[{"id":"verify"}]},{"name":"cleanup_early","run_on":"always","purpose":"teardown","skip_when_preserve_test_env":true,"depends_on":["verify"],"jobs":[{"id":"cleanup-early"}]},{"name":"touchpoint","run_on":"success","purpose":"review_touchpoint","depends_on":["cleanup_early"],"jobs":[{"id":"pr-touchpoint","primitive":"pr_touchpoint"}]},{"name":"touchpoint_gate","kind":"touchpoint_gate","depends_on":["touchpoint"],"jobs":[{"id":"pr-merge","primitive":"pr_merge"}]},{"name":"cleanup_final","run_on":"always","purpose":"teardown","depends_on":["touchpoint_gate"],"jobs":[{"id":"cleanup-final"}]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := len(store.upsert.Phases[1].Jobs); got != 2 {
		t.Fatalf("work jobs=%d, want 2", got)
	}
}

func TestValidateWorkflowRegisterAcceptsManagedRunSteps(t *testing.T) {
	req := WorkflowRegister{
		Name: "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{
				ID:      "prep",
				Image:   "runner:latest",
				Managed: true,
				Steps: []NativeStepSpec{{
					Slug: "checkout",
					Run:  "echo ready",
				}},
			}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
	if got := req.Phases[0].Jobs[0].Steps[0].Type; got != "run" {
		t.Fatalf("managed run step type=%q, want run", got)
	}
}

func TestNormalizeWorkflowRegisterCanonicalizesEvidenceGate(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{
				Name:                     "gate",
				EvidenceVerificationGate: true,
				DependsOn:                []string{"verify"},
				Jobs: []NativeJobSpec{{
					ID:      "custom-gate",
					Image:   "python:3.12-slim",
					Command: []string{"python", "-c"},
					Args:    []string{"exit(1)"},
				}},
			},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
	gate := req.Phases[2]
	if len(gate.Jobs) != 1 {
		t.Fatalf("gate jobs=%#v", gate.Jobs)
	}
	job := gate.Jobs[0]
	if job.ID != "custom-gate" || !job.Managed || job.Image != "" || len(job.Command) != 0 || len(job.Args) != 0 {
		t.Fatalf("gate job=%#v", job)
	}
	if len(job.Steps) != 1 || job.Steps[0].Slug != EvidenceGateStepSlug || !strings.Contains(job.Steps[0].Run, "GLIMMUNG_INPUT_VERIFICATION") {
		t.Fatalf("gate steps=%#v", job.Steps)
	}
}

func TestCanonicalWorkflowCanonicalizesDeclaredPRTouchpointPrimitive(t *testing.T) {
	wf := Workflow{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "work", Jobs: []NativeJobSpec{{ID: "work"}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"work"}, Jobs: []NativeJobSpec{
				{ID: "env-destroy"},
				{ID: "publish-pr", Primitive: JobPrimitivePRTouchpoint, Image: "ignored:latest", Command: []string{"ignored"}, TimeoutSeconds: intPtr(60)},
			}},
		},
	}

	got := CanonicalWorkflow(wf)

	if len(got.Phases) != 2 {
		t.Fatalf("phase count=%d, want 2", len(got.Phases))
	}
	cleanup := got.Phases[1]
	if len(cleanup.Jobs) != 2 {
		t.Fatalf("cleanup jobs=%#v", cleanup.Jobs)
	}
	if cleanup.Jobs[0].ID != "env-destroy" || cleanup.Jobs[1].ID != "publish-pr" {
		t.Fatalf("cleanup job ids=%q,%q", cleanup.Jobs[0].ID, cleanup.Jobs[1].ID)
	}
	job := cleanup.Jobs[1]
	if job.ID != "publish-pr" || job.Primitive != JobPrimitivePRTouchpoint || !job.Managed || job.Image != "" || len(job.Command) != 0 {
		t.Fatalf("pr touchpoint job=%#v", job)
	}
	if job.TimeoutSeconds == nil || *job.TimeoutSeconds != 60 {
		t.Fatalf("timeout=%v, want 60", job.TimeoutSeconds)
	}
	if len(job.Steps) != 1 || job.Steps[0].Slug != PRTouchpointStepSlug || !strings.Contains(job.Steps[0].Run, "GLIMMUNG_PR_TOUCHPOINT_URL") {
		t.Fatalf("pr touchpoint job=%#v", job)
	}
}

func TestValidateWorkflowRegisterRequiresPRTouchpoint(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "pr_touchpoint") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want missing pr_touchpoint", err)
	}
}

func TestValidateWorkflowRegisterRequiresPRTouchpointInReviewTouchpointPhase(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "publish", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeWork, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"publish"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "purpose=\"review_touchpoint\"") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want review-touchpoint error", err)
	}
}

func TestValidateWorkflowRegisterRejectsMultiplePrTouchpointJobs(t *testing.T) {
	// Two pr_touchpoint primitives is invalid even without a gate.
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{
				{ID: "publish-pr-a", Primitive: JobPrimitivePRTouchpoint},
				{ID: "publish-pr-b", Primitive: JobPrimitivePRTouchpoint},
			}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly one is required") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want multiple-pr_touchpoint error", err)
	}
}

func TestValidateWorkflowRegisterAcceptsTouchpointGatePhase(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister err=%v, want nil for valid touchpoint_gate shape", err)
	}
}

func TestValidateWorkflowRegisterRejectsTouchpointGateWithoutMergeJob(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"verify"}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must declare exactly one job with primitive \"pr_merge\"") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want missing-pr_merge error", err)
	}
}

func TestValidateWorkflowRegisterRejectsPRMergeOutsideGate(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}, {ID: "rogue-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must live inside a touchpoint_gate phase") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want pr_merge-outside-gate error", err)
	}
}

func TestValidateWorkflowRegisterRejectsTouchpointGateMarkedVerify(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", Verify: true, DependsOn: []string{"prep"}},
			{Name: "verify", Verify: true, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "cannot also be the verify phase") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want verify-conflict error", err)
	}
}

func TestValidateWorkflowRegisterRejectsUnknownPhaseKind(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Kind: "mystery_kind", Jobs: []NativeJobSpec{{ID: "prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	// deliberately do NOT call normalizeWorkflowRegister so the unknown kind survives.

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want unknown-kind error", err)
	}
}

func TestValidateWorkflowRegisterRejectsUnknownJobPrimitive(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep", Primitive: "mystery"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "unknown primitive") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want unknown primitive", err)
	}
}

func TestValidateWorkflowRegisterRejectsInvalidManagedSteps(t *testing.T) {
	base := func(job NativeJobSpec) WorkflowRegister {
		return WorkflowRegister{
			Name: "agent-run",
			Phases: []PhaseSpec{
				{Name: "prep", Jobs: []NativeJobSpec{job}},
				{Name: "verify", Verify: true, DependsOn: []string{"prep"}},
				{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}},
			},
		}
	}
	tests := []struct {
		name string
		job  NativeJobSpec
		want string
	}{
		{
			name: "command",
			job:  NativeJobSpec{ID: "prep", Image: "runner:latest", Managed: true, Command: []string{"bash"}, Steps: []NativeStepSpec{{Slug: "s", Run: "echo ok"}}},
			want: "cannot declare command or args",
		},
		{
			name: "missing run",
			job:  NativeJobSpec{ID: "prep", Image: "runner:latest", Managed: true, Steps: []NativeStepSpec{{Slug: "s"}}},
			want: "is missing run",
		},
		{
			name: "duplicate step",
			job:  NativeJobSpec{ID: "prep", Image: "runner:latest", Managed: true, Steps: []NativeStepSpec{{Slug: "s", Run: "echo one"}, {Slug: "s", Run: "echo two"}}},
			want: "duplicates step",
		},
		{
			name: "unsupported type",
			job:  NativeJobSpec{ID: "prep", Image: "runner:latest", Managed: true, Steps: []NativeStepSpec{{Slug: "s", Type: "agent", Run: "codex"}}},
			want: "unsupported type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base(tt.job)
			normalizeWorkflowRegister(&req)
			err := ValidateWorkflowRegister(req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateWorkflowRegister error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestRegisterWorkflowRejectsNonNativeKind(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{
		ID:       "glimmung",
		Name:     "glimmung",
		Metadata: map[string]any{"native_webapp": true},
	}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"glimmung","name":"agent-run","phases":[{"name":"prep","kind":"container"},{"name":"verify","kind":"k8s_job","verify":true,"depends_on":["prep"]},{"name":"cleanup","kind":"k8s_job","run_on":"always","purpose":"teardown","depends_on":["verify"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegisterWorkflowRequiresMandatoryPhases(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"verify","verify":true}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestRegisterWorkflowRejectsMultipleEntryPhases(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"prep"},{"name":"verify","verify":true},{"name":"cleanup","run_on":"always","purpose":"teardown","depends_on":["verify"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must declare exactly one depends_on") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRegisterWorkflowRejectsEvidenceGateWithoutVerifyProducer(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"prep"},{"name":"gate","evidence_verification_gate":true,"depends_on":["prep"]},{"name":"cleanup","run_on":"always","purpose":"teardown","depends_on":["gate"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verify") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRegisterWorkflowRejectsBadPhaseInputRef(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"prep","outputs":["validation_url"]},{"name":"verify","verify":true,"depends_on":["prep"],"inputs":{"missing":"${{ phases.prep.outputs.nope }}"}},{"name":"cleanup_early","run_on":"always","purpose":"teardown","skip_when_preserve_test_env":true,"depends_on":["verify"]},{"name":"touchpoint","run_on":"success","purpose":"review_touchpoint","depends_on":["cleanup_early"],"jobs":[{"id":"pr-touchpoint","primitive":"pr_touchpoint"}]},{"name":"touchpoint_gate","kind":"touchpoint_gate","depends_on":["touchpoint"],"jobs":[{"id":"pr-merge","primitive":"pr_merge"}]},{"name":"cleanup_final","run_on":"always","purpose":"teardown","depends_on":["touchpoint_gate"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "doesn't declare that output") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestPatchWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{"budget_total":50}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestPatchWorkflowPatchesAndReturnsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{workflow: Workflow{
		ID:        "agent-run",
		Project:   "ambience",
		Name:      "agent-run",
		CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{"budget_total":50}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "ambience" || store.name != "agent-run" {
		t.Fatalf("project=%q name=%q", store.project, store.name)
	}
	if store.patchReq.BudgetTotal == nil || *store.patchReq.BudgetTotal != 50 {
		t.Fatalf("budget_total=%v", store.patchReq.BudgetTotal)
	}
}

func TestPatchWorkflowMapsMissingTo404(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: ErrNotFound},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/missing", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestPatchWorkflowStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: errors.New("boom")},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestDeleteWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestDeleteWorkflowDeletesAndReturnsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{workflow: Workflow{
		ID:        "agent-run",
		Project:   "ambience",
		Name:      "agent-run",
		CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "ambience" || store.name != "agent-run" {
		t.Fatalf("project=%q name=%q", store.project, store.name)
	}
	if !strings.Contains(rec.Body.String(), `"name":"agent-run"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestDeleteWorkflowMapsMissingTo404(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: ErrNotFound},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/missing", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestDeleteWorkflowStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: errors.New("boom")},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// workflowWithJobTimeout produces a valid workflow shape with a single
// configurable timeout on the entry phase's job — used by the timeout
// guardrail tests below.
func workflowWithJobTimeout(timeout *int) WorkflowRegister {
	return WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prep", Jobs: []NativeJobSpec{{ID: "prep", TimeoutSeconds: timeout}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
}

func TestValidateWorkflowRejectsTimeoutBelowFloor(t *testing.T) {
	err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MinNativePhaseJobTimeoutSeconds - 1)))
	if err == nil || !strings.Contains(err.Error(), "below minimum") {
		t.Fatalf("err=%v, want below-minimum rejection", err)
	}
}

func TestValidateWorkflowRejectsTimeoutAboveCeiling(t *testing.T) {
	err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MaxNativePhaseJobTimeoutSeconds + 1)))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("err=%v, want above-maximum rejection", err)
	}
}

func TestValidateWorkflowAcceptsTimeoutAtFloor(t *testing.T) {
	if err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MinNativePhaseJobTimeoutSeconds))); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowAcceptsNilTimeout(t *testing.T) {
	if err := ValidateWorkflowRegister(workflowWithJobTimeout(nil)); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}
