package server

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultPort                = "8000"
	defaultAuthority           = "https://login.microsoftonline.com/common"
	defaultTankOperatorBaseURL = "https://tank.romaine.life"
)

type Settings struct {
	Port                           string
	CosmosEndpoint                 string
	CosmosDatabase                 string
	EntraClientID                  string
	EntraTestClientID              string
	AllowedEmails                  string
	K8sSAAllowlist                 string
	K8sAPIHost                     string
	K8sSATokenPath                 string
	K8sCACertPath                  string
	TankOperatorBaseURL            string
	StaticDir                      string
	StaticOverrideDir              string
	ArtifactsStorageAccount        string
	ArtifactsContainer             string
	NativeRunnerProjectConcurrency int
	GitHubAppID                    string
	GitHubAppInstallationID        string
	GitHubAppPrivateKey            string
	GitHubWebhookSecret            string
}

func SettingsFromEnv() Settings {
	return Settings{
		Port:                envOrDefault("PORT", defaultPort),
		CosmosEndpoint:      os.Getenv("COSMOS_ENDPOINT"),
		CosmosDatabase:      os.Getenv("COSMOS_DATABASE"),
		EntraClientID:       os.Getenv("ENTRA_CLIENT_ID"),
		EntraTestClientID:   os.Getenv("ENTRA_TEST_CLIENT_ID"),
		AllowedEmails:       os.Getenv("ALLOWED_EMAILS"),
		K8sSAAllowlist:      os.Getenv("K8S_SA_ALLOWLIST"),
		K8sAPIHost:          envOrDefault("K8S_API_HOST", "https://kubernetes.default.svc"),
		K8sSATokenPath:      envOrDefault("K8S_SA_TOKEN_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		K8sCACertPath:       envOrDefault("K8S_CA_CERT_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
		TankOperatorBaseURL: envOrDefault("TANK_OPERATOR_BASE_URL", defaultTankOperatorBaseURL),
		StaticDir:           os.Getenv("GLIMMUNG_STATIC_DIR"),
		StaticOverrideDir:   os.Getenv("GLIMMUNG_STATIC_OVERRIDE_DIR"),
		ArtifactsStorageAccount: envOrDefault(
			"ARTIFACTS_STORAGE_ACCOUNT",
			"romaineglimmungartifacts",
		),
		ArtifactsContainer: envOrDefault("ARTIFACTS_CONTAINER", "artifacts"),
		NativeRunnerProjectConcurrency: envIntOrDefault(
			"NATIVE_RUNNER_PROJECT_CONCURRENCY",
			5,
		),
		GitHubAppID:             os.Getenv("GITHUB_APP_ID"),
		GitHubAppInstallationID: os.Getenv("GITHUB_APP_INSTALLATION_ID"),
		GitHubAppPrivateKey:     os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubWebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),
	}
}

func New(settings Settings) http.Handler {
	return NewWithStore(settings, nil)
}

func NewWithStore(settings Settings, store ReadStore) http.Handler {
	return NewWithDependencies(settings, store, nil)
}

// NewWithSyncClient extends NewWithDependencies with an optional GitHub client for workflow sync.
func NewWithSyncClient(settings Settings, store ReadStore, authResolver AuthResolver, ghClient WorkflowSyncClient, artifactStores ...ArtifactStore) http.Handler {
	return newHandler(settings, store, authResolver, ghClient, artifactStores...)
}

func NewWithDependencies(settings Settings, store ReadStore, authResolver AuthResolver, artifactStores ...ArtifactStore) http.Handler {
	return newHandler(settings, store, authResolver, nil, artifactStores...)
}

