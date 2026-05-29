package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/nelsong6/glimmung/internal/auth"
	azureclient "github.com/nelsong6/glimmung/internal/azure"
	githubclient "github.com/nelsong6/glimmung/internal/github"
	"github.com/nelsong6/glimmung/internal/metrics"
	"github.com/nelsong6/glimmung/internal/server"
	artifactstore "github.com/nelsong6/glimmung/internal/store/artifacts"
	pgstore "github.com/nelsong6/glimmung/internal/store/pg"
	glimmungstore "github.com/nelsong6/glimmung/internal/store/store"
)

// runtimeStore is the combined store passed to the HTTP server and the
// background reconcilers. It embeds the data-access wrapper plus the
// Postgres-backed LocksStore. Go field promotion gives the wrapper the
// union of both method sets.
type runtimeStore struct {
	*glimmungstore.Store
	*pgstore.LocksStore
}

func main() {
	settings := server.SettingsFromEnv()
	store, err := glimmungstore.NewFromSettings(settings)
	if err != nil {
		log.Printf("runtime store initialization failed: %v", err)
	}
	// Construct the Postgres pool and apply schema migrations before the
	// HTTP server starts. Postgres is the only durable runtime store.
	var pgPool *pgstore.Pool
	if settings.PostgresHost != "" && settings.PostgresDatabase != "" && settings.PostgresUsername != "" {
		cred, credErr := azidentity.NewDefaultAzureCredential(nil)
		if credErr != nil {
			log.Printf("postgres pool disabled: build default credential: %v", credErr)
		} else {
			poolCtx, poolCancel := context.WithTimeout(context.Background(), 20*time.Second)
			pool, poolErr := pgstore.NewPool(poolCtx, pgstore.Config{
				Host:         settings.PostgresHost,
				Database:     settings.PostgresDatabase,
				Username:     settings.PostgresUsername,
				Credential:   cred,
				QueryMetrics: metrics.PostgresQueryMetrics{},
			})
			poolCancel()
			if poolErr != nil {
				log.Printf("postgres pool disabled: %v", poolErr)
			} else {
				migCtx, migCancel := context.WithTimeout(context.Background(), 60*time.Second)
				if migErr := pgstore.RunMigrations(migCtx, pool); migErr != nil {
					log.Printf("postgres migrations failed: %v", migErr)
					pool.Close()
				} else {
					log.Printf("postgres pool ready (host=%s database=%s user=%s), schema migrations applied",
						settings.PostgresHost, settings.PostgresDatabase, settings.PostgresUsername)
					pgPool = pool
				}
				migCancel()
			}
		}
	} else {
		log.Printf("postgres pool disabled: POSTGRES_HOST/POSTGRES_DATABASE/POSTGRES_USER not all set")
	}
	// Build the combined runtime store. The wrapper owns the domain-shaped
	// methods and the embedded LocksStore supplies the lock primitive methods
	// required by server interfaces.
	var rt *runtimeStore
	if store != nil && pgPool != nil {
		pgLocks := pgstore.NewLocksStore(pgPool)
		store.SetPGLocks(pgLocks)

		pgRunEvents := pgstore.NewRunEventsStore(pgPool)
		store.SetPGRunEvents(pgRunEvents)

		pgProjects := pgstore.NewProjectsStore(pgPool)
		store.SetPGProjects(pgProjects)

		pgWorkflows := pgstore.NewWorkflowsStore(pgPool)
		store.SetPGWorkflows(pgWorkflows)

		pgIssues := pgstore.NewIssuesStore(pgPool)
		store.SetPGIssues(pgIssues)

		pgSlots := pgstore.NewSlotsStore(pgPool)
		store.SetPGSlots(pgSlots)

		pgSignals := pgstore.NewSignalsStore(pgPool)
		store.SetPGSignals(pgSignals)

		pgPlaybooks := pgstore.NewPlaybooksStore(pgPool)
		store.SetPGPlaybooks(pgPlaybooks)

		pgPortfolios := pgstore.NewPortfoliosStore(pgPool)
		store.SetPGPortfolios(pgPortfolios)

		pgTouchpoints := pgstore.NewTouchpointsStore(pgPool)
		store.SetPGTouchpoints(pgTouchpoints)

		pgRuns := pgstore.NewRunsStore(pgPool)
		store.SetPGRuns(pgRuns)

		pgLeases := pgstore.NewLeasesStore(pgPool)
		store.SetPGLeases(pgLeases)

		pgSlotInspections := pgstore.NewSlotInspectionsStore(pgPool)
		store.SetPGSlotInspections(pgSlotInspections)

		rt = &runtimeStore{Store: store, LocksStore: pgLocks}
	} else {
		log.Printf("runtime store disabled: store wrapper and postgres pool both required (store=%v pgPool=%v)", store != nil, pgPool != nil)
	}

	artifacts, err := artifactstore.NewFromSettings(settings)
	if err != nil {
		log.Printf("artifact store disabled: %v", err)
	}
	authenticator := buildAuthenticator(settings)
	ghClient := buildGitHubClient(settings)
	workloadIdentityClient, err := azureclient.NewWorkloadIdentityClient()
	if err != nil {
		log.Printf("native workload identity reconciler disabled: %v", err)
	}
	workloadIdentities := server.NativeWorkloadIdentityService{
		Client:                  workloadIdentityClient,
		Issuer:                  settings.NativeWorkloadIdentityIssuer,
		ServiceAccountTokenPath: settings.K8sSATokenPath,
	}
	// Glimmung-owned auth.romaine.life slot origin reconciler. Uses a
	// projected SA token mounted with audience = auth.romaine.life so it
	// cannot be replayed against other JWT validators. See glimmung#142.
	managedOrigins := server.ManagedOriginService{
		AuthBaseURL:             settings.AuthRomaineLifeBaseURL,
		ServiceAccountTokenPath: settings.AuthRomaineLifeTokenPath,
	}
	nativeLauncher := server.NewKubernetesNativeLauncher(settings)
	if store != nil {
		// One-shot slot-storage cleanup: copy any project's embedded
		// `metadata.native_standby_dns.slots[]` array into `slots`, then
		// strip the embedded array before readiness goes live.
		migrationCtx, cancelMigration := context.WithTimeout(context.Background(), 60*time.Second)
		summary, err := server.MigrateProjectSlotsIntoCollection(migrationCtx, store)
		cancelMigration()
		if err != nil {
			log.Printf("slot-storage migration failed: %v (summary: %s)", err, summary)
			os.Exit(1)
		}
		log.Printf("slot-storage migration ok: %s", summary)
	}
	// Background control-plane reconcilers and the test-slot recovery
	// sweep. These mutate shared runtime state in Postgres and the
	// glimmung-runs namespace and are owned by the prod glimmung
	// deployment. Test slots (k8s/issue chart) set
	// CONTROL_PLANE_LOOPS_ENABLED=false so a hot-swapped binary exercises
	// HTTP handlers and code paths without racing prod for the same rows
	// or Kubernetes Jobs. See Settings.ControlPlaneLoopsEnabled.
	//
	// Any new background reconciler must be started inside this gate.
	switch {
	case rt == nil:
		// Runtime store unavailable — reconcilers have nothing to read.
		// The earlier "runtime store disabled" log already explains why.
	case !settings.ControlPlaneLoopsEnabled:
		log.Printf("control-plane reconcilers disabled (CONTROL_PLANE_LOOPS_ENABLED=false); signal drain, run queue, dispatch timeout, and test-slot recovery will not run in this process")
	default:
		// One-shot lease-orphan sweep at startup. Transitions every
		// lease past its durable expires_at deadline whose state is
		// still active or claimed to state=expired. Covers active
		// dispatch leases whose completion callback never arrived and
		// claimed test-slot leases whose AfterFunc timer died with the
		// previous glimmung process. RecoverInFlightTestSlots below only
		// re-arms timers for claimed leases carrying
		// test_slot_checkout=true metadata; older lease shapes need
		// this sweep. See server.ExpireStaleLeases for the full
		// contract.
		expireCtx, cancelExpire := context.WithTimeout(context.Background(), 60*time.Second)
		expired, expireErr := server.ExpireStaleLeases(expireCtx, rt, time.Now().UTC(), log.Printf)
		cancelExpire()
		if expireErr != nil {
			log.Printf("stale-lease expiry sweep failed: %v", expireErr)
		} else {
			log.Printf("stale-lease expiry sweep ok: expired=%d", expired)
		}
		server.StartSignalDrainReconciler(context.Background(), rt, nativeLauncher, log.Printf)
		server.StartRunQueueReconciler(context.Background(), rt, nativeLauncher, log.Printf)
		server.StartRunDispatchTimeoutReconciler(context.Background(), settings, rt, nativeLauncher, log.Printf)
		if nativeMinter, ok := ghClient.(server.NativeGitHubTokenMinter); ok {
			// One-shot recovery sweep at startup: re-arm per-lease TTL
			// timers, resume in-flight warming/activating/cleaning work, and
			// warm any slots that should exist by count but have no record
			// yet. After this returns, the test-slot lifecycle is purely
			// event-driven — HTTP handlers and per-lease AfterFunc timers,
			// no polling loop.
			go server.RecoverInFlightTestSlots(context.Background(), rt, nativeLauncher, nativeMinter, log.Printf)
		}
	}
	addr := ":" + settings.Port

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.NewWithReconcilers(settings, rt, authenticator, ghClient, workloadIdentities, managedOrigins, nativeLauncher, artifacts),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown: SIGTERM (from k8s eviction or node drain) triggers
	// HTTP draining followed by a bounded wait for in-flight test-slot
	// goroutines (warmup, activation, cleanup) to finish their Helm
	// operations. Without this, a pod evicted mid-Helm-install leaves a
	// partial release that the next pod's recovery sweep has to clean up,
	// and inbound HTTP requests get dropped instead of completing.
	//
	// The wait budget is sized to fit inside the Pod's
	// terminationGracePeriodSeconds (300s in the chart) with margin for
	// the HTTP drain and final cleanup. A Helm operation longer than this
	// will be cut off; the next pod's recovery sweep handles it.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-signalCtx.Done()
		log.Printf("shutdown signal received; draining HTTP server")
		httpCtx, httpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := srv.Shutdown(httpCtx); err != nil {
			log.Printf("http server shutdown error: %v", err)
		}
		httpCancel()

		log.Printf("waiting for in-flight test-slot goroutines to finish")
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		if err := server.WaitForInflightTestSlots(waitCtx); err != nil {
			log.Printf("in-flight test-slot wait exceeded budget: %v (orphans will be picked up by next pod's recovery sweep)", err)
		} else {
			log.Printf("in-flight test-slot goroutines drained")
		}
		waitCancel()
	}()

	log.Printf("starting glimmung-go on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
	// Wait for the shutdown goroutine to finish its drain + wait before exiting.
	<-shutdownDone
	log.Printf("glimmung-go shutdown complete")
}

