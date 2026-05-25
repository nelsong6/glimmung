package pg

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// QueryTracer is the pgx tracer the glimmung pod installs on its pool to
// emit per-query Prometheus metrics. It is intentionally outcome-only:
// the only labels are the bounded "operation" (a stable keyword pulled
// from the SQL text) and "outcome" ("ok" / "error"). Per-statement,
// per-table, and per-error labels are forbidden — they would let any
// query introduced in a future commit blow up Prometheus cardinality.
//
// The matching collectors are registered in internal/metrics; the tracer
// reaches them through the SQLMetrics interface so this package doesn't
// import prometheus.
type QueryTracer struct {
	metrics SQLMetrics
}

// SQLMetrics is the narrow interface the tracer needs to record outcomes.
// It is satisfied by a prom* type in internal/metrics once Stage 2b wires
// up the actual collectors; for Stage 2a the tracer is constructed with
// nil metrics and is a no-op.
type SQLMetrics interface {
	RecordQuery(operation string, outcome string, duration time.Duration)
}

// NewQueryTracer constructs a tracer. metrics may be nil in tests, in
// which case the tracer becomes a no-op.
func NewQueryTracer(metrics SQLMetrics) *QueryTracer {
	return &QueryTracer{metrics: metrics}
}

type traceQueryContextKey struct{}

type traceQueryContext struct {
	start     time.Time
	operation string
}

// TraceQueryStart is the pgx callback invoked before a query executes. We
// stash the start time and a bounded operation keyword in the context;
// TraceQueryEnd reads them back. Returning the context (not the data
// pointer) is the documented pgx contract.
func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t == nil || t.metrics == nil {
		return ctx
	}
	return context.WithValue(ctx, traceQueryContextKey{}, &traceQueryContext{
		start:     time.Now(),
		operation: operationFromSQL(data.SQL),
	})
}

// TraceQueryEnd is the pgx callback invoked after a query finishes.
func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if t == nil || t.metrics == nil {
		return
	}
	state, ok := ctx.Value(traceQueryContextKey{}).(*traceQueryContext)
	if !ok {
		return
	}
	outcome := "ok"
	if data.Err != nil {
		outcome = "error"
	}
	t.metrics.RecordQuery(state.operation, outcome, time.Since(state.start))
}

// operationFromSQL extracts a bounded keyword from a SQL statement so the
// "operation" metric label stays at a small fixed cardinality. The
// returned value is one of:
//
//	select_<table>, insert_<table>, update_<table>, delete_<table>
//	migration, advisory_lock, advisory_unlock, ping, cron_schedule, other.
//
// "other" is the catch-all for anything not on the allowlist; an alert on
// `glimmung_pg_queries_total{operation="other"} > 0` surfaces unmapped
// SQL added by future commits without letting the cardinality grow with
// each new query string.
func operationFromSQL(sql string) string {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return "other"
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "select pg_advisory_lock"):
		return "advisory_lock"
	case strings.HasPrefix(lower, "select pg_advisory_unlock"):
		return "advisory_unlock"
	case strings.HasPrefix(lower, "create extension"),
		strings.HasPrefix(lower, "create table"),
		strings.HasPrefix(lower, "create index"),
		strings.HasPrefix(lower, "create unique index"),
		strings.HasPrefix(lower, "alter table"),
		strings.HasPrefix(lower, "drop "):
		return "migration"
	case strings.HasPrefix(lower, "select cron.schedule"),
		strings.HasPrefix(lower, "select cron.unschedule"):
		return "cron_schedule"
	case lower == "select 1", lower == "select 1;":
		return "ping"
	}
	verb := firstWord(lower)
	table := tableFromSQL(lower, verb)
	if table == "" {
		return "other"
	}
	switch verb {
	case "select":
		return "select_" + table
	case "insert":
		return "insert_" + table
	case "update":
		return "update_" + table
	case "delete":
		return "delete_" + table
	case "with":
		return "select_" + table
	}
	return "other"
}

func firstWord(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\n' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}

// knownTables is the closed set of tables the operationFromSQL extractor
// will match. Mirrors the schema in migrations.go. Adding a new table here
// is an explicit choice — the alert on `operation="other"` surfaces any
// unrecognized SQL before the new table's queries become invisible.
var knownTables = []string{
	"projects",
	"workflows",
	"workflow_schemas",
	"leases",
	"runs",
	"run_events",
	"locks",
	"signals",
	"issues",
	"issue_comments",
	"playbooks",
	"reports",
	"slots",
	"slot_history",
	"touchpoints",
}

// tableFromSQL pulls the first known table name out of a SELECT/INSERT/
// UPDATE/DELETE statement. The match is substring-based and case-folded;
// we never invent a table name from caller-controlled SQL text. If no
// known table appears, the caller falls back to "other" — which makes
// unmapped SQL surface as a single low-cardinality bucket rather than
// distinct labels per query.
func tableFromSQL(lower, verb string) string {
	switch verb {
	case "insert":
		idx := strings.Index(lower, "into ")
		if idx < 0 {
			return ""
		}
		return firstKnownTable(lower[idx+len("into "):])
	case "update":
		return firstKnownTable(lower[len("update "):])
	case "delete":
		idx := strings.Index(lower, "from ")
		if idx < 0 {
			return ""
		}
		return firstKnownTable(lower[idx+len("from "):])
	case "select", "with":
		idx := strings.Index(lower, "from ")
		if idx < 0 {
			return ""
		}
		return firstKnownTable(lower[idx+len("from "):])
	}
	return ""
}

func firstKnownTable(after string) string {
	after = strings.TrimSpace(after)
	if after == "" {
		return ""
	}
	for _, table := range knownTables {
		if strings.HasPrefix(after, table) {
			rest := after[len(table):]
			if rest == "" || rest[0] == ' ' || rest[0] == '\n' || rest[0] == '\t' || rest[0] == ',' || rest[0] == '(' {
				return table
			}
		}
	}
	return ""
}
