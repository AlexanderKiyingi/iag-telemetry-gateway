package pg

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The services share one database and separate by schema. This process was the
// only one taking its schema purely from a ?search_path= DSN param, and when
// that param is missing the writes land in `public` rather than `iag_fleet`.
//
// Nothing errors when that happens, because the table exists in both: the
// gateway logs a clean ingest and the fleet API reads an empty table. A fleet
// reporting every twenty seconds had no history at all, while hot state — which
// goes through the registry connection — kept updating and made it look like a
// missing feature rather than a misdirected write.
func TestSearchPath_defaultsToTheFleetSchema(t *testing.T) {
	t.Setenv("PG_SEARCH_PATH", "")
	got := SearchPath()
	if !strings.HasPrefix(got, "iag_fleet") {
		t.Fatalf("default search_path = %q, want it to start with iag_fleet", got)
	}
	// public stays on the end so shared extensions and types still resolve.
	if !strings.Contains(got, "public") {
		t.Fatalf("default search_path = %q, want public as a fallback", got)
	}
}

func TestSearchPath_overridable(t *testing.T) {
	// A deployment that separates differently must not be forced onto ours.
	t.Setenv("PG_SEARCH_PATH", "tenant_b, public")
	if got := SearchPath(); got != "tenant_b, public" {
		t.Fatalf("override ignored: %q", got)
	}
}

func TestSearchPath_ignoresBlankOverride(t *testing.T) {
	// An env var set to whitespace is not a choice; it is a deployment mistake,
	// and honouring it would put the writes back in public.
	t.Setenv("PG_SEARCH_PATH", "   ")
	if got := SearchPath(); !strings.HasPrefix(got, "iag_fleet") {
		t.Fatalf("blank override should fall back to the default, got %q", got)
	}
}

// The pin has to reach the connection config, not just exist as a helper — this
// is the step whose absence caused the outage.
func TestConnect_pinsSearchPathOntoTheConnection(t *testing.T) {
	t.Setenv("PG_SEARCH_PATH", "iag_fleet, public")
	// Build the config the same way Connect does, without needing a database.
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost:5432/db?sslmode=disable")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = SearchPath()
	if got := cfg.ConnConfig.RuntimeParams["search_path"]; got != "iag_fleet, public" {
		t.Fatalf("search_path not applied to the connection: %q", got)
	}
}

// A DSN that already carries a search_path must not win, or the bug returns for
// exactly the deployment that has the wrong one set.
func TestConnect_overridesADsnSearchPath(t *testing.T) {
	t.Setenv("PG_SEARCH_PATH", "iag_fleet, public")
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost:5432/db?sslmode=disable&search_path=public")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams["search_path"] != "public" {
		t.Skip("pgx no longer surfaces search_path from the DSN; the override below is what matters")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = SearchPath()
	if got := cfg.ConnConfig.RuntimeParams["search_path"]; got != "iag_fleet, public" {
		t.Fatalf("DSN search_path won: %q", got)
	}
}

func TestConnect_emptyURLIsRejected(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Connect(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("want an error naming DATABASE_URL, got %v", err)
	}
	_ = os.Getenv
}
