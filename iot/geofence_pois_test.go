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
	custom := []GeofencePOI{{Name: "New Depot", Lat: 0.5, Lng: 32.6, Type: "site", RadiusKm: 1.2}}
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
	SetGeofencePOIs([]GeofencePOI{{Name: "New Depot", Lat: 0.5, Lng: 32.6, Type: "site", RadiusKm: 1.0}})
	t.Cleanup(func() { SetGeofencePOIs(nil) })

	tr := ProcessGeofences(Ping{VehicleID: "V1", Lat: 0.5005, Lng: 32.6005})
	if len(tr) != 1 {
		t.Fatalf("got %d transitions, want 1", len(tr))
	}
	if tr[0].POIName != "New Depot" || !tr[0].Entered {
		t.Fatalf("got %+v, want to be inside New Depot", tr[0])
	}
}

// Deleting the last geofence has to mean there are no geofences.
//
// Before fences were editable, an empty active set could only mean "not loaded
// yet", so falling back to the built-in six was right. Once an operator can
// delete one it is ambiguous, and guessing wrong is the worse failure: the six
// defaults would quietly come back, vehicles would be judged against fences
// nobody could see in the management view, and arrivals would fire for sites
// the operator believed they had removed.
func TestEmptyAfterLoadMeansNoGeofences(t *testing.T) {
	t.Cleanup(func() { SetGeofencePOIs(nil); poisLoaded.Store(false) })

	SetGeofencePOIs(nil)
	poisLoaded.Store(false)
	if got := len(ActiveGeofencePOIs()); got != len(DefaultGeofencePOIs) {
		t.Fatalf("before any load: %d POIs, want the %d built-in defaults", got, len(DefaultGeofencePOIs))
	}

	SetGeofencePOIsLoaded(nil)
	if got := ActiveGeofencePOIs(); len(got) != 0 {
		t.Fatalf("after loading an empty table: %d POIs, want none — the defaults came back", len(got))
	}
}

// A load that never succeeded must not be mistaken for an empty table.
func TestLoadFailureKeepsTheBuiltInSet(t *testing.T) {
	t.Cleanup(func() { SetGeofencePOIs(nil); poisLoaded.Store(false) })
	SetGeofencePOIs(nil)
	poisLoaded.Store(false)
	if got := len(ActiveGeofencePOIs()); got == 0 {
		t.Fatal("no fences before the first successful load — a gateway that cannot reach the database would stop recording arrivals")
	}
}

// Scoping a fence to particular vehicles.
//
// Empty means EVERY vehicle, not none. Getting that default backwards would
// have switched off monitoring for every fence that already existed, silently,
// on the day this shipped.
func TestAppliesTo(t *testing.T) {
	all := GeofencePOI{Name: "Depot"}
	if !all.AppliesTo("V1") || !all.AppliesTo("anything") {
		t.Fatal("an unscoped fence must apply to every vehicle")
	}

	scoped := GeofencePOI{Name: "Client A", VehicleIDs: []string{"V1", "V2"}}
	if !scoped.AppliesTo("V1") || !scoped.AppliesTo("V2") {
		t.Fatal("an assigned vehicle must be evaluated")
	}
	if scoped.AppliesTo("V3") {
		t.Fatal("a vehicle that is not assigned must not be evaluated")
	}
}

func TestProcessGeofences_skipsUnassignedVehicles(t *testing.T) {
	// The point of the whole feature: a customer-site fence used to raise
	// arrivals for all 37 trucks, and the events for the two that serve the
	// site were lost in the noise from the thirty-five that never go there.
	SetGeofencePOIs([]GeofencePOI{
		{Name: "Client A", Lat: 0.5, Lng: 32.6, Type: "site", RadiusKm: 1.0, VehicleIDs: []string{"V1"}},
	})
	t.Cleanup(func() { SetGeofencePOIs(nil) })

	at := Ping{Lat: 0.5005, Lng: 32.6005}

	at.VehicleID = "V1"
	if got := ProcessGeofences(at); len(got) != 1 {
		t.Fatalf("assigned vehicle: %d transitions, want 1", len(got))
	}
	at.VehicleID = "V2"
	if got := ProcessGeofences(at); len(got) != 0 {
		t.Fatalf("unassigned vehicle: %d transitions, want 0", len(got))
	}
}

func TestProcessGeofences_carriesTheRule(t *testing.T) {
	SetGeofencePOIs([]GeofencePOI{
		{Name: "Corridor", Lat: 0.5, Lng: 32.6, Type: "site", RadiusKm: 1.0, Rule: RuleStayInside},
	})
	t.Cleanup(func() { SetGeofencePOIs(nil) })
	got := ProcessGeofences(Ping{VehicleID: "V1", Lat: 0.5005, Lng: 32.6005})
	if len(got) != 1 || got[0].Rule != RuleStayInside {
		t.Fatalf("rule not carried into the transition: %+v", got)
	}
}

// Which direction of crossing is worth waking someone for.
func TestBreaches(t *testing.T) {
	cases := []struct {
		rule            GeofenceRule
		onEnter, onExit bool
	}{
		// Watch reports both, exactly as every fence did before rules existed.
		{RuleWatch, true, true},
		// A permitted area: leaving is the breach, arriving back is not news.
		{RuleStayInside, false, true},
		// A restricted zone: entering is the breach, leaving is the relief.
		{RuleNoEntry, true, false},
		// Anything unreadable behaves as watch — a fence that silently stopped
		// watching would be worse than one watching with the wrong verb.
		{GeofenceRule("nonsense"), true, true},
		{GeofenceRule(""), true, true},
	}
	for _, tc := range cases {
		p := GeofencePOI{Rule: tc.rule}
		if got := p.Breaches(true); got != tc.onEnter {
			t.Errorf("%q on enter = %v, want %v", tc.rule, got, tc.onEnter)
		}
		if got := p.Breaches(false); got != tc.onExit {
			t.Errorf("%q on exit = %v, want %v", tc.rule, got, tc.onExit)
		}
	}
}

func TestParseGeofenceRule(t *testing.T) {
	for in, want := range map[string]GeofenceRule{
		"stay_inside": RuleStayInside,
		"STAY_INSIDE": RuleStayInside,
		" no_entry ":  RuleNoEntry,
		"watch":       RuleWatch,
		"":            RuleWatch,
		"garbage":     RuleWatch,
	} {
		if got := ParseGeofenceRule(in); got != want {
			t.Errorf("ParseGeofenceRule(%q) = %q, want %q", in, got, want)
		}
	}
}
