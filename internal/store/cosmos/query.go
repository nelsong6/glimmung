// Package-internal Cosmos query primitives.
//
// Every Cosmos query in this package goes through one of three helpers
// declared here. Each helper requires the caller to be explicit about
// partition strategy at the call site. The legacy `queryAll` /
// `queryAllWhere` pair that defaulted to an empty partition key has been
// deleted; the migration guard at scripts/check-cosmos-queries.sh fails
// CI if it returns.
//
// The Azure Go SDK (azcosmos) does not implement the client-side query
// plan handshake that newer C#/Java/JS SDKs use to fan out cross-partition
// queries with ORDER BY / DISTINCT / GROUP BY / OFFSET / TOP. The Cosmos
// gateway returns the well-known 400 "The provided cross partition query
// can not be directly served by the gateway" for those shapes, and the
// SDK surfaces it as an error rather than handling it transparently. The
// production symptom this fixes is a 5xx-per-minute on `GET /v1/touchpoints`
// caused by `SELECT * FROM c ORDER BY c.updated_at DESC` against the
// `reports` container with an empty partition key.
//
// See docs/cosmos-partition-strategy.md for the partition key inventory
// and the rules for choosing between these helpers.
//
// Per-query observability — Prometheus counters/histograms and structured
// slog on error — lives in instrumentPager below and the metrics package
// (RecordCosmosQuery / RecordCosmosFanoutPartition). The dedicated
// glimmung_cosmos_query_plan_error_total counter fires when a Cosmos 400
// matches the query-plan failure shape so a regression of the original
// bug surfaces on a dashboard, not as opaque 5xx logs.
package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/nelsong6/glimmung/internal/metrics"
)

// singlePartitionQuery executes a SQL query against one partition of
// container, identified by pk. Use this for any query whose predicates
// scope to a single partition-key value — including ORDER BY, DISTINCT,
// GROUP BY, OFFSET, and TOP, which all work locally inside one partition.
//
// pk must be a real partition key (NewPartitionKeyString / Bool / Number).
// Passing azcosmos.NewPartitionKey() (the empty/null key) silently turns
// the query into a cross-partition scan, which is the failure mode this
// helper exists to prevent.
func singlePartitionQuery[T any](
	ctx context.Context,
	container *azcosmos.ContainerClient,
	pk azcosmos.PartitionKey,
	query string,
	parameters []azcosmos.QueryParameter,
	target *[]T,
) error {
	pager := container.NewQueryItemsPager(query, pk, &azcosmos.QueryOptions{
		QueryParameters: parameters,
	})
	return instrumentPager(ctx, container, metrics.CosmosQueryModeSingle, query, pager, target)
}

// crossPartitionQuery executes a SQL query across every physical partition
// of container in a single round-trip. Use this only for queries whose
// shape the Cosmos gateway can serve directly: simple WHERE filters with
// no ORDER BY, DISTINCT, GROUP BY, OFFSET, or TOP. These queries either
// return a single doc (secondary-index lookups by id, callback token,
// etc.) or a small unordered set that the caller will sort in Go.
//
// If the query needs server-side ordering or aggregation, use
// fanOutByProject (or a comparable explicit fan-out) instead — the Go
// SDK cannot handle the gateway's query-plan response for those shapes
// and will surface a Cosmos 400 to the handler layer.
//
// The query must not reference the @project parameter; cross-partition
// queries by definition do not bind a partition key, and the helper
// rejects accidental misuse.
func crossPartitionQuery[T any](
	ctx context.Context,
	container *azcosmos.ContainerClient,
	query string,
	parameters []azcosmos.QueryParameter,
	target *[]T,
) error {
	if err := rejectOrderingClauses(query); err != nil {
		return fmt.Errorf("crossPartitionQuery: %w", err)
	}
	pager := container.NewQueryItemsPager(query, azcosmos.NewPartitionKey(), &azcosmos.QueryOptions{
		QueryParameters: parameters,
	})
	return instrumentPager(ctx, container, metrics.CosmosQueryModeCross, query, pager, target)
}

