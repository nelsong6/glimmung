package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/auth"
)

type fakePlaybookStore struct {
	fakeReadStore
	rows   []PlaybookPublic
	detail PlaybookPublic
	err    error
}

func (s *fakePlaybookStore) ListPlaybooks(_ context.Context, _ PlaybookListFilter) ([]PlaybookPublic, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *fakePlaybookStore) GetPlaybook(_ context.Context, _, _ string) (PlaybookPublic, error) {
	if s.err != nil {
		return PlaybookPublic{}, s.err
	}
	return s.detail, nil
}

func (s *fakePlaybookStore) PatchPlaybookEntryGate(_ context.Context, _, _, _ string, _ bool) (PlaybookPublic, error) {
	if s.err != nil {
		return PlaybookPublic{}, s.err
	}
	return s.detail, nil
}

func (s *fakePlaybookStore) CreatePlaybook(_ context.Context, req PlaybookCreate) (PlaybookPublic, error) {
	if s.err != nil {
		return PlaybookPublic{}, s.err
	}
	return PlaybookPublic{
		SchemaVersion: 1,
		Ref:           "my-playbook-20260101000000",
		Project:       req.Project,
		Title:         req.Title,
		State:         "draft",
		Entries:       []PlaybookEntryPublic{},
		Metadata:      map[string]any{},
	}, nil
}