type gitHubClientAdapter struct {
	client *githubclient.Client
}

func (a *gitHubClientAdapter) FetchWorkflowFile(ctx context.Context, repo, name, ref string) ([]byte, int, error) {
	path := ".glimmung/workflows/" + name + ".yaml"
	data, err := a.client.FetchFileContents(ctx, repo, path, ref)
	if errors.Is(err, githubclient.ErrNotFound) {
		return nil, 404, err
	}
	if err != nil {
		return nil, 502, err
	}
	return data, 200, nil
}

func (a *gitHubClientAdapter) InstallationToken(ctx context.Context) (string, error) {
	return a.client.InstallationToken(ctx)
}

func (a *gitHubClientAdapter) EnsurePullRequest(ctx context.Context, req server.PullRequestEnsureRequest) (server.PullRequest, error) {
	pr, err := a.client.EnsurePullRequest(ctx, githubclient.PullRequestEnsureRequest{
		Repo:  req.Repo,
		Base:  req.Base,
		Head:  req.Head,
		Title: req.Title,
		Body:  req.Body,
	})
	if err != nil {
		return server.PullRequest{}, err
	}
	return server.PullRequest{
		Number:  pr.Number,
		Title:   pr.Title,
		Body:    pr.Body,
		Branch:  pr.Branch,
		BaseRef: pr.BaseRef,
		HeadSHA: pr.HeadSHA,
		HTMLURL: pr.HTMLURL,
		State:   pr.State,
	}, nil
}

