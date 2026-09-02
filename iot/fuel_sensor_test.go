package iot

import "testing"

// The default has to reproduce exactly what was hardcoded in the gateway before
// migration 0047 (IO 89, value/10), or applying the migration silently changes
// every existing device's fuel readings.
func TestDefaultFuelSensorMatchesTheOldHardcodedBehaviour(t *testing.T) {
	d := &Device{FuelIOID: DefaultFuelIOID, FuelScale: DefaultFuelScale}
	got, ok := d.FuelSensor().FuelFrom(map[uint16]int64{89: 623})
	if !ok {
		t.Fatal("default sensor did not read IO 89")
	}
	// Within float64 noise, not bit-identical: the old code divided by 10, this
	// multiplies by a configurable 0.1, and 623*0.1 lands one ulp off 623/10.0.
	// A femtopercent of diesel is not a difference anything downstream can see —
	// the fuel-event detector compares deltas against whole-percent thresholds —
	// but asserting exact equality would be asserting something untrue.
	if diff := got - 62.3; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("got %v%%, want 62.3%% (623 tenths, as the old constant did)", got)
	}
}

// A device row loaded by a code path that forgot the new columns arrives with a
// zero scale. Treating that as "sensor disabled" would silently stop fuel on
// exactly the paths most likely to be missed, so it falls back to the default.
func TestZeroScaleFallsBackRatherThanDisabling(t *testing.T) {
	d := &Device{}
	s := d.FuelSensor()
	if s.IOID != DefaultFuelIOID || s.Scale != DefaultFuelScale {
		t.Fatalf("zero-valued device gave %+v, want the default sensor", s)
	}
	if _, ok := s.FuelFrom(map[uint16]int64{89: 500}); !ok {
		t.Error("fallback sensor did not read a fuel value")
	}
}

func TestFuelSensorSourceMappings(t *testing.T) {
	cases := []struct {
		name   string
		sensor FuelSensor
		ios    map[uint16]int64
		want   float64
	}{
		{
			// CAN reports tenths of a percent.
			name:   "teltonika CAN percent",
			sensor: FuelSensor{IOID: 89, Scale: 0.1},
			ios:    map[uint16]int64{89: 1000},
			want:   100,
		},
		{
			// An LLS probe reports a raw count over its full range.
			name:   "LLS probe 0-4095",
			sensor: FuelSensor{IOID: 201, Scale: 100.0 / 4095.0},
			ios:    map[uint16]int64{201: 2048},
			want:   50.01221001221001,
		},
		{
			// A 0.5-4.5 V analog sender reads 500 mV at empty, so the offset
			// cancels the dead band rather than reporting a 12.5% floor.
			name:   "analog sender, empty tank",
			sensor: FuelSensor{IOID: 9, Scale: 0.025, Offset: -12.5},
			ios:    map[uint16]int64{9: 500},
			want:   0,
		},
		{
			name:   "analog sender, full tank",
			sensor: FuelSensor{IOID: 9, Scale: 0.025, Offset: -12.5},
			ios:    map[uint16]int64{9: 4500},
			want:   100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.sensor.FuelFrom(tc.ios)
			if !ok {
				t.Fatal("no reading")
			}
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// An out-of-range reading must not become a fuel event. The detector works on
// the delta between consecutive readings, so an unclamped 140 followed by a
// real 95 is a 45-point drop — indistinguishable from siphoning.
func TestFuelPercentClamps(t *testing.T) {
	s := FuelSensor{IOID: 9, Scale: 0.1}
	if got := s.Percent(2000); got != 100 {
		t.Errorf("over-range gave %v, want 100", got)
	}
	if got := (FuelSensor{IOID: 9, Scale: 0.1, Offset: -50}).Percent(0); got != 0 {
		t.Errorf("under-range gave %v, want 0", got)
	}
}

// "No fuel element in this frame" and "the tank is empty" must not look the
// same: a false zero is a 100-point drop to the fuel-event detector.
func TestMissingElementYieldsNoReading(t *testing.T) {
	s := FuelSensor{IOID: 201, Scale: 0.1}
	if _, ok := s.FuelFrom(map[uint16]int64{89: 500}); ok {
		t.Error("reported a reading from an element that was not present")
	}
	if _, ok := s.FuelFrom(nil); ok {
		t.Error("reported a reading from an empty IO map")
	}
}

// A vehicle with no fuel sensor should say so rather than report 0%.
func TestDisabledSensorYieldsNoReading(t *testing.T) {
	d := &Device{FuelIOID: 0, FuelScale: 0.1}
	if _, ok := d.FuelSensor().FuelFrom(map[uint16]int64{0: 900, 89: 900}); ok {
		t.Error("a device with fuel decoding disabled still reported a level")
	}
}
