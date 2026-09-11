// Package pg opens Postgres pools for fleet telemetry ingest.
package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SearchPath is the schema this process reads and writes.
//
// PG_SEARCH_PATH overrides it for a deployment that separates differently; the
// default matches the fleet service, which is the only other writer of these
// tables. `public` stays on the end so shared extensions and types resolve.
func SearchPath() string {
	if v := strings.TrimSpace(os.Getenv("PG_SEARCH_PATH")); v != "" {
		return v
	}
	return "iag_fleet, public"
}

// Connect parses DATABASE_URL and pings Postgres.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		return nil, errors.New("DATABASE_URL is empty")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// Pin the schema in code rather than trusting the DSN.
	//
	// The services share one database and separate by schema, and this process
	// was the only one taking the schema purely from a ?search_path= param. When
	// that param is missing the writes land in `public` instead of `iag_fleet` —
	// and because the table exists in both, nothing errors. The gateway logs a
	// clean ingest, the fleet API reads iag_fleet.telemetry_timeseries and finds
	// nothing, and a fleet reporting every twenty seconds has no history at all.
	//
	// That is exactly what happened: a vehicle whose hot state updated seven
	// seconds ago returned zero pings over 72 hours. Hot state goes through the
	// registry connection and kept working, which is what made it look like a
	// missing feature rather than a misdirected write.
	//
	// The fleet service already does this (internal/db/db.go) and its migrator
	// carries a safety net for "deployments whose DATABASE_URL ever lacked the
	// ?search_path= param". This side had neither defence. Matching it means the
	// two agree by default instead of by configuration.
	cfg.ConnConfig.RuntimeParams["search_path"] = SearchPath()

	cfg.MaxConns = intEnv("DB_MAX_CONNS", 30)
	cfg.MinConns = intEnv("DB_MIN_CONNS", 2)
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// ConnectSplit opens registry (REGISTRY_DATABASE_URL) and telemetry (DATABASE_URL) pools.
// When REGISTRY_DATABASE_URL is unset, both pointers reference the same pool (single-DB dev).
func ConnectSplit(ctx context.Context) (registry, telemetry *pgxpool.Pool, err error) {
	telemetry, err = Connect(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	registryURL := strings.TrimSpace(os.Getenv("REGISTRY_DATABASE_URL"))
	if registryURL == "" {
		return telemetry, telemetry, nil
	}
	registry, err = Connect(ctx, registryURL)
	if err != nil {
		telemetry.Close()
		return nil, nil, err
	}
	return registry, telemetry, nil
}

func intEnv(key string, fallback int32) int32 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return int32(n)
}
