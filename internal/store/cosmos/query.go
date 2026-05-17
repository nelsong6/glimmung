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
package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
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
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			var row T
			if err := json.Unmarshal(item, &row); err != nil {
				return err
			}
			*target = append(*target, row)
		}
	}
	return nil
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
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			var row T
			if err := json.Unmarshal(item, &row); err != nil {
				return err
			}
			*target = append(*target, row)
		}
	}
	return nil
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
	for _, project := range projects {
		params := make([]azcosmos.QueryParameter, 0, len(parameters)+1)
		params = append(params, parameters...)
		params = append(params, azcosmos.QueryParameter{Name: "@project", Value: project})
		if err := singlePartitionQuery(
			ctx,
			container,
			azcosmos.NewPartitionKeyString(project),
			query,
			params,
			target,
		); err != nil {
			return err
		}
	}
	return nil
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
