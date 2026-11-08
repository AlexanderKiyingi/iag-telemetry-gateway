package iot

import "math"

// LatLng is one WGS84 vertex.
type LatLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

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

// InsidePolygon reports whether (lat,lng) is inside a closed ring (ray casting).
//
// Vertices are treated as a planar ring in lat/lng degrees. That is accurate
// enough for depot / corridor fences a few kilometres across; continental
// polygons should not be drawn this way. Needs at least three vertices.
func InsidePolygon(lat, lng float64, ring []LatLng) bool {
	n := len(ring)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		yi, xi := ring[i].Lat, ring[i].Lng
		yj, xj := ring[j].Lat, ring[j].Lng
		intersect := ((yi > lat) != (yj > lat)) &&
			(lng < (xj-xi)*(lat-yi)/(yj-yi+1e-18)+xi)
		if intersect {
			inside = !inside
		}
		j = i
	}
	return inside
}

// CentroidAndRadiusKm returns the arithmetic mean of vertices and the farthest
// vertex distance from that mean. Used when persisting a polygon so lat/lng
// and radius_km stay populated for map focus and the existing CHECK.
func CentroidAndRadiusKm(ring []LatLng) (lat, lng, radiusKm float64, ok bool) {
	if len(ring) < 3 {
		return 0, 0, 0, false
	}
	var sumLat, sumLng float64
	for _, v := range ring {
		sumLat += v.Lat
		sumLng += v.Lng
	}
	lat = sumLat / float64(len(ring))
	lng = sumLng / float64(len(ring))
	for _, v := range ring {
		if d := HaversineKm(lat, lng, v.Lat, v.Lng); d > radiusKm {
			radiusKm = d
		}
	}
	if radiusKm <= 0 {
		radiusKm = 0.05 // tiny but positive — degenerate near-points
	}
	return lat, lng, radiusKm, true
}
