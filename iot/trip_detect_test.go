package iot

import (
	"testing"
	"time"
)

func TestDetectTripsFromPings(t *testing.T) {
	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	speed := func(kmh float64) *float64 { return &kmh }
	pings := []Ping{
		{VehicleID: "V1", TS: base, Lat: 0, Lng: 30, SpeedKmh: speed(0)},
		{VehicleID: "V1", TS: base.Add(2 * time.Minute), Lat: 0.01, Lng: 30.01, SpeedKmh: speed(40)},
		{VehicleID: "V1", TS: base.Add(12 * time.Minute), Lat: 0.02, Lng: 30.02, SpeedKmh: speed(50)},
		{VehicleID: "V1", TS: base.Add(22 * time.Minute), Lat: 0.03, Lng: 30.03, SpeedKmh: speed(45)},
	}
	trips := DetectTripsFromPings("V1", pings)
	if len(trips) != 1 {
		t.Fatalf("want 1 trip, got %d", len(trips))
	}
	if trips[0].DistanceKm <= 0 {
		t.Errorf("expected positive distance, got %v", trips[0].DistanceKm)
	}
}
