// Command sinotrack listens for SinoTrack / HQ-protocol TCP connections
// (default :5013) and folds their position reports into the same telemetry
// pipeline as the Teltonika gateway — telemetry_timeseries, vehicle hot-state,
// geofence transitions, and the live SSE hub.
//
// SinoTrack trackers (ST-901/906/915 and HQ-protocol clones) cannot speak
// Teltonika Codec 8, so they need their own listener. The device id embedded in
// each frame is matched against iot_devices.serial, exactly as the Teltonika
// gateway matches IMEI — so a SinoTrack unit is provisioned identically: create
// an iot_devices row whose serial equals the id the tracker is programmed to
// send, bound to a vehicle.
//
//	DATABASE_URL=postgres://... SINOTRACK_ADDR=:5013 go run ./cmd/sinotrack
package main

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iag/fleet-iot/iot"
	"github.com/iag/fleet-iot/pg"
)

func main() {
	configureLogger()
	addr := os.Getenv("SINOTRACK_ADDR")
	if addr == "" {
		addr = ":5013"
	}
	connectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	registryPool, telemetryPool, err := pg.ConnectSplit(connectCtx)
	cancel()
	if err != nil {
		slog.Error("connect Postgres", "err", err)
		os.Exit(1)
	}
	defer registryPool.Close()
	if telemetryPool != registryPool {
		defer telemetryPool.Close()
	}

	store := iot.NewSplitStore(registryPool, telemetryPool)
	if os.Getenv("REGISTRY_DATABASE_URL") != "" {
		slog.Info("sinotrack gateway: split DB (registry + telemetry)")
	}
	hub := iot.NewHubFromEnv()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}
	slog.Info("sinotrack TCP gateway listening", "addr", addr, "protocol", "HQ")

	// Load geofences from the database, then keep them fresh. A failure here is
	// deliberately non-fatal: the built-in defaults still apply, and refusing to
	// start a telemetry gateway because a POI table was unreachable would trade
	// a stale geofence for total blindness.
	geoCtx, geoCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := store.RefreshGeofencePOIs(geoCtx); err != nil {
		slog.Warn("geofence POIs: using built-in defaults", "err", err)
	} else {
		slog.Info("geofence POIs loaded", "count", len(iot.ActiveGeofencePOIs()))
	}
	geoCancel()
	go store.StartGeofenceRefresh(context.Background())

	overspeed := iot.OverspeedConfigFromEnv()
	if overspeed.Enabled() {
		slog.Info("overspeed monitoring on",
			"defaultLimitKmh", overspeed.DefaultLimitKmh, "sustain", overspeed.MinBreachDuration)
	}
	// Remote immobilisation is opt-in and stays unavailable until a verified
	// per-model encoder is registered, so enabling the flag alone cannot send
	// anything to a device.
	commandsEnabled := strings.EqualFold(os.Getenv("FLEET_DEVICE_COMMANDS_ENABLED"), "true")
	if commandsEnabled {
		slog.Info("device commands enabled", "modelsWithVerifiedEncoder", iot.SupportedCommandModels())
	}
	srv := &hqGateway{
		store: store, hub: hub, overspeed: overspeed,
		interlock: iot.DefaultInterlockConfig(), commandsEnabled: commandsEnabled,
		sem: make(chan struct{}, maxTCPConns),
	}
	go srv.serve(listener)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	_ = listener.Close()
	done := make(chan struct{})
	go func() {
		srv.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

func configureLogger() {
	var h slog.Handler
	if os.Getenv("LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		h = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(h))
}

// maxTCPConns caps concurrently-handled device connections so an attacker
// opening many sockets cannot exhaust goroutines/file descriptors. Connections
// beyond the cap are rejected immediately rather than queued.
const maxTCPConns = 2048

// hqStore is the subset of *iot.Store the connection loop uses. Narrowing it to
// an interface lets the loop be tested with a fake store (no Postgres). The
// concrete *iot.Store satisfies it.
type hqStore interface {
	FindBySerial(ctx context.Context, serial string) (*iot.Device, error)
	MarkSeen(ctx context.Context, deviceID int64, ip string) error
	InsertPings(ctx context.Context, pings []iot.Ping) (int, error)
	ApplyVehicleHotState(ctx context.Context, p iot.Ping) (iot.StatusSyncResult, error)
	ApplyGeofenceTransitions(ctx context.Context, transitions []iot.GeofenceTransition) error
	RecordStatusWord(ctx context.Context, deviceID int64, frameType, statusWord, sampleFrame string, hadFix bool) error
	SetDeviceProtocol(ctx context.Context, deviceID int64, protocol string) error
	ApplyOverspeed(ctx context.Context, p iot.Ping, cfg iot.OverspeedConfig) error
	NextPendingCommand(ctx context.Context, deviceID int64) (*iot.DeviceCommand, error)
	VehicleSnapshotFor(ctx context.Context, vehicleID string) (iot.VehicleSnapshot, error)
	MarkCommandSent(ctx context.Context, id int64, payload string, snap iot.VehicleSnapshot) error
	MarkCommandRefused(ctx context.Context, id int64, reason string, snap iot.VehicleSnapshot) error
}

// deliverPendingCommand checks for a queued command and, if the interlocks
// allow, writes it to the device's own socket.
//
// Delivery is attempted while the device is connected and talking, which is the
// only moment it can be reached — HQ devices dial out and there is no way to
// call them. Crucially the interlock runs HERE, against the vehicle state right
// now, not when the operator pressed the button: a command can sit queued while
// the truck pulls onto a highway.
//
// Every outcome is recorded, refusals included.
func (g *hqGateway) deliverPendingCommand(ctx context.Context, conn net.Conn, device *iot.Device, logger *slog.Logger) {
	if !g.commandsEnabled {
		return
	}
	cmd, err := g.store.NextPendingCommand(ctx, device.ID)
	if err != nil {
		logger.Warn("check pending command failed", "err", err)
		return
	}
	if cmd == nil {
		return
	}

	snap, err := g.store.VehicleSnapshotFor(ctx, cmd.VehicleID)
	if err != nil {
		logger.Warn("vehicle snapshot for interlock failed", "err", err)
		return // leave pending; refusing on a database blip would be wrong
	}

	verdict := iot.EvaluateInterlock(cmd.Kind, time.Since(cmd.RequestedAt), snap, g.interlock)
	if !verdict.Allowed {
		logger.Warn("command refused by interlock",
			"commandId", cmd.ID, "kind", cmd.Kind, "reason", verdict.Reason)
		if err := g.store.MarkCommandRefused(ctx, cmd.ID, verdict.Reason, snap); err != nil {
			logger.Warn("record command refusal failed", "err", err)
		}
		return
	}

	// The model must have a verified encoder. There is no default: an
	// unrecognised model is refused rather than sent a guessed byte sequence.
	payload, err := iot.EncodeCommand(device.Model, cmd.Kind)
	if err != nil {
		logger.Warn("command refused: no verified encoder",
			"commandId", cmd.ID, "model", device.Model, "err", err)
		if err := g.store.MarkCommandRefused(ctx, cmd.ID, err.Error(), snap); err != nil {
			logger.Warn("record command refusal failed", "err", err)
		}
		return
	}

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		logger.Error("command write failed", "commandId", cmd.ID, "err", err)
		return // stays pending; the device will reconnect
	}
	_ = conn.SetWriteDeadline(time.Time{})

	logger.Info("command delivered",
		"commandId", cmd.ID, "kind", cmd.Kind, "reason", verdict.Reason)
	if err := g.store.MarkCommandSent(ctx, cmd.ID, string(payload), snap); err != nil {
		logger.Warn("record command sent failed", "err", err)
	}
}

type hqGateway struct {
	store           hqStore
	hub             *iot.Hub
	overspeed       iot.OverspeedConfig
	interlock       iot.InterlockConfig
	commandsEnabled bool
	wg              sync.WaitGroup
	sem             chan struct{}
}

func (g *hqGateway) serve(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("accept failed", "err", err)
			continue
		}
		select {
		case g.sem <- struct{}{}:
			g.wg.Add(1)
			go func() {
				defer g.wg.Done()
				defer func() { <-g.sem }()
				g.handle(conn)
			}()
		default:
			slog.Warn("sinotrack gateway at capacity; rejecting connection",
				"remote", conn.RemoteAddr().String(), "max", maxTCPConns)
			_ = conn.Close()
		}
	}
}

