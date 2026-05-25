// Package pg is the shared Postgres connection layer for glimmung's durable
// stores. It builds a pgxpool.Pool whose connections present a fresh Azure
// AD access token as the password on every dial, so the glimmung pod
// authenticates to Azure Database for PostgreSQL through its workload
// identity instead of a static admin password.
//
// The Azure AD resource ID for the OSS RDBMS service is fixed
// (`https://ossrdbms-aad.database.windows.net/.default`). Tokens expire roughly
// every hour, so connections are recycled before that lifetime so pgx never
// presents an expired credential. Schema-migration runner lives in this
// package; per-table stores will be added in subsequent migration stages
// (see docs/postgres-migration.md).
//
// Stage 2a scope: foundation only. This package exists, the pool can be
// constructed, RunMigrations applies all 12 tables — but no consumer in
// internal/server/ uses it yet. The runtime still reads/writes Cosmos.
package pg

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is a re-export of *pgxpool.Pool so callers don't need to import
// pgxpool directly just to hold a reference to the connection pool. The
// underlying type is unchanged; this is a type alias only.
type Pool = pgxpool.Pool

// AADTokenScope is the resource identifier for Azure Database for PostgreSQL's
// AAD-issued access tokens. It is a fixed Microsoft-owned value, not a per-server
// or per-tenant string. The trailing `/.default` selects the v2 token endpoint.
const AADTokenScope = "https://ossrdbms-aad.database.windows.net/.default"

// MaxConnLifetime is bounded below the AAD access-token TTL (~60 minutes) so a
// connection never holds a token past its expiry. Refreshes happen transparently
// inside BeforeConnect when pgx recycles the connection.
const MaxConnLifetime = 50 * time.Minute

// Config describes how to reach the glimmung Postgres Flexible Server.
// Username is the AAD principal name as it appears in the server's
// `pg_authid` (for a UAMI, this is the UAMI's name — i.e.
// "glimmung-identity").
//
// QueryMetrics is optional; when non-nil every query the pool runs is
// observed by the tracer (operation + outcome + duration). Tests can pass
// nil to opt out.
type Config struct {
	Host         string
	Database     string
	Username     string
	Credential   azcore.TokenCredential
	QueryMetrics SQLMetrics
}

// NewPool builds a pgxpool.Pool wired with AAD authentication. The pool
// validates one connection synchronously before returning so misconfiguration
// fails fast at startup rather than on first request.
//
// MaxConns is bounded at 6 to fit comfortably under glimmung-pg's B1ms
// default max_connections (~50) with room for multiple replicas and
// out-of-band psql sessions. The tier-upgrade trigger documented in
// docs/postgres-migration.md alerts on pg_stat_activity > 35 sustained
// 10 min — that's the signal to either reduce MaxConns or bump tier.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	host := strings.TrimSpace(cfg.Host)
	database := strings.TrimSpace(cfg.Database)
	username := strings.TrimSpace(cfg.Username)
	if host == "" || database == "" || username == "" {
		return nil, fmt.Errorf("pg: host, database, and username are required")
	}
	if cfg.Credential == nil {
		return nil, fmt.Errorf("pg: azcore.TokenCredential is required")
	}

	// Construct a libpq-style DSN. The password is set per-connection by
	// BeforeConnect, so the static URL omits it. sslmode=require is mandatory
	// for Flexible Server's public endpoint.
	dsn := fmt.Sprintf(
		"postgres://%s@%s/%s?sslmode=require",
		url.QueryEscape(username),
		host,
		url.QueryEscape(database),
	)
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse DSN: %w", err)
	}
	poolConfig.MaxConnLifetime = MaxConnLifetime
	poolConfig.MaxConns = 6
	poolConfig.MinConns = 1
	if cfg.QueryMetrics != nil {
		poolConfig.ConnConfig.Tracer = NewQueryTracer(cfg.QueryMetrics)
	}

	credential := cfg.Credential
	poolConfig.BeforeConnect = func(ctx context.Context, c *pgx.ConnConfig) error {
		tok, err := credential.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{AADTokenScope},
		})
		if err != nil {
			return fmt.Errorf("pg: acquire AAD token: %w", err)
		}
		c.Password = tok.Token
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("pg: build pool: %w", err)
	}

	ping, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(ping); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}

	return pool, nil
}
