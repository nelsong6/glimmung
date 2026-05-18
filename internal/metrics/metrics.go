// Package metrics owns glimmung's Prometheus instrumentation.
//
// The exported recorder helpers (Record*) are the only surface domain
// packages should call. The underlying prometheus.Registry and metric
// definitions are intentionally private so the metric contract — names,
// label sets, bucket choices — lives in one file. See docs/observability.md
// for the metric families and label policy.
//
// Cardinality is bounded by construction: every label value used here is
// either a closed enum (decision outcomes, hot-swap outcomes, verification
// status) or a registered identifier (workflow name, route pattern,
// requirement token). Raw user input — issue numbers, project slugs, repo
// paths — never lands in a label.
package metrics

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// registry is glimmung's single Prometheus registry. The default global
// registry is not used so tests can replace it deterministically and the
// surface stays explicit.
var registry = prometheus.NewRegistry()

// Handler returns the http.Handler that serves /metrics. Mounted by the
// server constructor next to /healthz.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		Registry:          registry,
		EnableOpenMetrics: true,
	})
}

// Registry exposes the underlying registry for test packages that need to
// inspect metric values. Do not use this from production code — call the
// Record* helpers below instead.
func Registry() *prometheus.Registry {
	return registry
}

// --- HTTP layer --------------------------------------------------------------

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_http_requests_total",
			Help: "Glimmung HTTP requests, labelled by registered route pattern, method, and status class.",
		},
		[]string{"pattern", "method", "status"},
	)
	httpRequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "glimmung_http_request_duration_seconds",
			Help:    "Glimmung HTTP request duration, labelled by registered route pattern and method.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"pattern", "method"},
	)
)

// statusRecorder captures the response status code without buffering the
// body. http.ResponseWriter.WriteHeader is optional — handlers that write
// directly default to 200, which we mirror here.
//
// We forward Flush and Hijack to the wrapped writer because handlers
// type-assert to http.Flusher (SSE in stateEvents) and http.Hijacker
// (any future WebSocket-style upgrade) and would silently degrade if we
// dropped those interfaces. WriteHeader is the only method we actually
// intercept; everything else passes through.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var errHijackerUnsupported = errors.New("hijacker not supported by underlying ResponseWriter")

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errHijackerUnsupported
}

// Middleware wraps an http.Handler and records request count and duration.
// It must wrap the *mux* (not individual handlers) so r.Pattern is the
// registered route — keeping cardinality bounded.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		pattern := r.Pattern
		if pattern == "" {
			pattern = "unmatched"
		}
		method := r.Method
		status := strconv.Itoa(rec.status)
		httpRequestsTotal.WithLabelValues(pattern, method, status).Inc()
		httpRequestDurationSeconds.WithLabelValues(pattern, method).Observe(time.Since(start).Seconds())
	})
}

// --- Verify loop / decisions -------------------------------------------------

var (
	decisionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_decisions_total",
			Help: "Verify-loop decisions emitted by the decision engine, labelled by outcome (retry, advance, abort_budget_attempts, abort_budget_cost, abort_malformed).",
		},
		[]string{"decision"},
	)
	budgetBreachesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_budget_breaches_total",
			Help: "Run budget breaches that aborted the verify loop, labelled by breach reason (cost, attempts).",
		},
		[]string{"reason"},
	)
)

// RecordDecision increments the decision counter for one verify-loop
// outcome. Decision is the decision.RunDecision value; the caller passes
// its string form so this package stays free of the domain dependency.
func RecordDecision(decision string) {
	if decision == "" {
		return
	}
	decisionsTotal.WithLabelValues(decision).Inc()
	switch decision {
	case "abort_budget_cost":
		budgetBreachesTotal.WithLabelValues("cost").Inc()
	case "abort_budget_attempts":
		budgetBreachesTotal.WithLabelValues("attempts").Inc()
	}
}

// --- Runs --------------------------------------------------------------------
//
// V1 records only run creation. Terminal-state histograms (duration,
// attempts, cost) require plumbing the run's created-at timestamp and
// cumulative cost through SetRunTerminalState callers and are deferred to
// a follow-up. Terminal decisions are still observable via
// glimmung_decisions_total (every terminal Decide() call emits one
// advance / abort_* row), so the verify loop stays fully visible without
// the run histograms.

var runsCreatedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "glimmung_runs_created_total",
		Help: "Runs created via dispatch, labelled by workflow.",
	},
	[]string{"workflow"},
)

// RecordRunCreated counts a newly dispatched run. workflow is the
// registered workflow name (bounded cardinality).
func RecordRunCreated(workflow string) {
	runsCreatedTotal.WithLabelValues(safeLabel(workflow)).Inc()
}

// --- Leases ------------------------------------------------------------------

