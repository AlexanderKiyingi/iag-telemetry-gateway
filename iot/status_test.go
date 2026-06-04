package iot

import "testing"

func TestDeriveStatusFromPing(t *testing.T) {
	speed := 12.0
	zero := 0.0
	tests := []struct {
		current string
		speed   *float64
		want    string
	}{
		{"maintenance", &speed, "maintenance"},
		{"moving", &zero, "idle"},
		{"idle", &speed, "moving"},
		{"offline", nil, "idle"},
	}
	for _, tc := range tests {
		got := DeriveStatusFromPing(tc.current, tc.speed)
		if got != tc.want {
			t.Errorf("DeriveStatusFromPing(%q, %v) = %q, want %q", tc.current, tc.speed, got, tc.want)
		}
	}
}
