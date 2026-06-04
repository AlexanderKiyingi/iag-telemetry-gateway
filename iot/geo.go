package iot

import "math"

// HaversineKm returns great-circle distance in kilometres between two WGS84 points.
func HaversineKm(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusKm = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusKm * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// InsideGeofence reports whether (lat,lng) is within radiusKm of (centerLat, centerLng).
func InsideGeofence(lat, lng, centerLat, centerLng, radiusKm float64) bool {
	if radiusKm <= 0 {
		return false
	}
	return HaversineKm(lat, lng, centerLat, centerLng) <= radiusKm
}