func (g *hqGateway) handle(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	defer conn.Close()
	// A malformed frame must not panic the whole process (an unrecovered panic
	// in any goroutine is fatal in Go). Contain it to this one connection.
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("sinotrack connection panic recovered", "remote", remote, "panic", rec)
		}
	}()

	// The HQ protocol has no handshake — the device id rides in every frame.
	// The first frame authenticates the connection; it is then pinned so a
	// single socket cannot inject positions for more than one device/vehicle.
	var (
		device  *iot.Device
		boundID string
		logger  = slog.With("remote", remote)
	)

	sc := iot.NewHQScanner(bufio.NewReader(conn))
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for sc.Scan() {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		msg, err := iot.ParseHQFrame(sc.Text())
		if err != nil {
			logger.Debug("drop malformed HQ frame", "err", err)
			continue
		}
		if msg.DeviceID == "" {
			continue
		}

		// Authenticate on the first frame; reject frames that switch device id.
		if device == nil {
			hsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			device, err = g.store.FindBySerial(hsCtx, msg.DeviceID)
			cancel()
			if err != nil {
				logger.Info("unknown or inactive sinotrack device, closing", "deviceId", msg.DeviceID, "err", err)
				return
			}
			boundID = msg.DeviceID
			logger = logger.With("deviceId", boundID, "vehicleId", device.VehicleID)
			logger.Info("sinotrack device connected", "model", device.Model)
			// Record the wire protocol so "why isn't this unit reporting" can be
			// answered from the device row rather than by reading gateway logs.
			psCtx, psCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := g.store.SetDeviceProtocol(psCtx, device.ID, iot.ProtocolHQ); err != nil {
				logger.Warn("record device protocol failed", "err", err)
			}
			psCancel()
			// The binding is fixed for the connection's life, so warn once here
			// rather than per frame.
			if device.VehicleID == "" {
				logger.Warn("device has no vehicle binding; positions will be dropped until it is bound to a vehicle")
			}
		} else if msg.DeviceID != boundID {
			logger.Warn("frame device id does not match bound device, dropping",
				"frameDeviceId", msg.DeviceID)
			continue
		}

		opCtx, opCancel := context.WithTimeout(context.Background(), 30*time.Second)

		// Capture the status/alarm word on EVERY frame — before any of the skip
		// branches below, deliberately. It only reaches telemetry_timeseries via
		// a ping's raw payload, so heartbeats, no-fix frames and 0,0 frames would
		// otherwise drop theirs; and those are precisely the frames that carry
		// power-cut and tamper alarms, which arrive without a GPS lock. Without
		// this a pilot can run for weeks and still not have the data needed to
		// derive the per-model bit layout.
		// One definition of "this frame carries a position we can actually use",
		// shared by the capture below and the skip branch further down, so the two
		// can never disagree about what counts as a fix.
		usablePosition := msg.IsPosition && msg.ValidFix && !(msg.Lat == 0 && msg.Lng == 0)

		if msg.Status != "" {
			if err := g.store.RecordStatusWord(opCtx, device.ID, msg.Type, msg.Status, msg.Raw,
				usablePosition); err != nil {
				logger.Warn("record status word failed", "err", err)
			}
		}

		// Non-position frames (heartbeat / login / command echo) keep the link
		// alive and refresh last-seen, but produce no ping.
		if !msg.IsPosition {
			if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
				logger.Warn("mark device seen failed", "err", err)
			}
			opCancel()
			continue
		}
		// A 'V' fix means no GPS lock; coordinates are stale/garbage. Skip the
		// insert (avoids polluting the track with 0,0) but stay connected.
		//
		// The validity flag alone is not enough. ParseHQFrame classifies a frame
		// as a position by its SHAPE, not its type code, so a heartbeat or
		// command echo that happens to carry a position-shaped payload is
		// treated as a fix — and HQ-protocol clones routinely emit exactly that
		// on cold start or indoors: validity 'A' with 0,0 coordinates. Inserted,
		// it drops the vehicle on Null Island in the Gulf of Guinea, corrupts the
		// track, and hands trip detection a continent-sized jump. ProcessGeofences
		// already refuses to evaluate 0,0 for the same reason.
		if !usablePosition {
			if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
				logger.Warn("mark device seen failed", "err", err)
			}
			opCancel()
			logger.Debug("skip fix without a usable position",
				"type", msg.Type, "validFix", msg.ValidFix, "lat", msg.Lat, "lng", msg.Lng)
			continue
		}

		// An unbound device has nowhere to attach telemetry —
		// telemetry_timeseries.vehicle_id is NOT NULL and the hot-state/geofence
		// steps below are vehicle-scoped. Keep the link alive (mark seen) but skip
		// the insert so we never write orphan ''-vehicle pings.
		if device.VehicleID == "" {
			if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
				logger.Warn("mark device seen failed", "err", err)
			}
			opCancel()
			logger.Debug("skip position from unbound device", "type", msg.Type)
			continue
		}

		ping := messageToPing(msg, device)
		if _, err := g.store.InsertPings(opCtx, []iot.Ping{ping}); err != nil {
			opCancel()
			logger.Error("insert ping failed", "err", err)
			return
		}
		if _, err := g.store.ApplyVehicleHotState(opCtx, ping); err != nil {
			logger.Warn("registry sync failed after sinotrack ingest", "err", err)
		}
		if err := g.store.ApplyGeofenceTransitions(opCtx, iot.ProcessGeofences(ping)); err != nil {
			logger.Warn("geofence transitions failed after sinotrack ingest", "err", err)
		}
		// Overspeed is evaluated server-side from the same fix rather than
		// trusting the tracker's own alarm, which is set per unit over SMS and
		// invisible here. No-op unless a limit is configured.
		if err := g.store.ApplyOverspeed(opCtx, ping, g.overspeed); err != nil {
			logger.Warn("overspeed evaluation failed after sinotrack ingest", "err", err)
		}
		if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
			logger.Warn("mark device seen failed", "err", err)
		}
		// The device is connected and listening right now — the only moment a
		// queued command can reach it.
		g.deliverPendingCommand(opCtx, conn, device, logger)
		opCancel()

		if g.hub != nil {
			g.hub.Publish(ping)
		}
		logger.Info("ping persisted", "type", msg.Type, "lat", ping.Lat, "lng", ping.Lng)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Debug("sinotrack connection closed", "err", err)
	}
}

func messageToPing(msg iot.HQMessage, device *iot.Device) iot.Ping {
	devID := device.ID
	heading := msg.Heading
	speed := msg.SpeedKmh
	return iot.Ping{
		VehicleID: device.VehicleID,
		DeviceID:  &devID,
		TS:        msg.Timestamp,
		Lat:       msg.Lat,
		Lng:       msg.Lng,
		Heading:   &heading,
		SpeedKmh:  &speed,
		Raw:       msg.RawJSON(),
	}
}

func ipOnly(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}
