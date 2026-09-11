// Schema verification for the telemetry connection.
package pg

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaReport is where a connection will actually read and write.
type SchemaReport struct {
	/// Schema an unqualified table name resolves to on this connection.
	Resolved string
	/// search_path as the server sees it, for when Resolved is a surprise.
	SearchPath string
	/// True when the table exists somewhere OTHER than Resolved as well.
	Shadowed bool
	/// Where the shadow copies live.
	ShadowedIn []string
}

/*
VerifyTable reports which schema `table` resolves to on this pool, and whether
a copy of it exists in another schema.

This exists because of a fault that produced no error anywhere. The services
share one database and separate by schema; the gateway took its schema from a
?search_path= DSN param, and when that param was missing the pings were written
to public.telemetry_timeseries while the fleet API read
iag_fleet.telemetry_timeseries. The table exists in both, so every insert
succeeded, every read returned an empty array, and a vehicle reporting every
twenty seconds had no history at all for as long as it ran.

Nothing in either service could see it. The gateway logged clean ingests. The
API returned 200. The fleet service already checks for missing COLUMNS at boot
(reportSchemaDrift) and that check passes perfectly when the table is simply the
wrong one.

So: resolve it once, at startup, and say so out loud.
*/
func VerifyTable(ctx context.Context, pool *pgxpool.Pool, table string) (SchemaReport, error) {
	var rep SchemaReport

	// to_regclass follows the live search_path, which is the only thing that
	// decides where an unqualified INSERT lands.
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(n.nspname, ''), current_setting('search_path')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`, table).Scan(&rep.Resolved, &rep.SearchPath); err != nil {
		return rep, fmt.Errorf("resolve %s: %w", table, err)
	}

	rows, err := pool.Query(ctx, `
		SELECT table_schema FROM information_schema.tables
		WHERE table_name = $1 AND table_schema <> $2
		ORDER BY table_schema`, table, rep.Resolved)
	if err != nil {
		return rep, fmt.Errorf("find shadow copies of %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return rep, err
		}
		rep.ShadowedIn = append(rep.ShadowedIn, schema)
	}
	rep.Shadowed = len(rep.ShadowedIn) > 0
	return rep, rows.Err()
}

/*
AssertTableSchema resolves `table` and refuses to start when it is not `want`.

Refusing rather than warning, deliberately. A gateway pointed at the wrong
schema does not fail — it accepts every frame, writes it where nobody reads,
and reports success. The device is happy, the log is clean, and the data is
gone. There is no later moment at which this becomes visible on its own, so the
only useful time to stop is now, the way autoMigrate refuses to serve on a
failed migration.

A shadow copy elsewhere is logged rather than fatal: it is the footprint of this
bug having already happened, and it may hold real history worth recovering.
Wrong is fatal; suspicious is loud.
*/
func AssertTableSchema(ctx context.Context, pool *pgxpool.Pool, table, want string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	rep, err := VerifyTable(ctx, pool, table)
	if err != nil {
		return err
	}
	if rep.Resolved == "" {
		// Not on the search_path at all. Two very different situations:
		//
		// Nowhere in the database — the migrations have not run yet. This
		// process does not own them (the fleet service does), so a fresh
		// environment can legitimately start the gateway first. Refusing here
		// would be a crash loop waiting on someone else's deploy, so it warns.
		//
		// Present in another schema — that IS the bug: the writes would go
		// somewhere nothing reads, or fail outright. Fatal.
		if !rep.Shadowed {
			log.Warn("telemetry table not found yet; assuming migrations have not run",
				"table", table, "search_path", rep.SearchPath)
			return nil
		}
		return fmt.Errorf(
			"%s is not on this connection's search_path (%q) but exists in %v — writes would not reach the schema readers use",
			table, rep.SearchPath, rep.ShadowedIn)
	}
	if rep.Resolved != want {
		return fmt.Errorf(
			"%s resolves to schema %q, not %q (search_path=%q) — writes here would be invisible to readers of %s.%s",
			table, rep.Resolved, want, rep.SearchPath, want, table)
	}
	if rep.Shadowed {
		log.Warn("another copy of this table exists in a different schema; it may hold history written before the schema was pinned",
			"table", table, "writing_to", rep.Resolved, "also_in", rep.ShadowedIn)
	}
	log.Info("telemetry schema verified", "table", table, "schema", rep.Resolved, "search_path", rep.SearchPath)
	return nil
}
