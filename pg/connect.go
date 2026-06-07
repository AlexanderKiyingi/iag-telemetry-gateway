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
