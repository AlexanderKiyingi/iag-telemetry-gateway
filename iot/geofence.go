package iot

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// GeofenceTransition is emitted when a vehicle enters or leaves a POI geofence.
type GeofenceTransition struct {
	POIName string
	Entered bool
	Ping    Ping
}

// ProcessGeofences compares the ping position to configured POIs and returns transitions.
func ProcessGeofences(p Ping) []GeofenceTransition {
	// A (0,0) position is the classic "no GPS fix" sentinel (open ocean off
	// West Africa, where no IAG vehicle operates); skip geofence evaluation so a
	// dropped fix cannot fabricate enter/exit transitions and spurious safety
	// events.
	if p.Lat == 0 && p.Lng == 0 {
		return nil
	}
	var out []GeofenceTransition
	for _, poi := range ActiveGeofencePOIs() {
		inside := InsideGeofence(p.Lat, p.Lng, poi.Lat, poi.Lng, poi.RadiusKm)
		out = append(out, GeofenceTransition{POIName: poi.Name, Entered: inside, Ping: p})
	}
	return out
}

// ApplyGeofenceTransitions persists state and creates safety_events on enter/exit.
//
// One read for the whole POI set, not one per POI. ProcessGeofences returns a
// transition for every active point of interest — six by default, more once the
// geofence_pois table is populated — and this used to issue a separate
// `SELECT inside FROM vehicle_geofence_state WHERE vehicle_id = $1 AND poi_name
// = $2` for each of them, on every ping. That was the single largest
// contributor to the ingest path's round-trip count, and it grew with the
// number of sites rather than staying flat.
//
// The writes stay per-transition because in the steady state there are none:
// a vehicle's inside/outside relationship to a site changes on the order of
// times per day, not times per ping, so the loop below almost always falls
// through on the `prevInside == tr.Entered` check having done no I/O at all.
func (s *Store) ApplyGeofenceTransitions(ctx context.Context, transitions []GeofenceTransition) error {
	if len(transitions) == 0 {
		return nil
	}
	vehicleID := ""
	names := make([]string, 0, len(transitions))
	for _, tr := range transitions {
		if tr.Ping.VehicleID == "" {
			continue
		}
		vehicleID = tr.Ping.VehicleID
		names = append(names, tr.POIName)
	}
	if vehicleID == "" {
		return nil
	}

	prior, err := s.geofenceStateFor(ctx, vehicleID, names)
	if err != nil {
		return err
	}

	for _, tr := range transitions {
		if tr.Ping.VehicleID == "" {
			continue
		}
		prevInside, known := prior[tr.POIName]
		if known && prevInside == tr.Entered {
			continue
		}
		if err := s.upsertGeofenceState(ctx, tr.Ping.VehicleID, tr.POIName, tr.Entered, tr.Ping.TS); err != nil {
			return err
		}
		if !known {
			// First observation — record state only, no alert.
			continue
		}
		if err := s.insertGeofenceSafetyEvent(ctx, tr); err != nil {
			return err
		}
	}
	return nil
}

// geofenceStateFor reads this vehicle's inside/outside flag for every named POI
// in one query. A POI absent from the map has never been observed for this
// vehicle, which is what distinguishes "first sighting, record it silently"
// from "a real crossing, raise a safety event".
func (s *Store) geofenceStateFor(ctx context.Context, vehicleID string, poiNames []string) (map[string]bool, error) {
	out := make(map[string]bool, len(poiNames))
	if len(poiNames) == 0 {
		return out, nil
	}
	const q = `
        SELECT poi_name, inside
          FROM vehicle_geofence_state
         WHERE vehicle_id = $1 AND poi_name = ANY($2)`
	rows, err := s.op().Query(ctx, q, vehicleID, poiNames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var inside bool
		if err := rows.Scan(&name, &inside); err != nil {
			return nil, err
		}
		out[name] = inside
	}
	return out, rows.Err()
}

func (s *Store) upsertGeofenceState(ctx context.Context, vehicleID, poiName string, inside bool, ts time.Time) error {
	const q = `
        INSERT INTO vehicle_geofence_state (vehicle_id, poi_name, inside, updated_at)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (vehicle_id, poi_name) DO UPDATE SET
            inside = EXCLUDED.inside,
            updated_at = EXCLUDED.updated_at`
	_, err := s.op().Exec(ctx, q, vehicleID, poiName, inside, ts)
	return err
}

func (s *Store) insertGeofenceSafetyEvent(ctx context.Context, tr GeofenceTransition) error {
	action := "exited"
	eventType := "Driver behaviour"
	severity := "warn"
	if tr.Entered {
		action = "entered"
		eventType = "Near-miss"
		severity = "info"
	}
	id := fmt.Sprintf("SAF-GEO-%s-%s-%d", tr.Ping.VehicleID, geofencePOIKey(tr.POIName), tr.Ping.TS.Unix())
	desc := fmt.Sprintf("Vehicle %s geofence %s %s", tr.Ping.VehicleID, tr.POIName, action)
	loc := fmt.Sprintf("%s · %.4f, %.4f", tr.POIName, tr.Ping.Lat, tr.Ping.Lng)
	const q = `
        INSERT INTO safety_events (
            id, vehicle_id, date, type, severity, status, location, description, reported_by, status_history
        ) VALUES ($1, $2, $3, $4, $5, 'open', $6, $7, 'Geofence', '[]'::jsonb)
        ON CONFLICT (id) DO NOTHING`
	_, err := s.op().Exec(ctx, q, id, tr.Ping.VehicleID, tr.Ping.TS, eventType, severity, loc, desc)
	return err
}

func geofencePOIKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" {
		return "poi"
	}
	return s
}