func (a *gitHubClientAdapter) MergePullRequest(ctx context.Context, req server.PullRequestMergeRequest) (server.PullRequestMergeResult, error) {
	result, err := a.client.MergePullRequest(ctx, githubclient.PullRequestMergeRequest{
		Repo:        req.Repo,
		Number:      req.Number,
		CommitTitle: req.CommitTitle,
		MergeMethod: req.MergeMethod,
	})
	if err != nil {
		return server.PullRequestMergeResult{}, err
	}
	return server.PullRequestMergeResult{
		Number:         result.Number,
		HTMLURL:        result.HTMLURL,
		State:          result.State,
		MergeCommitSHA: result.MergeCommitSHA,
		AlreadyMerged:  result.AlreadyMerged,
	}, nil
}

func buildGitHubClient(settings server.Settings) server.WorkflowSyncClient {
	if settings.GitHubAppID == "" || settings.GitHubAppInstallationID == "" || settings.GitHubAppPrivateKey == "" {
		return nil
	}
	client, err := githubclient.New(settings.GitHubAppID, settings.GitHubAppInstallationID, settings.GitHubAppPrivateKey)
	if err != nil {
		log.Printf("GitHub App client disabled: %v", err)
		return nil
	}
	return &gitHubClientAdapter{client: client}
}

