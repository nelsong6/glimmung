package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nelsong6/glimmung/internal/metrics"
)

const (
	defaultPort                  = "8000"
	defaultAuthURL               = "https://auth.romaine.life"
	defaultTankOperatorBaseURL   = "https://tank.romaine.life"
	defaultGrafanaBaseURL        = "https://grafana.romaine.life"
	defaultGrafanaLokiDatasource = "loki"
)

type Settings struct {
	Port string
	// Postgres connection settings. The pool is constructed in
	// cmd/glimmung-go/main.go and RunMigrations applies the schema.
	// All durable runtime reads and writes flow through Postgres.
	PostgresHost        string
	PostgresDatabase    string
	PostgresUsername    string
	K8sSAAllowlist      string
	K8sAPIHost          string
	K8sSATokenPath      string
	K8sCACertPath       string
	TankOperatorBaseURL string
	// GrafanaBaseURL is the base URL of the cluster Grafana installation
	// (e.g. https://grafana.romaine.life). The frontend uses this plus the
	// Loki datasource UID to render Explore deep-links from run-report
	// step rows so operators do not have to discover by themselves that
	// the data is in Loki. Empty disables the affordance.
	GrafanaBaseURL string
	// GrafanaLokiDatasource is the datasource name (or UID) the Explore
	// link should target. Grafana resolves a name to a UID; both work.
	GrafanaLokiDatasource              string
	StaticDir                          string
	StaticOverrideDir                  string
	ArtifactsStorageAccount            string
	ArtifactsContainer                 string
	NativeRunnerNamespace              string
	NativeRunnerServiceAccount         string
	NativeRunnerNamespaceRole          string
	NativeRunnerCallbackBaseURL        string
	NativeRunnerJobTTLSeconds          int
	NativeRunnerImage                  string
	NativeRunnerEntrypoint             string
	NativeRunnerCodexSecret            string
	NativeRunnerCodexMountPath         string
	NativeRunnerPlaywrightEnabled      bool
	NativeRunnerPlaywrightImage        string
	NativeRunnerPlaywrightPort         string
	NativeRunnerProjectConcurrency     int
	NativeRunnerDispatchTimeoutSeconds int
	NativeWorkloadIdentityIssuer       string
	// AuthRomaineLifeBaseURL is the base URL of the auth.romaine.life
	// admin API used by ManagedOriginService. Empty disables the
	// reconciler; only useful for local dev / smoke runs.
	AuthRomaineLifeBaseURL string
	// AuthRomaineLifeTokenPath is the path to a projected k8s
	// ServiceAccount token with audience = AuthRomaineLifeBaseURL. The
	// chart mounts this at /var/run/secrets/auth.romaine.life/token.
	//
	// The mounted token is NOT a Glimmung-acceptable bearer JWT — it is
	// a k8s SA token whose only legitimate use is as Authorization
	// against auth.romaine.life itself (ManagedOriginService uses it to
	// call /api/admin/origins/ here). To obtain a JWT that Glimmung's
	// RomaineLifeJWTVerifier accepts (role=service, signed by
	// auth.romaine.life's JWKS), exchange this SA token at
	//   POST {AuthRomaineLifeBaseURL}/api/auth/exchange/k8s
	// and present the returned `token` as the Bearer on Glimmung
	// requests. The exchange is documented in nelsong6/auth's README.
	AuthRomaineLifeTokenPath string
	GitHubAppID              string
	GitHubAppInstallationID  string
	GitHubAppPrivateKey      string
	GitHubWebhookSecret      string
	// ControlPlaneLoopsEnabled gates every background reconciler and the
	// test-slot recovery sweep started from cmd/glimmung-go/main.go.
	//
	// True (the default) is the prod posture: signal drain, run queue,
	// dispatch-timeout, and the test-slot recovery sweep all run and own
	// the shared runtime state in Postgres + the glimmung-runs namespace.
	//
	// False is the test-slot posture. Test slots run a hot-swappable copy
	// of the glimmung binary against the same Postgres database and the
	// same Kubernetes apiserver as prod. If a slot also ran the control
	// loops, two processes would race on the same rows and Jobs — the
	// slot would mutate real run state, and any new reconciler that
	// touches the prod runtime namespace would either succeed and corrupt
	// state or fail RBAC and emit noise. The k8s/issue chart sets
	// CONTROL_PLANE_LOOPS_ENABLED=false on every per-issue release so a
	// hot-swap can exercise HTTP handlers and code paths without joining
	// the control plane.
	//
	// Any new background reconciler must be started inside this gate.
	ControlPlaneLoopsEnabled bool
	// Remote-host execution primitives (docs/remote-host-execution.md).
	// Empty values disable the corresponding endpoint cleanly (503).
	//
	// TailscaleOIDCClientID is the client ID of the Tailscale "Trust
	// credentials → OIDC" entry that pins this glimmung tenant. Not
	// secret on its own — Tailscale validates the credential by JWT
	// signature, not by possession of this ID. We mint JWTs against
	// `api.tailscale.com/<TailscaleOIDCClientID>` audience, exchange
	// them at Tailscale's OAuth token endpoint via RFC 7523, and use
	// the returned access token to mint tailnet auth keys. The
	// projected SA token + auth.romaine.life federation endpoint
	// (`AuthRomaineLifeBaseURL`, `AuthRomaineLifeTokenPath`) supply the
	// JWT mint step.
	TailscaleOIDCClientID string
	TailscaleTailnet      string
	TailscaleAPIBaseURL   string
}

