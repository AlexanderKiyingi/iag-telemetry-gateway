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

	srv := &hqGateway{store: store, hub: hub, sem: make(chan struct{}, maxTCPConns)}
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
}

type hqGateway struct {
	store hqStore
	hub   *iot.Hub
	wg    sync.WaitGroup
	sem   chan struct{}
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
			logger.Info("sinotrack device connected")
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
		if !msg.ValidFix {
			if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
				logger.Warn("mark device seen failed", "err", err)
			}
			opCancel()
			logger.Debug("skip invalid fix", "type", msg.Type)
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
		if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
			logger.Warn("mark device seen failed", "err", err)
		}
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
