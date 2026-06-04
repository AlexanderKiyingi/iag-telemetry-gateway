package iot

// GeofencePOI is a point-of-interest with a circular geofence (km).
type GeofencePOI struct {
	Name      string
	Lat       float64
	Lng       float64
	Type      string
	RadiusKm  float64
}

// GeofencePOIs mirrors fleet reference POIs that define geofenceKm > 0.
var GeofencePOIs = []GeofencePOI{
	{"Africa Coffee Park (ACP)", -0.880, 30.265, "iag", 0.6},
	{"Rwashamaire Estate", -0.814, 30.067, "iag", 0.4},
	{"IAG Kampala HQ", 0.327, 32.591, "iag", 0.3},
	{"Mombasa Port", -4.050, 39.667, "port", 1.5},
	{"Dar es Salaam Port", -6.792, 39.208, "port", 1.5},
	{"Malaba Border (URA)", 0.637, 34.265, "border", 0.5},
}