func newHandler(settings Settings, store ReadStore, authResolver AuthResolver, ghClient WorkflowSyncClient, artifactStores ...ArtifactStore) http.Handler {
	var artifactStore ArtifactStore
	if len(artifactStores) > 0 {
		artifactStore = artifactStores[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /v1/config", publicConfig(settings))
	mux.HandleFunc("GET /v1/auth/me", authMe(authResolver))
	mux.HandleFunc("GET /v1/artifacts/{blob_path...}", readArtifact(artifactStore))
	adminAuthenticator, _ := authResolver.(AdminAuthenticator)
	mux.HandleFunc(
		"GET /v1/issues/by-id/{project}/{issue_id}",
		storageIDGone("Issue storage-ID lookup is disabled; use /v1/issues/by-number/{project}/{issue_number}"),
	)
	mux.HandleFunc("GET /v1/issues", listIssues(store))
	mux.HandleFunc(
		"PATCH /v1/issues/by-id/{project}/{issue_id}",
		storageIDGone("Issue storage-ID mutation is disabled; use /v1/issues/by-number/{project}/{issue_number}"),
	)
	mux.HandleFunc(
		"POST /v1/issues/by-id/{project}/{issue_id}/archive",
		storageIDGone("Issue storage-ID mutation is disabled; use /v1/issues/by-number/{project}/{issue_number}/archive"),
	)
	mux.HandleFunc(
		"POST /v1/issues/by-id/{project}/{issue_id}/discard",
		storageIDGone("Issue storage-ID mutation is disabled; use /v1/issues/by-number/{project}/{issue_number}/discard"),
	)
	mux.HandleFunc(
		"POST /v1/issues/by-id/{project}/{issue_id}/comments",
		storageIDGone("Issue storage-ID comments are disabled; use /v1/issues/by-number/{project}/{issue_number}/comments"),
	)
	mux.HandleFunc(
		"PATCH /v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
		storageIDGone("Issue storage-ID comments are disabled; use /v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}"),
	)
	mux.HandleFunc(
		"DELETE /v1/issues/by-id/{project}/{issue_id}/comments/{comment_id}",
		storageIDGone("Issue storage-ID comments are disabled; use /v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}"),
	)
	mux.HandleFunc(
		"GET /v1/reports/by-id/{project}/{report_id}",
		storageIDGone("touchpoints are no longer addressable by storage id; use /v1/touchpoints/{owner}/{repo}/{pr_number} or /v1/projects/{project}/issues/{issue_number}/touchpoint"),
	)
	mux.HandleFunc(
		"GET /v1/touchpoints/by-id/{project}/{report_id}",
		storageIDGone("touchpoints are no longer addressable by storage id; use /v1/touchpoints/{owner}/{repo}/{pr_number} or /v1/projects/{project}/issues/{issue_number}/touchpoint"),
	)
	mux.HandleFunc(
		"GET /v1/reports/by-id/{project}/{report_id}/versions",
		storageIDGone("touchpoint versions are no longer addressable by storage id"),
	)
	mux.HandleFunc(
		"GET /v1/touchpoints/by-id/{project}/{report_id}/versions",
		storageIDGone("touchpoint versions are no longer addressable by storage id"),
	)
	mux.HandleFunc(
		"GET /v1/reports/by-id/{project}/{report_id}/versions/{version}",
		storageIDGone("touchpoint versions are no longer addressable by storage id"),
	)
	mux.HandleFunc(
		"GET /v1/touchpoints/by-id/{project}/{report_id}/versions/{version}",
		storageIDGone("touchpoint versions are no longer addressable by storage id"),
	)
	mux.HandleFunc(
		"POST /v1/reports/by-id/{project}/{report_id}/versions",
		storageIDGone("touchpoint versions are no longer addressable by storage id"),
	)
	mux.HandleFunc(
		"POST /v1/touchpoints/by-id/{project}/{report_id}/versions",
		storageIDGone("touchpoint versions are no longer addressable by storage id"),
	)
	mux.HandleFunc(
		"PATCH /v1/reports/by-id/{project}/{report_id}",
		storageIDGone("touchpoints are no longer patchable by storage id"),
	)
	mux.HandleFunc(
		"PATCH /v1/touchpoints/by-id/{project}/{report_id}",
		storageIDGone("touchpoints are no longer patchable by storage id"),
	)
	mux.HandleFunc("GET /v1/projects/{project}/runs", listProjectRuns(store))
	mux.HandleFunc(
		"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/report",
		getRunReportByNumber(store),
	)
	mux.HandleFunc("GET /v1/issues/by-number/{project}/{issue_number}", issueDetailByNumber(store))
	mux.HandleFunc(
		"GET /v1/issues/{repo_owner}/{repo_name}/{issue_number}",
		storageIDGone("GitHub Issue lookup is disabled; use /v1/issues/by-number/{project}/{issue_number}"),
	)
	mux.HandleFunc(
		"GET /v1/issues/by-number/{project}/{issue_number}/graph",
		storageIDGone("use /v1/issues/by-number/{project}/{issue_number}/graph is not supported in the Go backend yet"),
	)
	mux.HandleFunc(
		"GET /v1/issues/{repo_owner}/{repo_name}/{issue_number}/graph",
		storageIDGone("GitHub Issue graph lookup is disabled; use /v1/issues/by-number/{project}/{issue_number}/graph"),
	)
	mux.HandleFunc("GET /v1/graph", storageIDGone("system graph is not supported in the Go backend yet"))
	mux.HandleFunc("GET /v1/playbooks", listPlaybooks(store))
	mux.Handle("POST /v1/playbooks", requireAdmin(adminAuthenticator, http.HandlerFunc(createPlaybook(store))))
	mux.HandleFunc("GET /v1/playbooks/{project}/{playbook_ref}", getPlaybook(store))
	mux.HandleFunc("GET /v1/touchpoints", listTouchpoints(store))
	mux.HandleFunc("GET /v1/reports", listTouchpoints(store))
	mux.HandleFunc("GET /v1/touchpoints/{repo_owner}/{repo_name}/{pr_number}", touchpointDetailByRepoPR(store))
	mux.HandleFunc("GET /v1/reports/{repo_owner}/{repo_name}/{pr_number}", touchpointDetailByRepoPR(store))
	mux.HandleFunc("GET /v1/projects/{project}/issues/{issue_number}/touchpoint", issueTouchpointDetail(store))
	mux.Handle("POST /v1/touchpoints", requireAdmin(adminAuthenticator, http.HandlerFunc(createTouchpoint(store))))
	mux.Handle("POST /v1/reports", requireAdmin(adminAuthenticator, http.HandlerFunc(createTouchpoint(store))))
	mux.HandleFunc("GET /v1/projects", listProjects(store))
	mux.Handle("POST /v1/projects", requireAdmin(adminAuthenticator, http.HandlerFunc(registerProject(store))))
	mux.Handle("POST /v1/issues", requireAdmin(adminAuthenticator, http.HandlerFunc(createIssue(store))))
	mux.Handle(
		"PATCH /v1/issues/by-number/{project}/{issue_number}",
		requireAdmin(adminAuthenticator, http.HandlerFunc(patchIssueByNumber(store))),
	)
	mux.Handle(
		"POST /v1/issues/by-number/{project}/{issue_number}/archive",
		requireAdmin(adminAuthenticator, http.HandlerFunc(archiveIssueByNumber(store, "archived"))),
	)
	mux.Handle(
		"POST /v1/issues/by-number/{project}/{issue_number}/discard",
		requireAdmin(adminAuthenticator, http.HandlerFunc(archiveIssueByNumber(store, "discarded"))),
	)
	mux.Handle(
		"POST /v1/issues/by-number/{project}/{issue_number}/comments",
		requireAdmin(adminAuthenticator, http.HandlerFunc(createIssueComment(store))),
	)
	mux.Handle(
		"PATCH /v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}",
		requireAdmin(adminAuthenticator, http.HandlerFunc(updateIssueComment(store))),
	)
	mux.Handle(
		"DELETE /v1/issues/by-number/{project}/{issue_number}/comments/{comment_id}",
		requireAdmin(adminAuthenticator, http.HandlerFunc(deleteIssueComment(store))),
	)
	mux.Handle(
		"PATCH /v1/projects/{project}/test-environments/count",
		requireAdmin(adminAuthenticator, http.HandlerFunc(scaleProjectTestEnvironments(store))),
	)
	mux.HandleFunc("GET /v1/workflows", listWorkflows(store))
	mux.Handle("POST /v1/workflows", requireAdmin(adminAuthenticator, http.HandlerFunc(registerWorkflow(store))))
	mux.Handle("PATCH /v1/workflows/{project}/{name}", requireAdmin(adminAuthenticator, http.HandlerFunc(patchWorkflow(store))))
	mux.Handle("DELETE /v1/workflows/{project}/{name}", requireAdmin(adminAuthenticator, http.HandlerFunc(deleteWorkflow(store))))
	mux.HandleFunc("GET /v1/lease-callbacks/{callback_token}", readLeaseByCallbackToken(store))
	mux.HandleFunc("POST /v1/lease-callbacks/{callback_token}/heartbeat", heartbeatLeaseByCallbackToken(store))
	mux.HandleFunc("POST /v1/lease-callbacks/{callback_token}/release", releaseLeaseByCallbackToken(store))
	mux.HandleFunc("GET /v1/state", stateSnapshot(settings, store))
	mux.HandleFunc("GET /v1/events", stateEvents(settings, store))
	mux.Handle("POST /v1/signals", requireAdmin(adminAuthenticator, http.HandlerFunc(createSignal(store))))
	mux.HandleFunc("GET /v1/portfolio/elements", listPortfolioElements(store))
	mux.Handle("POST /v1/portfolio/elements", requireAdmin(adminAuthenticator, http.HandlerFunc(upsertPortfolioElement(store))))
	mux.Handle("PATCH /v1/portfolio/elements/{project}/{element_ref}", requireAdmin(adminAuthenticator, http.HandlerFunc(patchPortfolioElement(store))))
	mux.Handle("POST /v1/playbooks/{project}/{playbook_ref}/entries/{entry_id}/gate", requireAdmin(adminAuthenticator, http.HandlerFunc(patchPlaybookEntryGate(store))))
	mux.Handle("POST /v1/hosts", requireAdmin(adminAuthenticator, http.HandlerFunc(registerHost(store))))
	mux.Handle("POST /v1/lease", requireAdmin(adminAuthenticator, http.HandlerFunc(createLease(store))))
	mux.Handle("POST /v1/leases/cancel", requireAdmin(adminAuthenticator, http.HandlerFunc(cancelLeaseByRef(store))))
	mux.HandleFunc("GET /v1/projects/{project}/workflows/{name}/upstream", getWorkflowUpstream(store, ghClient))
	mux.Handle("POST /v1/projects/{project}/workflows/{name}/sync", requireAdmin(adminAuthenticator, http.HandlerFunc(syncWorkflow(store, ghClient))))
	mux.Handle("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/abort", requireAdmin(adminAuthenticator, http.HandlerFunc(abortRunByNumber(store))))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/started", runStartedByCallbackToken(store))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/aborted", runAbortedByCallbackToken(store))
	mux.HandleFunc("GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events", nativeRunEventsByNumber(store))
	mux.HandleFunc("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events", nativeRunEventWriteByNumber(store))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/events", nativeRunEventWriteByCallbackToken(store))
	mux.HandleFunc("GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/status", nativeRunStatusByNumber(store))
	mux.HandleFunc("GET /v1/run-callbacks/{callback_token}/native/status", nativeRunStatusByCallbackToken(store))
	mux.HandleFunc("POST /v1/webhook/github", githubWebhook(settings))
	if staticRoots(settings).enabled() {
		mux.HandleFunc("GET /assets/", serveAsset(settings))
		mux.HandleFunc("GET /", serveSPA(settings))
	}
	return rejectUnsafeArtifactPaths(mux)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func publicConfig(settings Settings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := frontendEntraClientID(settings, requestHost(r))
		writeJSON(w, http.StatusOK, map[string]string{
			"entra_client_id":        clientID,
			"authority":              defaultAuthority,
			"tank_operator_base_url": strings.TrimRight(settings.TankOperatorBaseURL, "/"),
		})
	}
}

func requestHost(r *http.Request) string {
	forwarded := r.Header.Get("x-forwarded-host")
	host := forwarded
	if comma := strings.Index(host, ","); comma >= 0 {
		host = host[:comma]
	}
	host = strings.TrimSpace(host)
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end >= 0 {
			return strings.ToLower(strings.TrimPrefix(host[:end], "["))
		}
	}
	if withoutPort, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(withoutPort)
	}
	if colon := strings.Index(host, ":"); colon >= 0 {
		host = host[:colon]
	}
	return strings.ToLower(host)
}

