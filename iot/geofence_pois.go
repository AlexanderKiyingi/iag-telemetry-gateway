package iot

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// GeofenceRule says what a crossing MEANS for this fence.
//
// Until now every fence meant the same thing — log the crossing — which is fine
// for a depot and wrong for the two cases an operator actually wants to be
// woken for: a vehicle leaving a corridor it was supposed to stay in, and a
// vehicle entering somewhere it was not supposed to go.
type GeofenceRule string

const (
	// RuleWatch logs arrivals and departures. The behaviour every fence had
	// before rules existed, and the default for anything unrecognised.
	RuleWatch GeofenceRule = "watch"
	// RuleStayInside treats LEAVING as the breach: permitted areas, corridors.
	RuleStayInside GeofenceRule = "stay_inside"
	// RuleNoEntry treats ENTERING as the breach: restricted or unsafe zones.
	RuleNoEntry GeofenceRule = "no_entry"
)

// ParseGeofenceRule is deliberately forgiving.
//
// An unreadable rule falls back to watch rather than erroring, because the
// alternative is a fence that stops being evaluated at all — and a fence
// silently not watching is the one failure mode worse than a fence watching
// with the wrong verb.
func ParseGeofenceRule(v string) GeofenceRule {
	switch GeofenceRule(strings.TrimSpace(strings.ToLower(v))) {
	case RuleStayInside:
		return RuleStayInside
	case RuleNoEntry:
		return RuleNoEntry
	default:
		return RuleWatch
	}
}

// GeofencePOI is a point-of-interest with a circular geofence (km).
type GeofencePOI struct {
	Name     string
	Lat      float64
	Lng      float64
	Type     string
	RadiusKm float64
	// Rule is what a crossing means. Zero value is the empty string, which
	// AppliesTo and Breaches both read as RuleWatch.
	Rule GeofenceRule
	// VehicleIDs scopes the fence. EMPTY MEANS EVERY VEHICLE — not "no
	// vehicles" — because that is what every existing fence means and because
	// the opposite default would silently switch off monitoring the day this
	// shipped.
	VehicleIDs []string
}

// AppliesTo reports whether this fence is evaluated for a given vehicle.
func (p GeofencePOI) AppliesTo(vehicleID string) bool {
	if len(p.VehicleIDs) == 0 {
		return true
	}
	for _, id := range p.VehicleIDs {
		if id == vehicleID {
			return true
		}
	}
	return false
}

// Breaches reports whether a crossing in this direction is worth an event.
//
// Every crossing still updates the stored inside/outside state; this only
// decides whether anyone is told. A "stay inside" fence that raised an event
// each time a vehicle arrived back where it belonged would bury the one event
// that mattered.
func (p GeofencePOI) Breaches(entered bool) bool {
	switch ParseGeofenceRule(string(p.Rule)) {
	case RuleStayInside:
		return !entered
	case RuleNoEntry:
		return entered
	default:
		return true
	}
}

