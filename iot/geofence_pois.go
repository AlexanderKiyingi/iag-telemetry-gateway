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

// poisLoaded records that a read of geofence_pois has actually succeeded.
//
// It is what separates "the table says there are no fences" from "we have not
// managed to ask yet", which an empty slice alone cannot express. Before
// geofences were editable the difference did not arise: the table was seeded by
// migration and only ever changed by another migration, so empty meant
// unloaded. Now that an operator can delete one, it matters — deleting the last
// fence has to switch site tracking off, not quietly restore the six built-in
// ones and leave the map disagreeing with what is enforced.
var poisLoaded atomic.Bool

// ActiveGeofencePOIs returns the POIs currently in force.
//
// The built-in set applies only until the first successful load. After that the
// database is the authority, including when it says there is nothing.
func ActiveGeofencePOIs() []GeofencePOI {
	if v := activePOIs.Load(); v != nil && (len(*v) > 0 || poisLoaded.Load()) {
		return *v
	}
	return DefaultGeofencePOIs
}

// SetGeofencePOIs replaces the active set without claiming it came from the
// database. Used by tests and by callers holding a set from elsewhere; a load
// that reached the table should call SetGeofencePOIsLoaded instead, so that an
// empty result is honoured rather than read as "not loaded yet".
func SetGeofencePOIs(pois []GeofencePOI) {
	cp := append([]GeofencePOI(nil), pois...)
	activePOIs.Store(&cp)
}

// SetGeofencePOIsLoaded replaces the active set with one that came from a
// successful read, so an empty set means no geofences rather than no answer.
func SetGeofencePOIsLoaded(pois []GeofencePOI) {
	cp := append([]GeofencePOI(nil), pois...)
	activePOIs.Store(&cp)
	poisLoaded.Store(true)
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
	// Loaded, so an empty result is an answer: no active fences. Only a read
	// that never succeeded leaves the built-in set in force.
	SetGeofencePOIsLoaded(pois)
	return nil
}

// GeofencePOIRecord is a POI as an operator manages it, rather than as the
// evaluator consumes it: the evaluator only ever sees active fences, so
// GeofencePOI has no is_active field to speak of.
type GeofencePOIRecord struct {
	GeofencePOI
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListGeofencePOIs reads every POI, active or not.
//
// Distinct from LoadGeofencePOIs, which the evaluator uses and which filters to
// active rows. A management view that hid deactivated fences would offer no way
// to turn one back on.
func (s *Store) ListGeofencePOIs(ctx context.Context) ([]GeofencePOIRecord, error) {
	rows, err := s.op().Query(ctx, `
		SELECT name, lat, lng, COALESCE(type,'site'), radius_km, is_active, created_at, updated_at
		  FROM geofence_pois
		 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GeofencePOIRecord{}
	for rows.Next() {
		var p GeofencePOIRecord
		if err := rows.Scan(&p.Name, &p.Lat, &p.Lng, &p.Type, &p.RadiusKm,
			&p.IsActive, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertGeofencePOI creates a POI or updates the one with that name.
//
// name is the primary key, so this is an upsert rather than separate create and
// update paths — and renaming a fence is therefore a delete plus a create, which
// is correct: vehicle_geofence_state is keyed by poi_name, so a rename starts
// the enter/exit state fresh rather than inheriting another fence's history.
func (s *Store) UpsertGeofencePOI(ctx context.Context, p GeofencePOIRecord) error {
	_, err := s.op().Exec(ctx, `
		INSERT INTO geofence_pois (name, lat, lng, type, radius_km, is_active)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4,''),'site'), $5, $6)
		ON CONFLICT (name) DO UPDATE SET
			lat        = EXCLUDED.lat,
			lng        = EXCLUDED.lng,
			type       = EXCLUDED.type,
			radius_km  = EXCLUDED.radius_km,
			is_active  = EXCLUDED.is_active,
			updated_at = NOW()`,
		p.Name, p.Lat, p.Lng, p.Type, p.RadiusKm, p.IsActive)
	return err
}

// DeleteGeofencePOI removes a POI. Reports whether a row was actually removed,
// so the caller can answer 404 rather than pretending a typo succeeded.
func (s *Store) DeleteGeofencePOI(ctx context.Context, name string) (bool, error) {
	tag, err := s.op().Exec(ctx, `DELETE FROM geofence_pois WHERE name = $1`, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
