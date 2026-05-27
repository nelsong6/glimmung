package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// All glimmung-domain metric names that the package registers. The test
// fails if the /metrics endpoint omits any of them — catches accidental
// removal during refactors and pins the wire-level contract.
var expectedMetrics = []string{
	"glimmung_http_requests_total",
	"glimmung_http_request_duration_seconds",
	"glimmung_decisions_total",
	"glimmung_budget_breaches_total",
	"glimmung_runs_created_total",
	"glimmung_leases_acquired_total",
	"glimmung_leases_released_total",
	"glimmung_leases_held",
	"glimmung_lease_acquire_wait_seconds",
	"glimmung_hot_swap_outcomes_total",
	"glimmung_hot_swap_duration_seconds",
	"glimmung_pg_queries_total",
	"glimmung_pg_query_duration_seconds",
	"glimmung_unavailable_total",
}

func TestHandlerServesAllRegisteredMetrics(t *testing.T) {
	// Prometheus omits metric families that have zero samples, so touch
	// each one with a representative observation first. This is what
	// real traffic would do — the test catches accidental de-registration
	// during refactors.
	RecordDecision("retry")
	RecordDecision("abort_budget_cost")
	RecordDecision("abort_budget_attempts")
	RecordHotSwap("persisted", time.Second)
	RecordLeaseAcquire("dispatch", "granted", 100*time.Millisecond)
	RecordLeaseReleased("dispatch", "completed")
	RecordRunCreated("test-workflow")
	RecordPostgresQuery("select_reports", "ok", 5*time.Millisecond)
	RecordPostgresQuery("insert_runs", "error", 25*time.Millisecond)
	RecordUnavailable("POST /v1/test-slots/checkout", "test_slot_saturation")
	// HTTP layer needs a request through the middleware.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /probe-coverage", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe-coverage", nil))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, name := range expectedMetrics {
		if !strings.Contains(string(body), name) {
			t.Errorf("metric %q missing from /metrics output", name)
		}
	}
	// Default Go and process collectors should also be present.
	for _, name := range []string{"go_goroutines", "process_cpu_seconds_total"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("default collector metric %q missing", name)
		}
	}
}

func TestRecordDecisionIncrementsCounterAndBudgetBreaches(t *testing.T) {
	before := testutil.ToFloat64(decisionsTotal.WithLabelValues("abort_budget_cost"))
	beforeBreach := testutil.ToFloat64(budgetBreachesTotal.WithLabelValues("cost"))

	RecordDecision("abort_budget_cost")

	after := testutil.ToFloat64(decisionsTotal.WithLabelValues("abort_budget_cost"))
	afterBreach := testutil.ToFloat64(budgetBreachesTotal.WithLabelValues("cost"))

	if after-before != 1 {
		t.Errorf("decisions_total{decision=abort_budget_cost}: expected +1, got +%v", after-before)
	}
	if afterBreach-beforeBreach != 1 {
		t.Errorf("budget_breaches_total{reason=cost}: expected +1, got +%v", afterBreach-beforeBreach)
	}
}

