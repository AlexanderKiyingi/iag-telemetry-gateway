package iot

import "testing"

func TestInsidePolygon_square(t *testing.T) {
	// Rough Kampala block.
	ring := []LatLng{
		{Lat: 0.30, Lng: 32.55},
		{Lat: 0.30, Lng: 32.60},
		{Lat: 0.35, Lng: 32.60},
		{Lat: 0.35, Lng: 32.55},
	}
	if !InsidePolygon(0.32, 32.57, ring) {
		t.Fatal("point inside the square must be inside")
	}
	if InsidePolygon(0.40, 32.57, ring) {
		t.Fatal("point north of the square must be outside")
	}
	if InsidePolygon(0.32, 32.70, ring) {
		t.Fatal("point east of the square must be outside")
	}
}

func TestCentroidAndRadiusKm(t *testing.T) {
	ring := []LatLng{
		{Lat: 0.0, Lng: 0.0},
		{Lat: 0.0, Lng: 0.1},
		{Lat: 0.1, Lng: 0.1},
		{Lat: 0.1, Lng: 0.0},
	}
	lat, lng, r, ok := CentroidAndRadiusKm(ring)
	if !ok {
		t.Fatal("expected ok")
	}
	if lat < 0.04 || lat > 0.06 || lng < 0.04 || lng > 0.06 {
		t.Fatalf("centroid = %f,%f", lat, lng)
	}
	if r <= 0 {
		t.Fatalf("radius %f", r)
	}
}

func TestGeofencePOI_Contains_polygon(t *testing.T) {
	p := GeofencePOI{
		Shape: ShapePolygon,
		Vertices: []LatLng{
			{Lat: 0.30, Lng: 32.55},
			{Lat: 0.30, Lng: 32.60},
			{Lat: 0.35, Lng: 32.60},
			{Lat: 0.35, Lng: 32.55},
		},
		RadiusKm: 10, // ignored for polygon hit-test
	}
	if !p.Contains(0.32, 32.57) {
		t.Fatal("expected inside")
	}
	circle := GeofencePOI{Lat: 0.32, Lng: 32.57, RadiusKm: 0.5, Shape: ShapeCircle}
	if !circle.Contains(0.32, 32.57) {
		t.Fatal("circle centre must be inside")
	}
}
