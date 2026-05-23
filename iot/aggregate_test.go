package iot

import (
	"math"
	"testing"
	"time"
)

func ptrF(f float64) *float64 { return &f }

func TestAggregateEmpty(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	res := AggregateDay("V01", day, nil, 0)
	got := res.Summary
	if got.VehicleID != "V01" || got.PingCount != 0 || got.DistanceKm != 0 {
		t.Errorf("unexpected zero-value: %+v", got)
	}
	if got.FirstPing != nil || got.LastPing != nil {
		t.Errorf("expected nil first/last on empty input")
	}
	if len(res.FuelEvents) != 0 {
		t.Errorf("empty input should yield no fuel events, got %d", len(res.FuelEvents))
	}
}

// ─────────────────────── Fuel-detection tests ───────────────────────

func ptrB(b bool) *bool { return &b }

func TestRefuelDetected(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	pings := []Ping{
		{TS: day.Add(8 * time.Hour), Lat: 0, Lng: 0, SpeedKmh: ptrF(0), FuelLevel: ptrF(20), Ignition: ptrB(false)},
		{TS: day.Add(8*time.Hour + time.Minute), Lat: 0, Lng: 0, SpeedKmh: ptrF(0), FuelLevel: ptrF(85), Ignition: ptrB(false)},
	}
	res := AggregateDay("V01", day, pings, 200)
	if len(res.FuelEvents) != 1 {
		t.Fatalf("expected 1 fuel event, got %d", len(res.FuelEvents))
	}
	ev := res.FuelEvents[0]
	if ev.Kind != "refuel" {
		t.Errorf("Kind = %q, want refuel", ev.Kind)
	}
	if ev.DeltaPct != 65 {
		t.Errorf("DeltaPct = %v, want 65", ev.DeltaPct)
	}
	if ev.DeltaLitres == nil || *ev.DeltaLitres != 130 {
		t.Errorf("DeltaLitres = %v, want 130 (200L × 65%%)", ev.DeltaLitres)
	}
	if ev.Confidence != "high" {
		t.Errorf("Confidence = %q, want high (parked, ignition off)", ev.Confidence)
	}
	if res.Summary.FuelUsedLitres != nil && *res.Summary.FuelUsedLitres > 1 {
		t.Errorf("refuel should not count as consumption, got fuelUsed=%v", *res.Summary.FuelUsedLitres)
	}
}

func TestRefuelWhileMovingIsLowConfidence(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	pings := []Ping{
		{TS: day, Lat: 0, Lng: 0, SpeedKmh: ptrF(60), FuelLevel: ptrF(30), Ignition: ptrB(true)},
		{TS: day.Add(time.Minute), Lat: 0, Lng: 0, SpeedKmh: ptrF(60), FuelLevel: ptrF(80), Ignition: ptrB(true)},
	}
	res := AggregateDay("V01", day, pings, 200)
	if len(res.FuelEvents) != 1 {
		t.Fatalf("expected 1 event, got %d", len(res.FuelEvents))
	}
	if res.FuelEvents[0].Confidence != "low" {
		t.Errorf("refuel-while-moving should be low confidence, got %q", res.FuelEvents[0].Confidence)
	}
}

func TestSuspiciousDropDetected(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	// Vehicle parked, ignition off, fuel falls 15% in one minute → theft pattern.
	pings := []Ping{
		{TS: day.Add(2 * time.Hour), Lat: 0, Lng: 0, SpeedKmh: ptrF(0), FuelLevel: ptrF(70), Ignition: ptrB(false)},
		{TS: day.Add(2*time.Hour + time.Minute), Lat: 0, Lng: 0, SpeedKmh: ptrF(0), FuelLevel: ptrF(55), Ignition: ptrB(false)},
	}
	res := AggregateDay("V01", day, pings, 200)
	if len(res.FuelEvents) != 1 {
		t.Fatalf("expected 1 fuel event, got %d", len(res.FuelEvents))
	}
	ev := res.FuelEvents[0]
	if ev.Kind != "drop" {
		t.Errorf("Kind = %q, want drop", ev.Kind)
	}
	if ev.Confidence != "high" {
		t.Errorf("Confidence = %q, want high (theft pattern)", ev.Confidence)
	}
	if ev.DeltaLitres == nil || *ev.DeltaLitres != -30 {
		t.Errorf("DeltaLitres = %v, want -30", ev.DeltaLitres)
	}
}