var (
	leasesAcquiredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_leases_acquired_total",
			Help: "Lease acquisitions, labelled by caller purpose (dispatch, advance, retry, resume, test_slot_checkout, signal_drain) and outcome (granted, conflict, error).",
		},
		[]string{"purpose", "outcome"},
	)
	leasesReleasedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_leases_released_total",
			Help: "Lease releases, labelled by caller purpose and outcome (cancelled, expired, completed).",
		},
		[]string{"purpose", "outcome"},
	)
	leasesHeld = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "glimmung_leases_held",
			Help: "Currently-held leases by caller purpose. Approximate — derived from acquire/release deltas in-process; authoritative state lives in Cosmos.",
		},
		[]string{"purpose"},
	)
	leaseAcquireWaitSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "glimmung_lease_acquire_wait_seconds",
			Help:    "Wall-clock time spent in AcquireLease, labelled by caller purpose and outcome.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms .. ~40s
		},
		[]string{"purpose", "outcome"},
	)
)

// RecordLeaseAcquire records one AcquireLease call. outcome is "granted",
// "conflict" (lease already held by another claimant), or "error" (cosmos
// or transient failure). Increments the held gauge on grant; the caller
// must call RecordLeaseReleased on release to keep the gauge balanced.
func RecordLeaseAcquire(purpose, outcome string, wait time.Duration) {
	p := safeLabel(purpose)
	out := safeLabel(outcome)
	leasesAcquiredTotal.WithLabelValues(p, out).Inc()
	leaseAcquireWaitSeconds.WithLabelValues(p, out).Observe(wait.Seconds())
	if out == "granted" {
		leasesHeld.WithLabelValues(p).Inc()
	}
}

// RecordLeaseReleased records one lease release. outcome is "cancelled"
// (admin or programmatic cancel), "expired" (TTL fired), or "completed"
// (consumer reported done). Decrements the held gauge.
func RecordLeaseReleased(purpose, outcome string) {
	p := safeLabel(purpose)
	out := safeLabel(outcome)
	leasesReleasedTotal.WithLabelValues(p, out).Inc()
	leasesHeld.WithLabelValues(p).Dec()
}

// --- Hot-swap ----------------------------------------------------------------
//
// The hot-swap counter is the one explicitly named in
// scripts/check-apply-test-slot-hot-swap-migration.mjs as deferred to "a
// separate PR when glimmung gets a /metrics endpoint". This is that wire-up.

var (
	hotSwapOutcomesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_hot_swap_outcomes_total",
			Help: "Hot-swap apply outcomes, labelled by named failure mode (persisted, build_failed, swap_failed, timeout).",
		},
		[]string{"outcome"},
	)
	hotSwapDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "glimmung_hot_swap_duration_seconds",
			Help:    "Hot-swap apply duration, labelled by outcome.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10), // 1s .. ~17min
		},
		[]string{"outcome"},
	)
)

// RecordHotSwap records the terminal outcome and wall-clock duration of
// an ApplyHotSwap invocation. outcome must be one of: persisted,
// build_failed, swap_failed, timeout.
func RecordHotSwap(outcome string, duration time.Duration) {
	out := safeLabel(outcome)
	hotSwapOutcomesTotal.WithLabelValues(out).Inc()
	hotSwapDurationSeconds.WithLabelValues(out).Observe(duration.Seconds())
}

// --- Cosmos query layer ------------------------------------------------------
//
// Per-query observability for the three primitives in
// internal/store/cosmos/query.go (singlePartitionQuery, crossPartitionQuery,
// fanOutByProject). The contract migration that introduced those primitives
// also retired a class of latent bug — implicit cross-partition ORDER BY
// queries that the Go SDK can't fan out — and these metrics exist so a
// regression surfaces on a dashboard, not as a 5xx-per-minute log only the
// next operator to grep finds. See docs/cosmos-partition-strategy.md.

var (
	cosmosQueriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_cosmos_queries_total",
			Help: "Cosmos queries dispatched, labelled by container, partition mode (single, cross, fanout), and outcome (success, error).",
		},
		[]string{"container", "mode", "outcome"},
	)
	cosmosQueryDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "glimmung_cosmos_query_duration_seconds",
			Help:    "Cosmos query wall-clock duration, labelled by container and partition mode. Fanout duration is the total across all per-partition iterations.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms .. ~20s
		},
		[]string{"container", "mode"},
	)
	cosmosQueryRuChargeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_cosmos_query_ru_charge_total",
			Help: "Cumulative Cosmos RU charge consumed by queries, labelled by container and partition mode.",
		},
		[]string{"container", "mode"},
	)
	cosmosFanoutPartitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_cosmos_fanout_partitions_total",
			Help: "Total per-partition iterations executed by fanOutByProject, labelled by container. Divide by the queries_total fanout count for partition fan-out factor.",
		},
		[]string{"container"},
	)
	cosmosQueryPlanErrorTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "glimmung_cosmos_query_plan_error_total",
			Help: "Cosmos 400 BadRequest responses that match the query-plan failure shape (cross-partition ORDER BY/DISTINCT/GROUP BY/OFFSET/TOP via the Go SDK). The dedicated counter exists because the production /v1/touchpoints 5xx-per-minute bug surfaced as a generic 5xx for hours before anyone noticed; this counter would have alerted on it directly.",
		},
		[]string{"container"},
	)
)

