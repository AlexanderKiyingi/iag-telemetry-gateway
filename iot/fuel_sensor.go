package iot

// Per-device fuel sensor mapping.
//
// The Teltonika gateway used to read fuel from one hardcoded IO id — 89, CAN
// fuel level in tenths of a percent — which exists only on a unit wired to a
// CAN adapter. The two sensors an operator actually fits to a truck without
// CAN, an analog sender on AIN1 and an LLS capacitive probe on RS232/RS485,
// report on different ids in different units and were simply not read, even
// though the gateway was already storing them in the raw IO map.
//
// So which id carries fuel, and how its raw units become a percentage, is a
// property of the device, not a constant. See fleet migration 0047.

// DefaultFuelIOID is Teltonika CAN fuel level (percent, in tenths). It is the
// default for every device so the mapping is a no-op on an existing fleet.
const DefaultFuelIOID = 89

// DefaultFuelScale converts tenths-of-a-percent to percent.
const DefaultFuelScale = 0.1

// FuelSensor is the per-device mapping from a raw IO value to a fuel percentage.
type FuelSensor struct {
	// IOID is the protocol IO element carrying fuel. 0 disables decoding.
	IOID uint16
	// Scale multiplies the raw value; Offset is added after.
	Scale  float64
	Offset float64
}

// DefaultFuelSensor reproduces the behaviour that was hardcoded in the gateway.
func DefaultFuelSensor() FuelSensor {
	return FuelSensor{IOID: DefaultFuelIOID, Scale: DefaultFuelScale}
}

// Percent maps a raw IO value onto 0-100.
//
// The clamp is not cosmetic. A miscalibrated sender, or a probe reading out of
// range while the tank is being filled, produces values outside 0-100; the
// fuel-event detector works on the delta between consecutive readings, so an
// unclamped 140% followed by a real 95% is a 45-point drop, which is exactly
// the shape of a siphoning alert. Clamping cannot make a bad calibration
// correct, but it stops one manufacturing theft alarms.
func (f FuelSensor) Percent(raw int64) float64 {
	pct := float64(raw)*f.Scale + f.Offset
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// Enabled reports whether this device should have fuel decoded at all.
func (f FuelSensor) Enabled() bool {
	return f.IOID != 0 && f.Scale > 0
}

// FuelFrom extracts a fuel percentage from a decoded IO map, reporting whether
// the configured element was present. A device whose sensor is disabled, or
// whose element is absent from this particular record, yields no reading rather
// than a zero — "the tank is empty" and "this frame carried no fuel element"
// must not look the same downstream.
func (f FuelSensor) FuelFrom(ios map[uint16]int64) (float64, bool) {
	if !f.Enabled() {
		return 0, false
	}
	raw, ok := ios[f.IOID]
	if !ok {
		return 0, false
	}
	return f.Percent(raw), true
}

// FuelSensor resolves the device's configured mapping, falling back to the
// default for a row written before migration 0047 (or by a test fixture).
func (d *Device) FuelSensor() FuelSensor {
	if d == nil {
		return DefaultFuelSensor()
	}
	s := FuelSensor{IOID: d.FuelIOID, Scale: d.FuelScale, Offset: d.FuelOffset}
	// A zero scale is not a valid configuration — the column is NOT NULL with a
	// positive CHECK — so it means the struct was built without the columns.
	// Treating it as "disabled" would silently stop fuel on any code path that
	// forgot to select them, which is the failure this file exists to end.
	if s.Scale == 0 {
		return DefaultFuelSensor()
	}
	return s
}