func TestNormalConsumptionIsNotAnEvent(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	// Smooth 1%/minute decline while driving — not flagged, but should
	// accumulate into fuel_used_litres.
	pings := []Ping{
		{TS: day, Lat: 0, Lng: 0, SpeedKmh: ptrF(60), FuelLevel: ptrF(80), Ignition: ptrB(true)},
		{TS: day.Add(time.Minute), Lat: 0, Lng: 0, SpeedKmh: ptrF(60), FuelLevel: ptrF(79), Ignition: ptrB(true)},
		{TS: day.Add(2 * time.Minute), Lat: 0, Lng: 0, SpeedKmh: ptrF(60), FuelLevel: ptrF(78), Ignition: ptrB(true)},
	}
	res := AggregateDay("V01", day, pings, 200)
	if len(res.FuelEvents) != 0 {
		t.Errorf("normal consumption should not flag events, got %+v", res.FuelEvents)
	}
	if res.Summary.FuelUsedLitres == nil {
		t.Fatal("FuelUsedLitres should be set")
	}
	// 2% over the day, 200L tank → 4L used
	if got := *res.Summary.FuelUsedLitres; got < 3.99 || got > 4.01 {
		t.Errorf("FuelUsedLitres = %v, want ≈ 4", got)
	}
}

func TestUnknownTankCapacityNoLitres(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	pings := []Ping{
		{TS: day, Lat: 0, Lng: 0, FuelLevel: ptrF(60), SpeedKmh: ptrF(0), Ignition: ptrB(false)},
		{TS: day.Add(time.Minute), Lat: 0, Lng: 0, FuelLevel: ptrF(85), SpeedKmh: ptrF(0), Ignition: ptrB(false)},
	}
	res := AggregateDay("V01", day, pings, 0)
	if res.Summary.FuelUsedLitres != nil {
		t.Errorf("FuelUsedLitres should be nil when capacity unknown, got %v", *res.Summary.FuelUsedLitres)
	}
	if len(res.FuelEvents) != 1 {
		t.Fatalf("expected 1 event even without capacity, got %d", len(res.FuelEvents))
	}
	if res.FuelEvents[0].DeltaLitres != nil {
		t.Errorf("DeltaLitres should be nil when capacity unknown, got %v", *res.FuelEvents[0].DeltaLitres)
	}
}

func TestAggregateSinglePing(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	pings := []Ping{
		{VehicleID: "V01", TS: day.Add(8 * time.Hour), Lat: 0.327, Lng: 32.591, SpeedKmh: ptrF(40)},
	}
	got := AggregateDay("V01", day, pings, 0).Summary
	if got.PingCount != 1 {
		t.Errorf("PingCount = %d, want 1", got.PingCount)
	}
	if got.DistanceKm != 0 {
		t.Errorf("single ping → 0 km, got %v", got.DistanceKm)
	}
	if got.MaxSpeedKmh == nil || *got.MaxSpeedKmh != 40 {
		t.Errorf("MaxSpeed = %v, want 40", got.MaxSpeedKmh)
	}
}

// One degree of latitude ≈ 111.195 km. Verifying haversine + distance accumulator.
func TestAggregateDistance(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	pings := []Ping{
		{VehicleID: "V01", TS: day, Lat: 0.0, Lng: 0.0, SpeedKmh: ptrF(60)},
		{VehicleID: "V01", TS: day.Add(time.Minute), Lat: 1.0, Lng: 0.0, SpeedKmh: ptrF(60)},
		{VehicleID: "V01", TS: day.Add(2 * time.Minute), Lat: 2.0, Lng: 0.0, SpeedKmh: ptrF(60)},
	}
	got := AggregateDay("V01", day, pings, 0).Summary
	if got.PingCount != 3 {
		t.Errorf("PingCount = %d, want 3", got.PingCount)
	}
	want := 222.39 // 2 × 111.195
	if math.Abs(got.DistanceKm-want) > 1.0 {
		t.Errorf("DistanceKm = %.2f, want ≈ %.2f", got.DistanceKm, want)
	}
}