// CosmosQueryMode is the closed enum of partition-strategy modes recorded by
// RecordCosmosQuery. Mirrors the three primitives in
// internal/store/cosmos/query.go so a metrics label cannot drift away from
// the primitive a callsite actually used.
const (
	CosmosQueryModeSingle = "single"
	CosmosQueryModeCross  = "cross"
	CosmosQueryModeFanout = "fanout"
)

// CosmosQueryOutcome is the closed enum of query outcomes. error is the
// terminal SDK error; success is everything that completed without one,
// including empty result sets.
const (
	CosmosQueryOutcomeSuccess = "success"
	CosmosQueryOutcomeError   = "error"
)

// RecordCosmosQuery records one Cosmos query execution. container is the
// short container name (e.g. "reports"); mode is one of the
// CosmosQueryMode* constants; duration is wall-clock; ruCharge is the
// summed RU charge across every page returned by the pager; queryPlanErr
// is true when the SDK error matches the query-plan failure shape
// (rejected cross-partition ORDER BY etc.), which increments the
// dedicated counter alongside the generic error outcome.
func RecordCosmosQuery(container, mode string, duration time.Duration, ruCharge float64, outcome string, queryPlanErr bool) {
	c := safeLabel(container)
	m := safeLabel(mode)
	o := safeLabel(outcome)
	cosmosQueriesTotal.WithLabelValues(c, m, o).Inc()
	cosmosQueryDurationSeconds.WithLabelValues(c, m).Observe(duration.Seconds())
	if ruCharge > 0 {
		cosmosQueryRuChargeTotal.WithLabelValues(c, m).Add(ruCharge)
	}
	if queryPlanErr {
		cosmosQueryPlanErrorTotal.WithLabelValues(c).Inc()
	}
}

// RecordCosmosFanoutPartition increments the partition-iteration counter
// for one fanOutByProject step. Called once per project the helper
// visits; the count divided by the fanout-mode queries_total count gives
// the observed average fan-out factor.
func RecordCosmosFanoutPartition(container string) {
	cosmosFanoutPartitionsTotal.WithLabelValues(safeLabel(container)).Inc()
}

// --- Test slot lifecycle -----------------------------------------------------
//
// The activation_cancelled counter exists because the activation/cleanup race
// it tracks is the failure mode that previously stranded slots in `error`
// state: a return arriving mid-activation raced the in-flight activation
// goroutine, the goroutine kept recreating the lease-scoped Playwright
// Deployment, waitForNoPodsInNamespaces timed out, and the slot transitioned
// to terminal-error. The cancel-await wiring eliminates the race; this
// counter makes the cancel-from-cleanup path observable so a regression
// surfaces on a dashboard, not as a stranded slot the next operator finds.
// cause is a closed enum (return, callback_release, ttl_expiry, recovery);
// label cardinality is bounded.

var testSlotActivationCancelledTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "glimmung_test_slot_activation_cancelled_total",
		Help: "Test-slot activation goroutines cancelled by a cleanup path before completing on their own, labelled by trigger cause (return, callback_release, ttl_expiry, recovery).",
	},
	[]string{"cause"},
)

// RecordTestSlotActivationCancelled counts one cleanup-driven cancellation
// of an in-flight activation goroutine. Called from cancelInflightActivation
// when there was a live token in testSlotActivations — same-key cancellation
// is at-most-once per goroutine because the activation token is removed
// from the map by the goroutine's own defer chain.
func RecordTestSlotActivationCancelled(cause string) {
	testSlotActivationCancelledTotal.WithLabelValues(safeLabel(cause)).Inc()
}

var testSlotCleanupClaimTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "glimmung_test_slot_cleanup_claim_total",
		Help: "Outcomes of the etag-CAS that transitions a slot to `cleaning`, labelled by source (return, callback_release, ttl_expiry, recovery) and outcome (granted, lost_race, error). Granted means this caller won the claim and is running cleanup; lost_race means another replica got there first (multi-replica safety); error is a real store failure.",
	},
	[]string{"source", "outcome"},
)

// Test-slot cleanup-claim outcome labels.
const (
	CleanupClaimOutcomeGranted  = "granted"
	CleanupClaimOutcomeLostRace = "lost_race"
	CleanupClaimOutcomeError    = "error"
)

