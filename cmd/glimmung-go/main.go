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
	"github.com/nelsong6/glimmung/internal/server"
	artifactstore "github.com/nelsong6/glimmung/internal/store/artifacts"
	cosmosstore "github.com/nelsong6/glimmung/internal/store/cosmos"
	pgstore "github.com/nelsong6/glimmung/internal/store/pg"
)

// runtimeStore is the combined store passed to the HTTP server and the
// background reconcilers. It embeds the cosmos-backed store (which holds
// every method that hasn't been migrated yet) AND the Postgres-backed
// LocksStore (which serves all lock operations as of Stage 2b). Go field
// promotion gives the wrapper the union of both method sets — no manual
// forwarding. Successive sub-stages will add more pg.* embeds; Stage 2i
// removes the cosmos embed entirely.
type runtimeStore struct {
	*cosmosstore.Store
	*pgstore.LocksStore
}

func main() {
	settings := server.SettingsFromEnv()
	store, err := cosmosstore.NewFromSettings(settings)
	if err != nil {
		log.Printf("cosmos read store disabled: %v", err)
	}
	// Stage 2a foundation: construct the Postgres pool and apply schema
	// migrations. No internal/server/ consumer reads from this pool yet —
	// the runtime still uses the cosmos store above. Stages 2b through 2h
	// cut interface clusters over one at a time; 2i deletes the cosmos
	// store entirely. See docs/postgres-migration.md.
	//
	// Skipping pool construction when POSTGRES_HOST is unset preserves
	// local-dev ergonomics (no Postgres dependency until you set the env
	// var) and matches tank-operator's pgstore degraded-stub pattern.
	var pgPool *pgstore.Pool
	if settings.PostgresHost != "" && settings.PostgresDatabase != "" && settings.PostgresUsername != "" {
		cred, credErr := azidentity.NewDefaultAzureCredential(nil)
		if credErr != nil {
			log.Printf("postgres pool disabled: build default credential: %v", credErr)
		} else {
			poolCtx, poolCancel := context.WithTimeout(context.Background(), 20*time.Second)
			pool, poolErr := pgstore.NewPool(poolCtx, pgstore.Config{
				Host:       settings.PostgresHost,
				Database:   settings.PostgresDatabase,
				Username:   settings.PostgresUsername,
				Credential: cred,
				// QueryMetrics intentionally nil for Stage 2a — the
				// tracer becomes a no-op until 2b wires up the
				// Prometheus collector in internal/metrics.
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
	// Stage 2b: construct the Postgres LocksStore, run the one-shot
	// idempotent migration that copies every cosmos lock document into
	// the pg `locks` table, inject the LocksStore into cosmos.Store so
	// its lock-reading methods (ListIssues / ListTouchpoints /
	// GetIssueDetailByNumber / touchpoint detail) read from pg, and
	// build the combined runtime store the reconcilers consume.
	//
	// `rt` is the only argument every consumer (server.New*, reconcilers,
	// recovery sweep) should receive going forward. Passing the raw
	// cosmos.Store would re-introduce the (now-deleted) cosmos lock
	// methods to the interface fulfillment check and would fail to
	// compile against any interface that includes ClaimLock etc.
	var rt *runtimeStore
	if store != nil && pgPool != nil {
		// Stage 2c hotfix: pg.LocksStore and pg.RunEventsStore are
		// constructed and SET on cosmos.Store synchronously so the
		// runtime store the HTTP server consumes is correctly wired
		// from t=0. But the one-shot cosmos->pg Migrate copies run in
		// background goroutines, NOT blocking main.go's path to
		// srv.ListenAndServe(). The original Stage 2c blocked startup
		// on Migrate; for run_events that meant the pod's liveness
		// probe killed the container before the HTTP server bound to
		// port 8000 (cosmos `run_events` has thousands of rows and the
		// crossPartitionQuery + per-row INSERT took longer than the
		// 90s liveness window).
		//
		// Async is safe because Insert is idempotent (ON CONFLICT DO
		// NOTHING / DO UPDATE WHERE released-or-expired). Any new
		// claim or event written by handlers while the migration is in
		// flight goes to pg directly; if the migration later sees the
		// same key in cosmos, the ON CONFLICT path swallows it. The
		// runtime correctness contract — pg is the source of truth from
		// pod start — holds throughout.
		pgLocks := pgstore.NewLocksStore(pgPool)
		store.SetPGLocks(pgLocks)
		go func() {
			migCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			copied, skipped, err := pgLocks.Migrate(migCtx, store)
			if err != nil {
				log.Printf("lock migration cosmos->pg failed: %v (re-run by restarting pod; Migrate is idempotent)", err)
				return
			}
			log.Printf("lock migration cosmos->pg: copied=%d skipped=%d", copied, skipped)
		}()

		pgRunEvents := pgstore.NewRunEventsStore(pgPool)
		store.SetPGRunEvents(pgRunEvents)
		go func() {
			migCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			copied, skipped, err := pgRunEvents.Migrate(migCtx, store)
			if err != nil {
				log.Printf("run-events migration cosmos->pg failed: %v (re-run by restarting pod; Migrate is idempotent)", err)
				return
			}
			log.Printf("run-events migration cosmos->pg: copied=%d skipped=%d", copied, skipped)
		}()

		// Stage 2d: pg.ProjectsStore foundation. Idempotent Migrate
		// copies cosmos.projects (per-project docs + the singleton
		// settings doc) into pg's `projects` + `test_lease_defaults`
		// tables. Foundation-only this stage — no cosmos.Store method
		// delegation yet (Stage 2e). Production reads/writes still go
		// to cosmos until then.
		pgProjects := pgstore.NewProjectsStore(pgPool)
		_ = pgProjects // referenced by the goroutine below; no consumer in 2d.
		go func() {
			migCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			copied, skipped, err := pgProjects.Migrate(migCtx, store)
			if err != nil {
				log.Printf("projects migration cosmos->pg failed: %v (re-run by restarting pod; Migrate is idempotent)", err)
				return
			}
			log.Printf("projects migration cosmos->pg: copied=%d skipped=%d", copied, skipped)
		}()

		rt = &runtimeStore{Store: store, LocksStore: pgLocks}
	} else {
		log.Printf("runtime store disabled: cosmos store and postgres pool both required (store=%v pgPool=%v)", store != nil, pgPool != nil)
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
		// One-shot slot-storage migration: copy any project's legacy
		// `metadata.native_standby_dns.slots[]` array into the new
		// `slots` collection, then strip the legacy array. Idempotent —
		// re-running on every boot is safe. Blocks HTTP server start
		// so readiness doesn't go live while readers might see a
		// partially-migrated store.
		migrationCtx, cancelMigration := context.WithTimeout(context.Background(), 60*time.Second)
		summary, err := server.MigrateProjectSlotsIntoCollection(migrationCtx, store)
		cancelMigration()
		if err != nil {
			log.Printf("slot-storage migration failed: %v (summary: %s)", err, summary)
			os.Exit(1)
		}
		log.Printf("slot-storage migration ok: %s", summary)
	}
	// Background reconcilers consume the combined runtime store (rt) so
	// they get both the cosmos-backed durable methods AND the pg-backed
	// lock methods. cosmos.Store alone no longer satisfies these
	// interfaces (Stage 2b deleted its lock methods).
	if rt != nil {
		server.StartSignalDrainReconciler(context.Background(), rt, nativeLauncher, log.Printf)
		server.StartRunQueueReconciler(context.Background(), rt, nativeLauncher, log.Printf)
		server.StartRunDispatchTimeoutReconciler(context.Background(), settings, rt, nativeLauncher, log.Printf)
	}
	if rt != nil {
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
