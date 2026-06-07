package iot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// GeofenceTransition is emitted when a vehicle enters or leaves a POI geofence.
type GeofenceTransition struct {
	POIName string
	Entered bool
	Ping    Ping
}

// ProcessGeofences compares the ping position to configured POIs and returns transitions.
func ProcessGeofences(p Ping) []GeofenceTransition {
	var out []GeofenceTransition
	for _, poi := range GeofencePOIs {
		inside := InsideGeofence(p.Lat, p.Lng, poi.Lat, poi.Lng, poi.RadiusKm)
		out = append(out, GeofenceTransition{POIName: poi.Name, Entered: inside, Ping: p})
	}
	return out
}

// ApplyGeofenceTransitions persists state and creates safety_events on enter/exit.
func (s *Store) ApplyGeofenceTransitions(ctx context.Context, transitions []GeofenceTransition) error {
	for _, tr := range transitions {
		if tr.Ping.VehicleID == "" {
			continue
		}
		prevInside, known, err := s.geofenceWasInside(ctx, tr.Ping.VehicleID, tr.POIName)
		if err != nil {
			return err
		}
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

func (s *Store) geofenceWasInside(ctx context.Context, vehicleID, poiName string) (inside bool, known bool, err error) {
	const q = `SELECT inside FROM vehicle_geofence_state WHERE vehicle_id = $1 AND poi_name = $2`
	err = s.op().QueryRow(ctx, q, vehicleID, poiName).Scan(&inside)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	return inside, true, nil
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

