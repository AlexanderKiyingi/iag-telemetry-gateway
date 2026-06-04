package iot

import "time"

// StaleVehicleAfter returns how long without a ping before a vehicle is marked offline.
// Override with FLEET_TELEMETRY_STALE_MINUTES (default 15).
func StaleVehicleAfter() time.Duration {
	return time.Duration(envInt("FLEET_TELEMETRY_STALE_MINUTES", 15)) * time.Minute
}

// DeriveStatusFromPing maps telemetry into vehicles.status.
// Maintenance is never overwritten by ingest — operators set that explicitly.
func DeriveStatusFromPing(currentStatus string, speedKmh *float64) string {
	if currentStatus == "maintenance" {
		return "maintenance"
	}
	speed := 0.0
	if speedKmh != nil {
		speed = *speedKmh
	}
	if speed >= movingThresholdKmh {
		return "moving"
	}
	return "idle"
}
