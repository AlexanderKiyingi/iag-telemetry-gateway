package iot

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// GeofencePOI is a point-of-interest with a circular geofence (km).
type GeofencePOI struct {
	Name     string
	Lat      float64
	Lng      float64
	Type     string
	RadiusKm float64
}

// DefaultGeofencePOIs is the built-in fallback, used when the geofence_pois
// table is empty or has not been loaded yet.
//
// Keeping a fallback matters: geofences are evaluated on every ping, and a
// gateway that started before the table was reachable should not silently stop
// producing site arrivals. An empty table means "no override", never "no
// geofences".
var DefaultGeofencePOIs = []GeofencePOI{
	{"Africa Coffee Park (ACP)", -0.880, 30.265, "iag", 0.6},
	{"Rwashamaire Estate", -0.814, 30.067, "iag", 0.4},
	{"IAG Kampala HQ", 0.327, 32.591, "iag", 0.3},
	{"Mombasa Port", -4.050, 39.667, "port", 1.5},
	{"Dar es Salaam Port", -6.792, 39.208, "port", 1.5},
	{"Malaba Border (URA)", 0.637, 34.265, "border", 0.5},
}

// activePOIs holds the loaded set. An atomic pointer rather than a mutex
// because this is read once per ping on every connection goroutine and written
// only by the periodic refresh — readers must never block behind a reload.
var activePOIs atomic.Pointer[[]GeofencePOI]

// ActiveGeofencePOIs returns the POIs currently in force.
func ActiveGeofencePOIs() []GeofencePOI {
	if v := activePOIs.Load(); v != nil && len(*v) > 0 {
		return *v
	}
	return DefaultGeofencePOIs
}

// SetGeofencePOIs replaces the active set. Passing an empty slice reverts to
// the built-in defaults rather than disabling geofencing, so a truncated table
// cannot silently switch site tracking off.
func SetGeofencePOIs(pois []GeofencePOI) {
	cp := append([]GeofencePOI(nil), pois...)
	activePOIs.Store(&cp)
}

// LoadGeofencePOIs reads the active POIs from the database.
func (s *Store) LoadGeofencePOIs(ctx context.Context) ([]GeofencePOI, error) {
	rows, err := s.op().Query(ctx, `
		SELECT name, lat, lng, COALESCE(type,'site'), radius_km
		  FROM geofence_pois
		 WHERE is_active
		 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GeofencePOI
	for rows.Next() {
		var p GeofencePOI
		if err := rows.Scan(&p.Name, &p.Lat, &p.Lng, &p.Type, &p.RadiusKm); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GeofenceRefreshInterval is how often StartGeofenceRefresh reloads. Geofences
// are edited by hand and rarely, so this only needs to be short enough that a
// change takes effect within a shift.
const GeofenceRefreshInterval = 5 * time.Minute

// StartGeofenceRefresh keeps the active POI set current for the life of ctx.
//
// It lives here rather than in each gateway's main package on purpose: every
// ingest path evaluates the same geofences, and a binary that forgot to start
// this would silently run on the built-in defaults while its sibling used the
// configured ones. One shared implementation, started by each gateway, is the
// difference between "configurable geofences" and "geofences that depend on
// which gateway your tracker happens to speak to".
func (s *Store) StartGeofenceRefresh(ctx context.Context) {
	t := time.NewTicker(GeofenceRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := s.RefreshGeofencePOIs(refreshCtx); err != nil {
				// Keep the previous set: a transient database blip must not
				// turn geofencing off.
				slog.Warn("geofence POI refresh failed, keeping previous set", "err", err)
			}
			cancel()
		}
	}
}

// RefreshGeofencePOIs reloads the active set from the database.
//
// Errors are returned rather than swallowed, but callers should treat a failure
// as non-fatal and keep serving with the previous set: losing the database for
// a minute is not a reason to stop recording site arrivals.
func (s *Store) RefreshGeofencePOIs(ctx context.Context) error {
	pois, err := s.LoadGeofencePOIs(ctx)
	if err != nil {
		return err
	}
	SetGeofencePOIs(pois)
	return nil
}
