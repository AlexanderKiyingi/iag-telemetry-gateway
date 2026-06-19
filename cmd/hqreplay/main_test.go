package main

import (
	"testing"
	"time"

	"github.com/iag/fleet-iot/iot"
)

// TestBuildV1FrameRoundTrips confirms the synthetic frames this tool emits
// decode back to the coordinates that produced them — i.e. the encoder here is
// the faithful inverse of iot.ParseHQFrame, so a green replay actually exercises
// the same numbers the gateway will store.
func TestBuildV1FrameRoundTrips(t *testing.T) {
	cases := []struct {
		name     string
		lat, lng float64
	}{
		{"kampala (N,E)", 0.347596, 32.582520},
		{"nairobi (S,E)", -1.292066, 36.821945},
		{"accra (N,W)", 5.603717, -0.186964},
	}
	ts := time.Date(2026, 6, 19, 8, 30, 15, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := buildV1Frame("9170503816", tc.lat, tc.lng, 24.0, ts)
			msg, err := iot.ParseHQFrame(frame)
			if err != nil {
				t.Fatalf("ParseHQFrame(%q): %v", frame, err)
			}
			if !msg.IsPosition || !msg.ValidFix {
				t.Fatalf("IsPosition=%v ValidFix=%v", msg.IsPosition, msg.ValidFix)
			}
			// HQ carries 4 decimal minutes ≈ 1.85e-6° resolution; allow 1e-5.
			if abs(msg.Lat-tc.lat) > 1e-5 {
				t.Errorf("Lat = %v, want ~%v", msg.Lat, tc.lat)
			}
			if abs(msg.Lng-tc.lng) > 1e-5 {
				t.Errorf("Lng = %v, want ~%v", msg.Lng, tc.lng)
			}
			if !msg.Timestamp.Equal(ts) {
				t.Errorf("Timestamp = %v, want %v", msg.Timestamp, ts)
			}
		})
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
