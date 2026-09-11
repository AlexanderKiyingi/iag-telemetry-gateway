package iot

import (
	"testing"

	"github.com/iag/fleet-iot/pg"
)

// pg cannot import iot without a cycle, so the pings table name is written in
// both places. This is the guard that keeps them the same.
//
// If they drift, the boot assertion silently starts checking a table nothing
// writes — and a check that passes for the wrong reason is worse than no check,
// because it is the thing standing between a misconfigured gateway and a month
// of telemetry written where nobody reads it.
func TestPingsTableNameMatchesTheSchemaAssertion(t *testing.T) {
	if pg.PingsTableName != PingsTable {
		t.Fatalf("pg.PingsTableName = %q but iot.PingsTable = %q — the boot check would verify the wrong table",
			pg.PingsTableName, PingsTable)
	}
}
