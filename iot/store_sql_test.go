package iot

import (
	"regexp"
	"strings"
	"testing"
)

// Fleet migration 0043 retyped iot_devices.vehicle_id and
// device_commands.vehicle_id from TEXT to uuid. Comparing a parameter with the
// text literal ” pins that parameter's type to text, so NULLIF($n, ”) is a
// TEXT expression — and assigning one to a uuid column is rejected outright:
//
//	column "vehicle_id" is of type uuid but expression is of type text
//	(SQLSTATE 42804)
//
// Registering any IoT device failed on exactly that. The ::uuid casts are what
// fix it, and they are one deletion away from coming back.
//
// This asserts on the SQL text rather than running it because CI for this repo
// runs `go test ./...` with no Postgres: the integration tests skip, so nothing
// else here would notice. A shape test that always runs beats a behaviour test
// that never does.
func TestDeviceWritesCastVehicleIDToUUID(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"CreateDevice", createDeviceSQL},
		{"UpdateDevice", updateDeviceSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.sql, "::uuid") {
				t.Fatalf("no ::uuid cast anywhere in the statement:\n%s", tc.sql)
			}
			// Every NULLIF that feeds vehicle_id must carry the cast. Catching
			// "a cast exists somewhere" is not enough — the bug was one
			// specific column missing one.
			for _, m := range regexp.MustCompile(`NULLIF\([^)]*\)(::\w+)?`).FindAllString(tc.sql, -1) {
				if strings.Contains(m, "$3") || strings.Contains(m, "$5::text") {
					if !strings.HasSuffix(m, "::uuid") {
						t.Fatalf("vehicle_id parameter is not cast to uuid: %s", m)
					}
				}
			}
		})
	}
}

// The same retype hit device_commands.vehicle_id, and the same NULLIF pattern
// was there. Kept in this file so the two are found together.
func TestCommandInsertCastsVehicleIDToUUID(t *testing.T) {
	if !strings.Contains(insertCommandSQL, "NULLIF($2,'')::uuid") {
		t.Fatalf("device_commands insert does not cast vehicle_id to uuid:\n%s", insertCommandSQL)
	}
}