func buildAuthenticator(settings server.Settings) auth.CompositeAuthenticator {
	// auth.romaine.life is the single identity provider for the
	// .romaine.life ecosystem. Three presentation formats arrive on the
	// wire, all rooted in the same trust:
	//
	//   1. Session cookies for browser callers — forwarded to the IdP's
	//      get-session endpoint per request (cached 60s), CookieDelegate.
	//   2. Bearer JWTs minted by auth.romaine.life — verified locally
	//      against the IdP's published JWKS, RomaineLifeJWTVerifier.
	//      Carries role ∈ {admin, user, service}; service tokens
	//      additionally carry actor_email naming the human on whose
	//      behalf the bot is acting.
	//   3. Legacy in-cluster K8s SA tokens — verified via Kubernetes
	//      TokenReview, K8sAuthenticator. Retired once mcp-glimmung and
	//      mcp-tank-operator switch to the JWT exchange flow; see
	//      tank-operator#490 for the parallel cutover.
	cookieDelegate := auth.NewCookieDelegate()
	romaineJWT := auth.NewRomaineLifeJWTVerifier()

	var k8s *auth.K8sAuthenticator
	if settings.K8sSAAllowlist != "" {
		authenticator, err := auth.NewK8sAuthenticator(auth.K8sConfig{
			APIHost:      settings.K8sAPIHost,
			Allowlist:    settings.K8sSAAllowlist,
			OwnTokenPath: settings.K8sSATokenPath,
			CACertPath:   settings.K8sCACertPath,
		})
		if err != nil {
			log.Printf("k8s auth disabled: %v", err)
		} else {
			k8s = authenticator
		}
	}

	return auth.CompositeAuthenticator{Cookie: cookieDelegate, Romaine: romaineJWT, K8s: k8s}
}