func frontendEntraClientID(settings Settings, host string) string {
	if settings.EntraTestClientID != "" && isDisposableFrontendHost(host) {
		return settings.EntraTestClientID
	}
	return settings.EntraClientID
}

func isDisposableFrontendHost(host string) bool {
	host = strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
	return host == "glimmung.dev.romaine.life" || strings.HasSuffix(host, ".glimmung.dev.romaine.life")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type roots struct {
	override string
	base     string
}

func staticRoots(settings Settings) roots {
	return roots{override: settings.StaticOverrideDir, base: settings.StaticDir}
}

func (r roots) enabled() bool {
	for _, root := range []string{r.override, r.base} {
		if root == "" {
			continue
		}
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func serveAsset(settings Settings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		assetPath := strings.TrimPrefix(r.URL.Path, "/assets/")
		found, ok := staticFile(staticRoots(settings), "assets", filepath.FromSlash(assetPath))
		if !ok {
			http.Error(w, "static asset not found", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, found)
	}
}

func serveSPA(settings Settings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		if rel != "" {
			if found, ok := staticFile(staticRoots(settings), filepath.FromSlash(rel)); ok {
				http.ServeFile(w, r, found)
				return
			}
		}
		index, ok := staticFile(staticRoots(settings), "index.html")
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, index)
	}
}

func staticFile(r roots, parts ...string) (string, bool) {
	for _, root := range []string{r.override, r.base} {
		if root == "" {
			continue
		}
		found, ok := staticFileInRoot(root, parts...)
		if ok {
			return found, true
		}
	}
	return "", false
}

func staticFileInRoot(root string, parts ...string) (string, bool) {
	for _, part := range parts {
		if part == "" || filepath.IsAbs(part) {
			return "", false
		}
		for _, segment := range strings.Split(filepath.Clean(part), string(filepath.Separator)) {
			if segment == ".." {
				return "", false
			}
		}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	candidate := filepath.Join(append([]string{rootAbs}, parts...)...)
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	info, err := os.Stat(candidateAbs)
	if err != nil || info.IsDir() {
		return "", false
	}
	return candidateAbs, true
}

func envOrDefault(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func envIntOrDefault(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