func SettingsFromEnv() Settings {
	return Settings{
		Port:                  envOrDefault("PORT", defaultPort),
		PostgresHost:          os.Getenv("POSTGRES_HOST"),
		PostgresDatabase:      os.Getenv("POSTGRES_DATABASE"),
		PostgresUsername:      os.Getenv("POSTGRES_USER"),
		K8sSAAllowlist:        os.Getenv("K8S_SA_ALLOWLIST"),
		K8sAPIHost:            envOrDefault("K8S_API_HOST", "https://kubernetes.default.svc"),
		K8sSATokenPath:        envOrDefault("K8S_SA_TOKEN_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		K8sCACertPath:         envOrDefault("K8S_CA_CERT_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
		TankOperatorBaseURL:   envOrDefault("TANK_OPERATOR_BASE_URL", defaultTankOperatorBaseURL),
		GrafanaBaseURL:        envOrDefault("GRAFANA_BASE_URL", defaultGrafanaBaseURL),
		GrafanaLokiDatasource: envOrDefault("GRAFANA_LOKI_DATASOURCE", defaultGrafanaLokiDatasource),
		StaticDir:             os.Getenv("GLIMMUNG_STATIC_DIR"),
		StaticOverrideDir:     os.Getenv("GLIMMUNG_STATIC_OVERRIDE_DIR"),
		ArtifactsStorageAccount: envOrDefault(
			"ARTIFACTS_STORAGE_ACCOUNT",
			"romaineglimmungartifacts",
		),
		ArtifactsContainer: envOrDefault("ARTIFACTS_CONTAINER", "artifacts"),
		NativeRunnerNamespace: envOrDefault(
			"NATIVE_RUNNER_NAMESPACE",
			"glimmung-runs",
		),
		NativeRunnerServiceAccount: envOrDefault(
			"NATIVE_RUNNER_SERVICE_ACCOUNT",
			"glimmung-native-runner",
		),
		NativeRunnerNamespaceRole: envOrDefault(
			"NATIVE_RUNNER_NAMESPACE_ROLE",
			"cluster-admin",
		),
		NativeRunnerCallbackBaseURL: envOrDefault(
			"NATIVE_RUNNER_CALLBACK_BASE_URL",
			"http://glimmung.glimmung.svc.cluster.local",
		),
		NativeRunnerJobTTLSeconds: envIntOrDefault(
			"NATIVE_RUNNER_JOB_TTL_SECONDS",
			259200,
		),
		NativeRunnerImage: envOrDefault(
			"NATIVE_RUNNER_IMAGE",
			"romainecr.azurecr.io/glimmung-native-runner:native-runner",
		),
		NativeRunnerEntrypoint: envOrDefault(
			"NATIVE_RUNNER_ENTRYPOINT",
			"/app/glimmung-native-runner",
		),
		NativeRunnerCodexSecret: envOrDefault(
			"NATIVE_RUNNER_CODEX_CREDENTIALS_SECRET",
			"codex-credentials",
		),
		NativeRunnerCodexMountPath: envOrDefault(
			"NATIVE_RUNNER_CODEX_CREDENTIALS_MOUNT_PATH",
			"/etc/codex-creds",
		),
		NativeRunnerPlaywrightEnabled: envBoolOrDefault(
			"NATIVE_RUNNER_PLAYWRIGHT_ENABLED",
			true,
		),
		NativeRunnerPlaywrightImage: envOrDefault(
			"NATIVE_RUNNER_PLAYWRIGHT_IMAGE",
			"romainecr.azurecr.io/glimmung-slot-playwright:playwright-1.56.1",
		),
		NativeRunnerPlaywrightPort: envOrDefault(
			"NATIVE_RUNNER_PLAYWRIGHT_PORT",
			"3000",
		),
		NativeRunnerProjectConcurrency: envIntOrDefault(
			"NATIVE_RUNNER_PROJECT_CONCURRENCY",
			5,
		),
		NativeRunnerDispatchTimeoutSeconds: envIntOrDefault(
			"NATIVE_RUNNER_DISPATCH_TIMEOUT_SECONDS",
			defaultRunDispatchTimeoutSeconds,
		),
		NativeWorkloadIdentityIssuer: os.Getenv("NATIVE_WORKLOAD_IDENTITY_ISSUER"),
		AuthRomaineLifeBaseURL: envOrDefault(
			"AUTH_ROMAINE_LIFE_BASE_URL",
			defaultAuthURL,
		),
		AuthRomaineLifeTokenPath: envOrDefault(
			"AUTH_ROMAINE_LIFE_TOKEN_PATH",
			"/var/run/secrets/auth.romaine.life/token",
		),
		GitHubAppID:             os.Getenv("GITHUB_APP_ID"),
		GitHubAppInstallationID: os.Getenv("GITHUB_APP_INSTALLATION_ID"),
		GitHubAppPrivateKey:     os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubWebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),
		ControlPlaneLoopsEnabled: envBoolOrDefault(
			"CONTROL_PLANE_LOOPS_ENABLED",
			true,
		),
		TailscaleOIDCClientID: os.Getenv("GLIMMUNG_TAILSCALE_OIDC_CLIENT_ID"),
		TailscaleTailnet:      envOrDefault("GLIMMUNG_TAILSCALE_TAILNET", "-"),
		TailscaleAPIBaseURL:   envOrDefault("GLIMMUNG_TAILSCALE_API_BASE_URL", "https://api.tailscale.com"),
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
	return newHandler(settings, store, authResolver, ghClient, nil, artifactStores...)
}

func NewWithRuntimeClients(settings Settings, store ReadStore, authResolver AuthResolver, ghClient WorkflowSyncClient, nativeLauncher NativeLauncher, artifactStores ...ArtifactStore) http.Handler {
	return newHandlerWithReconcilers(settings, store, authResolver, ghClient, nil, nil, nativeLauncher, artifactStores...)
}

func NewWithRuntimeReconcilers(settings Settings, store ReadStore, authResolver AuthResolver, ghClient WorkflowSyncClient, workloadIdentities NativeWorkloadIdentityReconciler, nativeLauncher NativeLauncher, artifactStores ...ArtifactStore) http.Handler {
	return newHandlerWithReconcilers(settings, store, authResolver, ghClient, workloadIdentities, nil, nativeLauncher, artifactStores...)
}

// NewWithReconcilers extends NewWithRuntimeReconcilers with the
// managed-auth-origins reconciler (glimmung#142 stage 2). Existing callers
// keep working through NewWithRuntimeReconcilers (which passes nil for the
// origins reconciler); new wiring in cmd/glimmung-go uses this.
func NewWithReconcilers(settings Settings, store ReadStore, authResolver AuthResolver, ghClient WorkflowSyncClient, workloadIdentities NativeWorkloadIdentityReconciler, managedOrigins ManagedOriginReconciler, nativeLauncher NativeLauncher, artifactStores ...ArtifactStore) http.Handler {
	return newHandlerWithReconcilers(settings, store, authResolver, ghClient, workloadIdentities, managedOrigins, nativeLauncher, artifactStores...)
}

func NewWithDependencies(settings Settings, store ReadStore, authResolver AuthResolver, artifactStores ...ArtifactStore) http.Handler {
	return newHandler(settings, store, authResolver, nil, nil, artifactStores...)
}

func newHandler(settings Settings, store ReadStore, authResolver AuthResolver, ghClient WorkflowSyncClient, nativeLauncher NativeLauncher, artifactStores ...ArtifactStore) http.Handler {
	return newHandlerWithReconcilers(settings, store, authResolver, ghClient, nil, nil, nativeLauncher, artifactStores...)
}

func newHandlerWithReconcilers(settings Settings, store ReadStore, authResolver AuthResolver, ghClient WorkflowSyncClient, workloadIdentities NativeWorkloadIdentityReconciler, managedOrigins ManagedOriginReconciler, nativeLauncher NativeLauncher, artifactStores ...ArtifactStore) http.Handler {
	var artifactStore ArtifactStore
	if len(artifactStores) > 0 {
		artifactStore = artifactStores[0]
	}
	if writer, ok := artifactStore.(ArtifactWriter); ok {
		SetInspectionSweepArtifactWriter(writer)
		SetRunReconcilerArtifactWriter(writer)
	} else {
		SetInspectionSweepArtifactWriter(nil)
		SetRunReconcilerArtifactWriter(nil)
	}
	var nativeTokenMinter NativeGitHubTokenMinter
	if m, ok := ghClient.(NativeGitHubTokenMinter); ok {
		nativeTokenMinter = m
	}
	var prClient PullRequestClient
	if c, ok := ghClient.(PullRequestClient); ok {
		prClient = c
	}
	var testSlotPreparer TestSlotPreparer
	if p, ok := nativeLauncher.(TestSlotPreparer); ok {
		testSlotPreparer = p
	}
	// Remote-host execution primitives (docs/remote-host-execution.md).
	// Both endpoints fail closed (503) if their upstream wiring is empty.
	// auth.romaine.life is the sole SSH CA issuer: glimmung holds no CA
	// private material and instead exchanges its projected SA token for a
	// signed cert. NewSSHCertExchanger returns errSSHCertGatewayUnconfigured
	// when the auth base URL or SA-token path is empty, which we treat as
	// "endpoint disabled" rather than logging at error.
	sshCertExchanger, sshCertErr := NewSSHCertExchanger(
		settings.AuthRomaineLifeBaseURL,
		settings.AuthRomaineLifeTokenPath,
		nil,
	)
	if sshCertErr != nil && !errors.Is(sshCertErr, errSSHCertGatewayUnconfigured) {
		log.Printf("remote-host: ssh cert gateway disabled: %v", sshCertErr)
	}
	tailscaleAuthKeyMinter, tailscaleErr := NewTailscaleAuthKeyMinter(
		settings.TailscaleAPIBaseURL,
		settings.TailscaleTailnet,
		settings.TailscaleOIDCClientID,
		settings.AuthRomaineLifeBaseURL,
		settings.AuthRomaineLifeTokenPath,
		nil,
	)
	if tailscaleErr != nil &&
		!errors.Is(tailscaleErr, errTailscaleUnconfigured) &&
		!errors.Is(tailscaleErr, errAuthRomaineLifeUnconfigured) {
		log.Printf("remote-host: tailscale auth-key minter disabled: %v", tailscaleErr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(store))
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /v1/config", publicConfig(settings))
	mux.HandleFunc("GET /v1/auth/me", authMe(authResolver))
	mux.HandleFunc("GET /v1/artifacts/{blob_path...}", readArtifact(artifactStore))
	adminAuthenticator, _ := authResolver.(AdminAuthenticator)
	mux.HandleFunc("GET /v1/issues", listIssues(store))
	mux.HandleFunc("GET /v1/projects/{project}/runs", listProjectRuns(store))
	mux.HandleFunc(
		"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/report",
		getRunReportByNumber(store),
	)
	mux.HandleFunc(
		"GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/cycles/{cycle_number}/graph",
		runCycleGraphProjectionByNumber(store),
	)
	mux.HandleFunc("GET /v1/issues/by-number/{project}/{issue_number}", issueDetailByNumber(store))
	mux.HandleFunc(
		"GET /v1/issues/by-number/{project}/{issue_number}/graph",
		issueGraphByNumber(store),
	)
	mux.HandleFunc("GET /v1/graph", systemGraph(store))
	mux.HandleFunc("GET /v1/playbooks", listPlaybooks(store))
	mux.Handle("POST /v1/playbooks", requireAdmin(adminAuthenticator, http.HandlerFunc(createPlaybook(store))))
	mux.HandleFunc("GET /v1/playbooks/{project}/{playbook_ref}", getPlaybook(store))
	mux.HandleFunc("GET /v1/touchpoints", listTouchpoints(store))
	mux.HandleFunc("GET /v1/projects/{project}/issues/{issue_number}/touchpoint", issueTouchpointDetail(store))
	mux.Handle("POST /v1/touchpoints", requireAdmin(adminAuthenticator, http.HandlerFunc(createTouchpoint(store))))
	mux.HandleFunc("GET /v1/projects", listProjects(store))
	mux.Handle("POST /v1/projects", requireAdmin(adminAuthenticator, http.HandlerFunc(registerProject(store, managedOrigins))))
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
		requireAdmin(adminAuthenticator, http.HandlerFunc(scaleProjectTestEnvironments(store, workloadIdentities, managedOrigins, testSlotPreparer, nativeTokenMinter))),
	)
	mux.Handle(
		"POST /v1/projects/{project}/test-environments/{slot_name}/repair",
		requireAdmin(adminAuthenticator, http.HandlerFunc(repairProjectTestEnvironment(store, testSlotPreparer, nativeTokenMinter))),
	)
	mux.HandleFunc("GET /v1/workflows", listWorkflows(store))
	mux.Handle("POST /v1/workflows", requireAdmin(adminAuthenticator, http.HandlerFunc(registerWorkflow(store))))
	mux.Handle("PATCH /v1/workflows/{project}/{name}", requireAdmin(adminAuthenticator, http.HandlerFunc(patchWorkflow(store))))
	mux.Handle("DELETE /v1/workflows/{project}/{name}", requireAdmin(adminAuthenticator, http.HandlerFunc(deleteWorkflow(store))))
	mux.HandleFunc("GET /v1/lease-callbacks/{callback_token}", readLeaseByCallbackToken(store))
	mux.HandleFunc("POST /v1/lease-callbacks/{callback_token}/heartbeat", heartbeatLeaseByCallbackToken(store))
	mux.HandleFunc("POST /v1/lease-callbacks/{callback_token}/release", releaseLeaseByCallbackToken(store, testSlotPreparer))
	mux.HandleFunc("GET /v1/state", stateSnapshot(settings, store))
	mux.HandleFunc("GET /v1/projects/{project}/test-environments/{slot_name}", testEnvironmentStatus(settings, store))
	mux.HandleFunc("GET /v1/events", stateEvents(settings, store))
	mux.Handle("POST /v1/signals", requireAdmin(adminAuthenticator, http.HandlerFunc(createSignal(store))))
	mux.Handle("POST /v1/signals/drain", requireAdmin(adminAuthenticator, http.HandlerFunc(drainSignalsHandler(store, nativeLauncher))))
	mux.HandleFunc("GET /v1/portfolio/elements", listPortfolioElements(store))
	mux.Handle("POST /v1/portfolio/elements", requireAdmin(adminAuthenticator, http.HandlerFunc(upsertPortfolioElement(store))))
	mux.Handle("POST /v1/portfolio/elements/dispatch", requireAdmin(adminAuthenticator, http.HandlerFunc(dispatchPortfolioElements(store, nativeLauncher))))
	mux.Handle("PATCH /v1/portfolio/elements/{project}/{element_ref}", requireAdmin(adminAuthenticator, http.HandlerFunc(patchPortfolioElement(store))))
	mux.Handle("POST /v1/playbooks/{project}/{playbook_ref}/run", requireAdmin(adminAuthenticator, http.HandlerFunc(runPlaybook(store, nativeLauncher))))
	mux.Handle("POST /v1/playbooks/{project}/{playbook_ref}/entries/{entry_id}/gate", requireAdmin(adminAuthenticator, http.HandlerFunc(patchPlaybookEntryGate(store))))
	mux.Handle("POST /v1/leases/cancel", requireAdmin(adminAuthenticator, http.HandlerFunc(cancelLeaseByRef(store))))
	mux.Handle("PATCH /v1/leases/ttl", requireAdmin(adminAuthenticator, http.HandlerFunc(updateLeaseTTLByRef(store, testSlotPreparer))))
	mux.Handle("PATCH /v1/test-slots/default-ttl", requireAdmin(adminAuthenticator, http.HandlerFunc(updateTestLeaseDefaultTTL(store))))
	mux.Handle("PATCH /v1/test-slots/hot-swap-min-ttl", requireAdmin(adminAuthenticator, http.HandlerFunc(updateTestLeaseHotSwapMinTTL(store))))
	mux.HandleFunc("GET /v1/projects/{project}/workflows/{name}/upstream", getWorkflowUpstream(store, ghClient))
	mux.Handle("POST /v1/projects/{project}/workflows/{name}/sync", requireAdmin(adminAuthenticator, http.HandlerFunc(syncWorkflow(store, ghClient))))
	mux.HandleFunc("GET /v1/projects/{project}/upstream", getProjectUpstream(store, ghClient))
	mux.Handle("POST /v1/projects/{project}/sync", requireAdmin(adminAuthenticator, http.HandlerFunc(syncProject(store, ghClient))))
	mux.Handle("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/abort", requireAdmin(adminAuthenticator, http.HandlerFunc(abortRunByNumber(store))))
	mux.Handle("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/touchpoint/finalize", requireAdmin(adminAuthenticator, http.HandlerFunc(finalizeRunTouchpointByNumber(store, prClient, artifactStore))))
	mux.Handle("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/cycles/{cycle_number}/touchpoint/finalize", requireAdmin(adminAuthenticator, http.HandlerFunc(finalizeRunCycleTouchpointByNumber(store, prClient, artifactStore))))
	mux.Handle("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/touchpoint/merge", requireAdmin(adminAuthenticator, http.HandlerFunc(mergeRunTouchpointByNumber(store, prClient))))
	mux.Handle("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/cycles/{cycle_number}/touchpoint/merge", requireAdmin(adminAuthenticator, http.HandlerFunc(mergeRunCycleTouchpointByNumber(store, prClient))))
	mux.HandleFunc("GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events", nativeRunEventsByNumber(store))
	mux.HandleFunc("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/events", nativeRunEventWriteByNumber(store))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/events", nativeRunEventWriteByCallbackToken(store))
	mux.HandleFunc("GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/status", nativeRunStatusByNumber(store))
	mux.HandleFunc("GET /v1/run-callbacks/{callback_token}/native/status", nativeRunStatusByCallbackToken(store))
	mux.HandleFunc("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/native/github-token", nativeGitHubTokenByNumber(store, nativeTokenMinter))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/github-token", nativeGitHubTokenByCallbackToken(store, nativeTokenMinter))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/pr-touchpoint", nativePRTouchpointByCallbackToken(store, prClient, artifactStore))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/pr-merge", nativePRMergeByCallbackToken(store, prClient))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/ssh-cert", mintRunCallbackSSHCert(store, sshCertExchanger))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/tailscale-authkey", mintRunCallbackTailscaleAuthKey(store, tailscaleAuthKeyMinter))
	mux.HandleFunc("POST /v1/run-callbacks/{callback_token}/native/completed", nativeRunCompletedByCallbackToken(store, nativeLauncher))
	if stateStore, ok := store.(StateStore); ok {
		if inspectionStore, ok := store.(SlotInspectionStore); ok {
			var artifactWriter ArtifactWriter
			if w, ok := artifactStore.(ArtifactWriter); ok {
				artifactWriter = w
			}
			var runResolver InspectionRunResolver
			if runReadStore, ok := store.(inspectionRunReadStore); ok {
				runResolver = newInspectionRunResolver(runReadStore)
			}
			mux.Handle(
				"GET /v1/inspections",
				requireAdmin(adminAuthenticator, listInspections(inspectionStore)),
			)
			mux.Handle(
				"GET /v1/inspections/{inspection_id}",
				requireAdmin(adminAuthenticator, getInspectionByID(inspectionStore)),
			)
			mux.Handle(
				"POST /v1/inspections",
				requireAdmin(adminAuthenticator, createInspection(createInspectionDeps{
					store:         inspectionStore,
					leases:        newInspectionLeaseResolver(stateStore),
					runs:          runResolver,
					artifactWrite: artifactWriter,
				})),
			)
		}
	}
	mux.Handle("POST /v1/test-slots/checkout", requireAdmin(adminAuthenticator, http.HandlerFunc(checkoutTestSlot(settings, store, testSlotPreparer, nativeTokenMinter))))
	mux.Handle("POST /v1/test-slots/return", requireAdmin(adminAuthenticator, http.HandlerFunc(returnTestSlot(store, testSlotPreparer, nativeTokenMinter))))
	mux.Handle("POST /v1/test-slots/extend", requireAdmin(adminAuthenticator, http.HandlerFunc(extendTestSlotLease(store, testSlotPreparer))))
	mux.Handle("POST /v1/test-slots/hot-swap-history", requireAdmin(adminAuthenticator, http.HandlerFunc(appendTestSlotHotSwapHistory(store, testSlotPreparer))))
	// /v1/test-slots/apply-hot-swap — developer-driven build-and-swap.
	// Sync UX per docs/test-slot-hot-swap.md. The performer wraps
	// ApplyHotSwap with a real httpK8sJobClient that talks to the k8s
	// API directly (no kubectl shell-out — glimmung's runtime image
	// doesn't include kubectl, matching the existing native_launcher
	// pattern of using `request()` over HTTP).
	k8sClient := newHTTPK8sJobClient(settings)
	applyPerformer := func(ctx context.Context, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error) {
		return ApplyHotSwap(ctx, k8sClient, opts)
	}
	mux.Handle("POST /v1/test-slots/apply-hot-swap", requireAdmin(adminAuthenticator, http.HandlerFunc(applyTestSlotHotSwap(store, testSlotPreparer, applyPerformer))))
	mux.Handle("POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/replay", requireAdmin(adminAuthenticator, http.HandlerFunc(replayRunDecisionByNumber(store))))
	mux.Handle("POST /v1/runs/dispatch", requireAdmin(adminAuthenticator, http.HandlerFunc(dispatchRunHandler(store, nativeLauncher))))
	mux.HandleFunc("POST /v1/webhook/github", githubWebhook(settings))
	// Per-run OpenGraph image: public, unauthenticated PNG card matching
	// the SPA's run-URL shape so unfurlers (Discord, Slack, etc.) get a
	// scrapeable picture of the run's phase graph. The SPA HTML injection
	// in serveSPAWithOG points og:image at this endpoint. PNG only — an
	// earlier SVG renderer was retired because Discord drops SVG (see
	// .tank/docs/migration-policy.md: no compatibility, no parallel
	// format). net/http.ServeMux can't have a literal `.png` after a
	// `{wildcard}`, so the route captures the suffix and the handler
	// strips it. See og_run.go and og_run_png.go.
	mux.HandleFunc(
		"GET /og/runs/{project}/{issue_number}/{run_number}",
		runOGImagePNG(store),
	)
	if staticRoots(settings).enabled() {
		mux.HandleFunc("GET /assets/", serveAsset(settings))
		// serveSPAWithOG falls back to serveSPA for non-run paths and any
		// run path it can't enrich. See og_run.go.
		mux.HandleFunc("GET /", serveSPAWithOG(settings, store))
	}
	return metrics.Middleware(rejectUnsafeArtifactPaths(mux))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func readyz(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "read store not configured")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if !readStoreReady(ctx, store) {
			writeUnavailable(w, r, "read store not ready", "read_store_not_ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func readStoreReady(ctx context.Context, store ReadStore) (ready bool) {
	defer func() {
		if recover() != nil {
			ready = false
		}
	}()
	_, err := store.ListProjects(ctx)
	return err == nil
}

// publicConfig serves /v1/config — read by the frontend on boot to discover
// where the auth service lives (auth.romaine.life) and where to link out for
// tank-operator. No per-host branching: slots delegate to auth.romaine.life
// just like prod and pass their own URL via `callbackURL`.
//
// The Grafana fields ship the cluster Grafana base URL and the Loki
// datasource name so the run-report UI can render Explore deep-links from
// each native-phase step row. Without them, operators have no signal in
// the dashboard that step logs exist in Loki — the data is durable, the
// discovery path was not.
func publicConfig(settings Settings) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"auth_url":                defaultAuthURL,
			"tank_operator_base_url":  strings.TrimRight(settings.TankOperatorBaseURL, "/"),
			"grafana_base_url":        strings.TrimRight(settings.GrafanaBaseURL, "/"),
			"grafana_loki_datasource": strings.TrimSpace(settings.GrafanaLokiDatasource),
			"native_runner_namespace": strings.TrimSpace(settings.NativeRunnerNamespace),
		})
	}
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

func envBoolOrDefault(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