func TestListPlaybooks(t *testing.T) {
	store := &fakePlaybookStore{rows: []PlaybookPublic{{
		SchemaVersion: 1,
		Ref:           "fix-dashboard-20260101120000",
		Project:       "glimmung",
		Title:         "Fix dashboard",
		State:         "draft",
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/playbooks?project=glimmung", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ref":"fix-dashboard-20260101120000"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGetPlaybook(t *testing.T) {
	store := &fakePlaybookStore{detail: PlaybookPublic{
		SchemaVersion: 1,
		Ref:           "fix-dashboard-20260101120000",
		Project:       "glimmung",
		Title:         "Fix dashboard",
		State:         "draft",
	}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/playbooks/glimmung/fix-dashboard-20260101120000", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"Fix dashboard"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGetPlaybookNotFound(t *testing.T) {
	handler := NewWithStore(Settings{}, &fakePlaybookStore{err: ErrNotFound})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/playbooks/glimmung/nonexistent", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreatePlaybook(t *testing.T) {
	store := &fakePlaybookStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	rec := httptest.NewRecorder()
	body := `{"project":"glimmung","title":"My Playbook","entries":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/playbooks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"My Playbook"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreatePlaybookValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakePlaybookStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	cases := []struct {
		body   string
		status int
		desc   string
	}{
		{`{"title":"t"}`, http.StatusBadRequest, "missing project"},
		{`{"project":"p"}`, http.StatusBadRequest, "missing title"},
		{`{"project":"p","title":"t","entries":[{"id":"a","issue":{"title":"x"}},{"id":"a","issue":{"title":"y"}}]}`, http.StatusUnprocessableEntity, "duplicate entry IDs"},
		{`{"project":"p","title":"t","entries":[{"id":"a","issue":{"title":"x"},"depends_on":["missing"]}]}`, http.StatusUnprocessableEntity, "unknown dep"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/playbooks", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestPlaybookRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	for _, path := range []string{"/v1/playbooks", "/v1/playbooks/glimmung/foo"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

type fakePlayableStore struct {
	fakePlaybookStore
	dispatchResult PlaybookEntryDispatchResult
}

func (s *fakePlayableStore) AdvancePlaybook(ctx context.Context, project, ref string, dispatch PlaybookEntryDispatcher) (PlaybookPublic, error) {
	result, err := dispatch(ctx, PlaybookEntryDispatch{
		Project:             project,
		PlaybookID:          "pb-1",
		PlaybookRef:         ref,
		EntryID:             "entry-1",
		IntegrationStrategy: "isolated_prs",
		WorkContext:         map[string]string{"branch": "glimmung/playbooks/pb-1/entry-1"},
		Issue: PlaybookIssueSpec{
			Title: "entry work",
			Body:  "do the work",
		},
	})
	if err != nil {
		return PlaybookPublic{}, err
	}
	s.dispatchResult = result
	return PlaybookPublic{
		SchemaVersion: 1,
		Ref:           ref,
		Project:       project,
		Title:         "playbook",
		State:         "running",
		Entries: []PlaybookEntryPublic{{
			ID:              "entry-1",
			State:           "running",
			CreatedIssueRef: result.CreatedIssueRef,
			RunRef:          result.RunRef,
		}},
	}, nil
}

func (s *fakePlayableStore) AdvancePlaybooksForRun(context.Context, string, string, PlaybookEntryDispatcher) error {
	return nil
}

func (s *fakePlayableStore) ListIssues(context.Context, IssueListFilter) ([]IssueRow, error) {
	return nil, nil
}

func (s *fakePlayableStore) GetIssueDetailByNumber(context.Context, string, int) (IssueDetail, error) {
	return IssueDetail{}, nil
}

func (s *fakePlayableStore) ArchiveIssueByNumber(context.Context, IssueArchive) (IssueDetail, error) {
	return IssueDetail{}, nil
}

func (s *fakePlayableStore) CreateIssue(context.Context, IssueCreate) (IssueDetail, error) {
	number := 7
	return IssueDetail{Ref: "glimmung#7", Project: "glimmung", Number: &number, Title: "entry work"}, nil
}

func (s *fakePlayableStore) PatchIssueByNumber(context.Context, IssuePatch) (IssueDetail, error) {
	return IssueDetail{}, nil
}

func (s *fakePlayableStore) AddIssueComment(context.Context, IssueCommentAdd) (IssueComment, error) {
	return IssueComment{}, nil
}

func (s *fakePlayableStore) UpdateIssueComment(context.Context, IssueCommentUpdate) (IssueComment, error) {
	return IssueComment{}, nil
}

func (s *fakePlayableStore) DeleteIssueComment(context.Context, IssueCommentDelete) (IssueDetail, error) {
	return IssueDetail{}, nil
}

func (s *fakePlayableStore) ReadProjectGitHubRepo(context.Context, string) (string, error) {
	return "owner/repo", nil
}

func (s *fakePlayableStore) ReadIssueForDispatch(context.Context, string, int) (IssueDispatchData, error) {
	return IssueDispatchData{ID: "issue-1", Title: "entry work", Body: "do the work"}, nil
}

func (s *fakePlayableStore) GetWorkflowByName(context.Context, string, string) (*Workflow, error) {
	return &Workflow{Name: "agent", Project: "glimmung", Phases: playbookTestWorkflowPhases()}, nil
}

func (s *fakePlayableStore) ListProjectWorkflows(context.Context, string) ([]Workflow, error) {
	return []Workflow{{Name: "agent", Project: "glimmung", Phases: playbookTestWorkflowPhases()}}, nil
}

func playbookTestWorkflowPhases() []PhaseSpec {
	return []PhaseSpec{
		{Name: "prep", Kind: "k8s_job", Jobs: []NativeJobSpec{{ID: "prep"}}},
		{Name: "verify", Kind: "k8s_job", Verify: true, DependsOn: []string{"prep"}, Jobs: []NativeJobSpec{{ID: "verify"}}},
		{Name: "cleanup", Kind: "k8s_job", Always: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
	}
}

func (s *fakePlayableStore) ClaimIssueLock(context.Context, string, int, string, int) error {
	return nil
}

func (s *fakePlayableStore) ReleaseIssueLock(context.Context, string, int, string) {}

func (s *fakePlayableStore) CreateRun(context.Context, CreateRunRequest) (CreatedRun, error) {
	return CreatedRun{ID: "run-1", RunNumber: 1, CycleNumber: 1, RunCycle: 1, RunDisplay: "1.1", CallbackToken: "tok"}, nil
}

func (s *fakePlayableStore) StartRunCycle(context.Context, StartRunCycleRequest) (int, error) {
	return 0, nil
}

func (s *fakePlayableStore) AcquireLease(context.Context, LeaseAcquireRequest) (Lease, error) {
	one := 1
	return Lease{Project: "glimmung", LeaseNumber: &one, Host: stringPtr("native-k8s"), State: "claimed", Metadata: map[string]any{"native_k8s": true, "native_slot_name": "slot-1"}}, nil
}

func (s *fakePlayableStore) ReadLeaseByRef(context.Context, string, string) (Lease, error) {
	one := 1
	return Lease{Project: "glimmung", LeaseNumber: &one, Host: stringPtr("native-k8s"), State: "claimed", Metadata: map[string]any{"native_k8s": true, "native_slot_name": "slot-1"}}, nil
}

func (s *fakePlayableStore) CancelLeaseByRef(context.Context, string, string) (CancelLeaseResult, error) {
	return CancelLeaseResult{}, nil
}

func (s *fakePlayableStore) AbortRunByID(context.Context, string, string, string) (AbortRunResult, error) {
	return AbortRunResult{}, nil
}

func TestRunPlaybookDispatchesReadyEntries(t *testing.T) {
	store := &fakePlayableStore{}
	handler := NewWithRuntimeClients(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil, &fakeNativeLauncher{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/playbooks/glimmung/pb-ref/run", nil)
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.dispatchResult.CreatedIssueRef == nil || *store.dispatchResult.CreatedIssueRef != "glimmung#7" {
		t.Fatalf("dispatch result=%#v", store.dispatchResult)
	}
	if store.dispatchResult.RunRef == nil || *store.dispatchResult.RunRef != "glimmung#7/runs/1.1" {
		t.Fatalf("dispatch run ref=%#v", store.dispatchResult.RunRef)
	}
	if !strings.Contains(rec.Body.String(), `"state":"running"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}
