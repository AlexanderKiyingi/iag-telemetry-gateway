package iot

import "testing"

// A (0,0) position is a GPS no-fix sentinel and must not produce geofence
// transitions (which would fabricate spurious enter/exit safety events).
func TestProcessGeofences_skipsNoFix(t *testing.T) {
	if got := ProcessGeofences(Ping{VehicleID: "V1", Lat: 0, Lng: 0}); got != nil {
		t.Fatalf("expected nil transitions for (0,0) no-fix, got %d", len(got))
	}
}