// RecordPostgresQuery feeds the Postgres query counter and duration
// histogram. The operation and outcome labels are bounded by the pgx
// tracer, and this test pins the exported metric wiring.
func TestRecordPostgresQueryWiresEveryFamily(t *testing.T) {
	const operation = "select_runs"

	okBefore := testutil.ToFloat64(postgresQueriesTotal.WithLabelValues(operation, "ok"))
	errorBefore := testutil.ToFloat64(postgresQueriesTotal.WithLabelValues(operation, "error"))

	RecordPostgresQuery(operation, "ok", 7*time.Millisecond)
	RecordPostgresQuery(operation, "error", 42*time.Millisecond)

	if got := testutil.ToFloat64(postgresQueriesTotal.WithLabelValues(operation, "ok")) - okBefore; got != 1 {
		t.Errorf("queries_total{ok} delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(postgresQueriesTotal.WithLabelValues(operation, "error")) - errorBefore; got != 1 {
		t.Errorf("queries_total{error} delta = %v, want 1", got)
	}
	if testutil.CollectAndCount(postgresQueryDurationSeconds) == 0 {
		t.Error("query_duration_seconds histogram has no samples")
	}
}

// RecordUnavailable feeds the dedicated counter for deliberate 503
// responses (saturation, etc.) so they surface on a dashboard instead
// of being silent writeProblem outputs. The test pins the label
// contract — route + reason — so a refactor cannot quietly drop one.
func TestRecordUnavailableIncrementsCounter(t *testing.T) {
	const route = "POST /v1/test-thing"
	const reason = "saturation_unit_test"
	before := testutil.ToFloat64(unavailableTotal.WithLabelValues(route, reason))
	RecordUnavailable(route, reason)
	RecordUnavailable(route, reason)
	after := testutil.ToFloat64(unavailableTotal.WithLabelValues(route, reason))
	if after-before != 2 {
		t.Errorf("unavailable_total delta = %v, want 2", after-before)
	}
}

func TestPostgresQueryMetricsAdapterRecordsQuery(t *testing.T) {
	const operation = "update_runs"
	before := testutil.ToFloat64(postgresQueriesTotal.WithLabelValues(operation, "ok"))
	PostgresQueryMetrics{}.RecordQuery(operation, "ok", time.Millisecond)
	after := testutil.ToFloat64(postgresQueriesTotal.WithLabelValues(operation, "ok"))
	if after-before != 1 {
		t.Errorf("adapter did not record query: delta=%v, want 1", after-before)
	}
}

func TestRecordDecisionEmptyIsNoop(t *testing.T) {
	// Empty decision string should not panic and should not increment any
	// counter. Guards against the deferred-named-return pattern in
	// Decide() recording on error paths where the decision is "".
	before := testutil.CollectAndCount(decisionsTotal)
	RecordDecision("")
	after := testutil.CollectAndCount(decisionsTotal)
	if after != before {
		t.Errorf("RecordDecision(\"\") changed counter family size: %d -> %d", before, after)
	}
}

func TestRecordHotSwapIncrementsOutcomeCounter(t *testing.T) {
	before := testutil.ToFloat64(hotSwapOutcomesTotal.WithLabelValues("build_failed"))
	RecordHotSwap("build_failed", 5*time.Second)
	after := testutil.ToFloat64(hotSwapOutcomesTotal.WithLabelValues("build_failed"))
	if after-before != 1 {
		t.Errorf("hot_swap_outcomes_total{outcome=build_failed}: expected +1, got +%v", after-before)
	}
}

func TestRecordLeaseAcquireBalancesGaugeOnGrant(t *testing.T) {
	before := testutil.ToFloat64(leasesHeld.WithLabelValues("dispatch"))
	RecordLeaseAcquire("dispatch", "granted", 50*time.Millisecond)
	afterGrant := testutil.ToFloat64(leasesHeld.WithLabelValues("dispatch"))
	if afterGrant-before != 1 {
		t.Errorf("leases_held{purpose=dispatch} after grant: expected +1, got +%v", afterGrant-before)
	}
	RecordLeaseReleased("dispatch", "completed")
	afterRelease := testutil.ToFloat64(leasesHeld.WithLabelValues("dispatch"))
	if afterRelease != before {
		t.Errorf("leases_held{purpose=dispatch} after release: expected back to baseline, got %v (baseline %v)", afterRelease, before)
	}
}

func TestRecordLeaseAcquireConflictDoesNotMoveGauge(t *testing.T) {
	before := testutil.ToFloat64(leasesHeld.WithLabelValues("retry"))
	RecordLeaseAcquire("retry", "conflict", 10*time.Millisecond)
	after := testutil.ToFloat64(leasesHeld.WithLabelValues("retry"))
	if after != before {
		t.Errorf("leases_held{purpose=retry} on conflict: expected no change, got %v -> %v", before, after)
	}
}

func TestSafeLabelDefaultsEmpty(t *testing.T) {
	if got := safeLabel(""); got != "unknown" {
		t.Errorf("safeLabel(\"\"): expected \"unknown\", got %q", got)
	}
	if got := safeLabel("dispatch"); got != "dispatch" {
		t.Errorf("safeLabel(\"dispatch\"): expected pass-through, got %q", got)
	}
}

func TestMiddlewareRecordsPatternMethodStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	before := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET /probe", "GET", "418"))

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rec := httptest.NewRecorder()
	Middleware(mux).ServeHTTP(rec, req)

	after := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET /probe", "GET", "418"))
	if after-before != 1 {
		t.Errorf("http_requests_total{pattern=GET /probe,method=GET,status=418}: expected +1, got +%v", after-before)
	}
}