// DefaultGeofencePOIs is the built-in fallback, used when the geofence_pois
// table is empty or has not been loaded yet.
//
// Keeping a fallback matters: geofences are evaluated on every ping, and a
// gateway that started before the table was reachable should not silently stop
// producing site arrivals. An empty table means "no override", never "no
// geofences".
var DefaultGeofencePOIs = []GeofencePOI{
	{Name: "Africa Coffee Park (ACP)", Lat: -0.880, Lng: 30.265, Type: "iag", RadiusKm: 0.6, Rule: RuleWatch},
	{Name: "Rwashamaire Estate", Lat: -0.814, Lng: 30.067, Type: "iag", RadiusKm: 0.4, Rule: RuleWatch},
	{Name: "IAG Kampala HQ", Lat: 0.327, Lng: 32.591, Type: "iag", RadiusKm: 0.3, Rule: RuleWatch},
	{Name: "Mombasa Port", Lat: -4.050, Lng: 39.667, Type: "port", RadiusKm: 1.5, Rule: RuleWatch},
	{Name: "Dar es Salaam Port", Lat: -6.792, Lng: 39.208, Type: "port", RadiusKm: 1.5, Rule: RuleWatch},
	{Name: "Malaba Border (URA)", Lat: 0.637, Lng: 34.265, Type: "border", RadiusKm: 0.5, Rule: RuleWatch},
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
	// Assignments come back with the fence in one query rather than one per
	// fence: this runs on a timer in every gateway process, and a fan-out that
	// grows with the number of sites is the shape the transition read already
	// had to be rescued from.
	rows, err := s.op().Query(ctx, `
		SELECT p.name, p.lat, p.lng, COALESCE(p.type,'site'), p.radius_km,
		       COALESCE(p.rule,'watch'),
		       COALESCE(array_agg(v.vehicle_id::text) FILTER (WHERE v.vehicle_id IS NOT NULL), '{}')
		  FROM geofence_pois p
		  LEFT JOIN geofence_vehicles v ON v.poi_name = p.name
		 WHERE p.is_active
		 GROUP BY p.name, p.lat, p.lng, p.type, p.radius_km, p.rule
		 ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GeofencePOI
	for rows.Next() {
		var p GeofencePOI
		var rule string
		if err := rows.Scan(&p.Name, &p.Lat, &p.Lng, &p.Type, &p.RadiusKm, &rule, &p.VehicleIDs); err != nil {
			return nil, err
		}
		p.Rule = ParseGeofenceRule(rule)
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
		SELECT p.name, p.lat, p.lng, COALESCE(p.type,'site'), p.radius_km,
		       COALESCE(p.rule,'watch'), p.is_active, p.created_at, p.updated_at,
		       COALESCE(array_agg(v.vehicle_id::text) FILTER (WHERE v.vehicle_id IS NOT NULL), '{}')
		  FROM geofence_pois p
		  LEFT JOIN geofence_vehicles v ON v.poi_name = p.name
		 GROUP BY p.name, p.lat, p.lng, p.type, p.radius_km, p.rule, p.is_active, p.created_at, p.updated_at
		 ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GeofencePOIRecord{}
	for rows.Next() {
		var p GeofencePOIRecord
		var rule string
		if err := rows.Scan(&p.Name, &p.Lat, &p.Lng, &p.Type, &p.RadiusKm,
			&rule, &p.IsActive, &p.CreatedAt, &p.UpdatedAt, &p.VehicleIDs); err != nil {
			return nil, err
		}
		p.Rule = ParseGeofenceRule(rule)
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
		INSERT INTO geofence_pois (name, lat, lng, type, radius_km, is_active, rule)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4,''),'site'), $5, $6, $7)
		ON CONFLICT (name) DO UPDATE SET
			lat        = EXCLUDED.lat,
			lng        = EXCLUDED.lng,
			type       = EXCLUDED.type,
			radius_km  = EXCLUDED.radius_km,
			is_active  = EXCLUDED.is_active,
			rule       = EXCLUDED.rule,
			updated_at = NOW()`,
		p.Name, p.Lat, p.Lng, p.Type, p.RadiusKm, p.IsActive, string(ParseGeofenceRule(string(p.Rule))))
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

// SetGeofenceVehicles replaces a fence's assignments.
//
// Replace rather than merge: the caller sends the set it wants, and a
// diff-based API would leave a removed vehicle assigned whenever a request was
// lost. Deleting then inserting in one transaction means a reader never sees a
// fence briefly scoped to nothing — which, since empty means EVERY vehicle,
// would be a moment where the fence silently applied to the whole fleet.
//
// An empty list clears the scope, and that is meaningful: it returns the fence
// to fleet-wide.
func (s *Store) SetGeofenceVehicles(ctx context.Context, poiName string, vehicleIDs []string) error {
	tx, err := s.op().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM geofence_vehicles WHERE poi_name = $1`, poiName); err != nil {
		return err
	}
	for _, id := range vehicleIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		// Cast explicitly: vehicles.id is uuid since migration 0043, and
		// binding a Go string without the cast pins the parameter to text —
		// which Postgres refuses to compare against a uuid column. That exact
		// mistake took out the device write path once already.
		if _, err := tx.Exec(ctx,
			`INSERT INTO geofence_vehicles (poi_name, vehicle_id) VALUES ($1, $2::uuid)
			 ON CONFLICT DO NOTHING`, poiName, strings.TrimSpace(id)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RenameGeofencePOI moves a fence to a new name, carrying its assignments.
//
// An UPDATE rather than the create-plus-delete the handler used to do, because
// geofence_vehicles references the name ON UPDATE CASCADE: recreating the row
// under a new name would drop every vehicle assigned to it, silently returning
// the fence to fleet-wide.
//
// vehicle_geofence_state is deliberately NOT carried. It has no foreign key and
// is keyed by name, so the old rows are left inert and the renamed fence starts
// its enter/exit state fresh — which is the intended behaviour, since inheriting
// another fence's history would fire arrivals for crossings that never happened.
func (s *Store) RenameGeofencePOI(ctx context.Context, from, to string) (bool, error) {
	tag, err := s.op().Exec(ctx,
		`UPDATE geofence_pois SET name = $2, updated_at = NOW() WHERE name = $1`, from, to)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