// RecordTestSlotCleanupClaim counts one outcome of claimTestSlotCleanup.
// source mirrors the activationCancel* and history-entry source strings
// (return, callback_release, ttl_expiry, recovery); outcome is one of the
// CleanupClaimOutcome* constants.
func RecordTestSlotCleanupClaim(source, outcome string) {
	testSlotCleanupClaimTotal.WithLabelValues(safeLabel(source), safeLabel(outcome)).Inc()
}

// --- Deliberate 503 responses -----------------------------------------------
//
// writeProblem with http.StatusServiceUnavailable is silent by design (see
// the comment block above writeProblem in internal/server/read_api.go);
// it is the right shape for "X store not configured" responses, which
// have no operational signal worth logging. Deliberate operational 503s
// — saturation, retryable transient unavailability — are different: they
// reflect runtime state operators want to know about. writeUnavailable
// in the server package emits a structured slog.Warn and feeds this
// counter so a saturation event is observable on a dashboard, not silent.

var unavailableTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "glimmung_unavailable_total",
		Help: "Deliberate 503 ServiceUnavailable responses emitted by writeUnavailable, labelled by registered route pattern and a short reason enum (e.g. test_slot_saturation). Configuration-absence 503s ('X store not configured') go through writeProblem and are intentionally not counted here.",
	},
	[]string{"route", "reason"},
)

// RecordUnavailable counts one deliberate 503 response. route is the
// registered ServeMux pattern (already used by the HTTP middleware so
// cardinality is bounded); reason is a short, closed-enum string the
// callsite picks at compile time (e.g. test_slot_saturation). Raw user
// input must not land in either label.
func RecordUnavailable(route, reason string) {
	unavailableTotal.WithLabelValues(safeLabel(route), safeLabel(reason)).Inc()
}

// --- Auth (auth.romaine.life JWT verifier) -----------------------------------

var authRomaineLifeRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "glimmung_auth_romaine_life_requests_total",
		Help: "auth.romaine.life JWT verifier outcomes. Mirrors tank-operator's tank_service_role_requests_total contract: result is a closed enum so cardinality is bounded.",
	},
	[]string{"role", "result"},
)

// AuthOutcome values are the closed enum tank-operator established in
// #490 plus glimmung's own additions for the missing-fields cases the
// glimmung verifier surfaces.
const (
	AuthOutcomeOK                     = "ok"
	AuthOutcomeDeniedToken            = "denied_token"
	AuthOutcomeDeniedRole             = "denied_role"
	AuthOutcomeDeniedActorMissing     = "denied_actor_missing"
	AuthOutcomeDeniedIssuer           = "denied_issuer"
	AuthOutcomeDeniedAudience         = "denied_audience"
	AuthOutcomeErrorVerifierMisconfig = "error_verifier_unconfigured"
)

// RecordAuthRomaineLifeRequest records one verification outcome for an
// inbound auth.romaine.life JWT. role is "admin" / "user" / "service" /
// "unknown" (when the token was rejected before the role claim could be
// read). result is one of the AuthOutcome* constants above. Both labels
// are closed sets so the metric stays low-cardinality.
func RecordAuthRomaineLifeRequest(role, result string) {
	authRomaineLifeRequestsTotal.WithLabelValues(safeLabel(role), safeLabel(result)).Inc()
}

// --- Registration ------------------------------------------------------------
//
// k8s Job apply/terminal metrics are not in V1: the dispatch path emits a
// Job and forgets — terminal state propagates back asynchronously via
// callbacks and pod log polling, with no single in-process site that owns
// "the Job is now terminal." Wiring them correctly needs the run-callback
// surface to grow a job-terminal signal first. The Job apply step is
// already observable via the lease metric (every apply is preceded by an
// AcquireLease) so the queue side stays visible without these.

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		httpRequestsTotal,
		httpRequestDurationSeconds,
		decisionsTotal,
		budgetBreachesTotal,
		runsCreatedTotal,
		leasesAcquiredTotal,
		leasesReleasedTotal,
		leasesHeld,
		leaseAcquireWaitSeconds,
		hotSwapOutcomesTotal,
		hotSwapDurationSeconds,
		cosmosQueriesTotal,
		cosmosQueryDurationSeconds,
		cosmosQueryRuChargeTotal,
		cosmosFanoutPartitionsTotal,
		cosmosQueryPlanErrorTotal,
		unavailableTotal,
		authRomaineLifeRequestsTotal,
		testSlotActivationCancelledTotal,
		testSlotCleanupClaimTotal,
	)
}

// safeLabel guards against the empty string winning a label slot.
// Prometheus accepts it but it leaves operators staring at "" — a
// concrete sentinel beats a blank.
func safeLabel(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
