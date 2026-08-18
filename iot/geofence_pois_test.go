package iot

import "testing"

// An empty or failed load must fall back to the built-in POIs rather than
// silently disabling geofencing — losing site arrivals without an error is the
// failure mode worth guarding.
func TestActiveGeofencePOIs_fallsBackWhenUnset(t *testing.T) {
	SetGeofencePOIs(nil)
	if got := ActiveGeofencePOIs(); len(got) != len(DefaultGeofencePOIs) {
		t.Fatalf("got %d POIs, want the %d built-in defaults", len(got), len(DefaultGeofencePOIs))
	}
}

func TestSetGeofencePOIs_overridesAndIsolates(t *testing.T) {
	custom := []GeofencePOI{{"New Depot", 0.5, 32.6, "site", 1.2}}
	SetGeofencePOIs(custom)
	t.Cleanup(func() { SetGeofencePOIs(nil) })

	got := ActiveGeofencePOIs()
	if len(got) != 1 || got[0].Name != "New Depot" {
		t.Fatalf("got %+v, want the custom set", got)
	}

	// The stored set must be a copy: a caller mutating its slice afterwards
	// must not silently rewrite live geofences under the connection goroutines.
	custom[0].Name = "Mutated"
	if ActiveGeofencePOIs()[0].Name != "New Depot" {
		t.Fatal("SetGeofencePOIs must copy, not alias, the caller's slice")
	}
}

// A ping is evaluated against whatever set is active, so a DB-loaded POI must
// produce transitions exactly like a built-in one.
func TestProcessGeofences_usesActiveSet(t *testing.T) {
	SetGeofencePOIs([]GeofencePOI{{"New Depot", 0.5, 32.6, "site", 1.0}})
	t.Cleanup(func() { SetGeofencePOIs(nil) })

	tr := ProcessGeofences(Ping{VehicleID: "V1", Lat: 0.5005, Lng: 32.6005})
	if len(tr) != 1 {
		t.Fatalf("got %d transitions, want 1", len(tr))
	}
	if tr[0].POIName != "New Depot" || !tr[0].Entered {
		t.Fatalf("got %+v, want to be inside New Depot", tr[0])
	}
}