// fanOutByProject runs query once per project partition and appends the
// merged docs to target. The query must contain a WHERE clause that
// binds @project; the helper supplies the parameter value per iteration
// and the partition key to scope each query to one partition.
//
// Use this for cross-project list views that require ORDER BY / TOP /
// pagination semantics the gateway cannot serve cross-partition. Caller
// owns final merge ordering and limiting (sort the merged slice in Go).
//
// projects must be the live list of project partition-key values
// (NewPartitionKeyString equivalents). It is required and not derived
// implicitly so callers can scope to a subset (e.g. the projects a user
// has open) instead of always querying every project.
//
// If parameters already binds @project (caller mistake), fanOutByProject
// returns an error rather than silently double-binding.
func fanOutByProject[T any](
	ctx context.Context,
	container *azcosmos.ContainerClient,
	projects []string,
	query string,
	parameters []azcosmos.QueryParameter,
	target *[]T,
) error {
	if !strings.Contains(query, "@project") {
		return errors.New("fanOutByProject: query must reference @project")
	}
	for _, p := range parameters {
		if p.Name == "@project" {
			return errors.New("fanOutByProject: @project must not be pre-bound in parameters")
		}
	}
	// Aggregate observability across every per-partition iteration so the
	// metric reflects the whole fan-out call as one operation. Per-
	// partition iterations are also counted on the fan-out counter, which
	// gives the dashboard an observed fan-out factor when divided by the
	// fanout-mode queries_total.
	start := time.Now()
	var totalRU float64
	containerName := containerName(container)
	for _, project := range projects {
		metrics.RecordCosmosFanoutPartition(containerName)
		params := make([]azcosmos.QueryParameter, 0, len(parameters)+1)
		params = append(params, parameters...)
		params = append(params, azcosmos.QueryParameter{Name: "@project", Value: project})
		pager := container.NewQueryItemsPager(
			query,
			azcosmos.NewPartitionKeyString(project),
			&azcosmos.QueryOptions{QueryParameters: params},
		)
		ru, err := drainPagerInto(ctx, pager, target)
		totalRU += ru
		if err != nil {
			outcome := metrics.CosmosQueryOutcomeError
			metrics.RecordCosmosQuery(containerName, metrics.CosmosQueryModeFanout, time.Since(start), totalRU, outcome, isQueryPlanError(err))
			slog.Error("cosmos query failed",
				"container", containerName,
				"mode", metrics.CosmosQueryModeFanout,
				"partitions_scanned", project, // last partition touched
				"duration_ms", time.Since(start).Milliseconds(),
				"ru_charge", totalRU,
				"query_plan_error", isQueryPlanError(err),
				"err", err,
			)
			return err
		}
	}
	metrics.RecordCosmosQuery(containerName, metrics.CosmosQueryModeFanout, time.Since(start), totalRU, metrics.CosmosQueryOutcomeSuccess, false)
	return nil
}

// instrumentPager wraps a single-pager loop (single- or cross-partition)
// with duration + RU + outcome recording and structured error logging.
// fanOutByProject does its own bookkeeping because it aggregates across
// multiple pagers; both paths agree on the metric and slog field shapes.
func instrumentPager[T any](
	ctx context.Context,
	container *azcosmos.ContainerClient,
	mode string,
	query string,
	pager pager,
	target *[]T,
) error {
	start := time.Now()
	containerName := containerName(container)
	ru, err := drainPagerInto(ctx, pager, target)
	outcome := metrics.CosmosQueryOutcomeSuccess
	if err != nil {
		outcome = metrics.CosmosQueryOutcomeError
		slog.Error("cosmos query failed",
			"container", containerName,
			"mode", mode,
			"duration_ms", time.Since(start).Milliseconds(),
			"ru_charge", ru,
			"query_plan_error", isQueryPlanError(err),
			"query", redactedQueryShape(query),
			"err", err,
		)
	}
	metrics.RecordCosmosQuery(containerName, mode, time.Since(start), ru, outcome, err != nil && isQueryPlanError(err))
	return err
}

// pager is the slice of azcosmos.NewQueryItemsPager / NewPartitionedQueryItemsPager
// that instrumentPager and drainPagerInto care about. Declaring it locally
// avoids leaking the runtime generic type back through public signatures.
type pager interface {
	More() bool
	NextPage(ctx context.Context) (azcosmos.QueryItemsResponse, error)
}

// drainPagerInto runs pager.More / NextPage to completion, decodes each
// item into T, appends to target, and returns the summed RU charge.
func drainPagerInto[T any](ctx context.Context, p pager, target *[]T) (float64, error) {
	var ru float64
	for p.More() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return ru, err
		}
		ru += float64(page.RequestCharge)
		for _, item := range page.Items {
			var row T
			if err := json.Unmarshal(item, &row); err != nil {
				return ru, err
			}
			*target = append(*target, row)
		}
	}
	return ru, nil
}

// containerName extracts the short container id (e.g. "reports") from an
// azcosmos.ContainerClient so the metric label and slog field reflect
// which collection a query touched without threading the name through
// every helper signature.
func containerName(c *azcosmos.ContainerClient) string {
	if c == nil {
		return ""
	}
	return c.ID()
}

// isQueryPlanError detects the Cosmos gateway's 400 BadRequest response
// for cross-partition queries that require a client-side query plan the
// Go SDK does not implement. The error body is a fixed phrase Microsoft
// has shipped for years; matching on it is the cheapest reliable signal
// short of parsing the SDK error type, which is internal.
func isQueryPlanError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "cross partition query can not be directly served")
}

// redactedQueryShape returns a compact rendering of the SQL query
// suitable for log inspection but free of bound values and identifiers
// that might leak partition-key contents. The full SQL text is kept
// because parameters are bound separately by the SDK; identifiers like
// container fields are not sensitive on their own.
func redactedQueryShape(query string) string {
	q := strings.Join(strings.Fields(query), " ")
	if len(q) > 240 {
		q = q[:237] + "..."
	}
	return q
}

// rejectOrderingClauses returns an error if query uses a clause that the
// Cosmos gateway cannot serve cross-partition without a client-side query
// plan. The check is intentionally conservative: it matches case-insensitive
// keywords surrounded by whitespace so identifiers like c.order_by_field do
// not trip it.
func rejectOrderingClauses(query string) error {
	upper := strings.ToUpper(query)
	for _, kw := range []string{" ORDER BY ", " GROUP BY ", " DISTINCT ", " OFFSET ", " TOP "} {
		if strings.Contains(upper, kw) {
			return fmt.Errorf("query uses %q which requires a client-side query plan; use singlePartitionQuery or fanOutByProject", strings.TrimSpace(kw))
		}
	}
	return nil
}