func TestAggregateMovingIdleClassification(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	// Three intervals of 1 minute each:
	//   gap 1: speeds (50, 60) → moving (1 min)
	//   gap 2: speeds (60, 0)  → average 30 → moving (1 min)
	//   gap 3: speeds (0, 0)   → idle (1 min)
	pings := []Ping{
		{TS: day, Lat: 0, Lng: 0, SpeedKmh: ptrF(50)},
		{TS: day.Add(time.Minute), Lat: 0.001, Lng: 0, SpeedKmh: ptrF(60)},
		{TS: day.Add(2 * time.Minute), Lat: 0.002, Lng: 0, SpeedKmh: ptrF(0)},
		{TS: day.Add(3 * time.Minute), Lat: 0.002, Lng: 0, SpeedKmh: ptrF(0)},
	}
	got := AggregateDay("V01", day, pings, 0).Summary
	if got.MovingMinutes != 2 {
		t.Errorf("MovingMinutes = %d, want 2", got.MovingMinutes)
	}
	if got.IdleMinutes != 1 {
		t.Errorf("IdleMinutes = %d, want 1", got.IdleMinutes)
	}
}

func TestAggregateGapCap(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	// Two pings 4 hours apart with low speed → gap-capped at 10 min idle.
	pings := []Ping{
		{TS: day, Lat: 0, Lng: 0, SpeedKmh: ptrF(0)},
		{TS: day.Add(4 * time.Hour), Lat: 0, Lng: 0, SpeedKmh: ptrF(0)},
	}
	got := AggregateDay("V01", day, pings, 0).Summary
	if got.IdleMinutes != gapCapMinutes {
		t.Errorf("IdleMinutes = %d, want %d (capped)", got.IdleMinutes, gapCapMinutes)
	}
}

func TestAggregateMaxAvgSpeed(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	pings := []Ping{
		{TS: day, Lat: 0, Lng: 0, SpeedKmh: ptrF(20)},
		{TS: day.Add(time.Minute), Lat: 0, Lng: 0, SpeedKmh: ptrF(80)},
		{TS: day.Add(2 * time.Minute), Lat: 0, Lng: 0, SpeedKmh: ptrF(50)},
	}
	got := AggregateDay("V01", day, pings, 0).Summary
	if got.MaxSpeedKmh == nil || *got.MaxSpeedKmh != 80 {
		t.Errorf("MaxSpeed = %v, want 80", got.MaxSpeedKmh)
	}
	if got.AvgSpeedKmh == nil || math.Abs(*got.AvgSpeedKmh-50) > 0.01 {
		t.Errorf("AvgSpeed = %v, want 50", got.AvgSpeedKmh)
	}
}

func TestAggregateUnsortedInput(t *testing.T) {
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	// Provide pings out of order; aggregator must sort and produce same result.
	a := []Ping{
		{TS: day.Add(2 * time.Minute), Lat: 2.0, Lng: 0},
		{TS: day, Lat: 0.0, Lng: 0},
		{TS: day.Add(time.Minute), Lat: 1.0, Lng: 0},
	}
	got := AggregateDay("V01", day, a, 0).Summary
	if got.FirstPing == nil || !got.FirstPing.Equal(day) {
		t.Errorf("FirstPing = %v, want %v", got.FirstPing, day)
	}
	if got.LastPing == nil || !got.LastPing.Equal(day.Add(2*time.Minute)) {
		t.Errorf("LastPing = %v, want %v", got.LastPing, day.Add(2*time.Minute))
	}
}
