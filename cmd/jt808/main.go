// Command jt808 listens for JT/T 808 TCP connections (default :5015) and folds
// position reports into the shared telemetry pipeline.
//
// Some SeeWorld GPS models (P5, W20L) speak JT808 rather than GT06. The terminal
// phone number in the JT808 header is matched against iot_devices.serial.
//
//	DATABASE_URL=postgres://... JT808_ADDR=:5015 go run ./cmd/jt808
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iag/fleet-iot/iot"
	"github.com/iag/fleet-iot/pg"
)

func main() {
	configureLogger()
	addr := os.Getenv("JT808_ADDR")
	if addr == "" {
		addr = ":5015"
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
		slog.Info("jt808 gateway: split DB (registry + telemetry)")
	}
	hub := iot.NewHubFromEnv()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}
	slog.Info("jt808 TCP gateway listening", "addr", addr, "protocol", "JT808")

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
	commandsEnabled := strings.EqualFold(os.Getenv("FLEET_DEVICE_COMMANDS_ENABLED"), "true")
	if commandsEnabled {
		slog.Info("device commands enabled", "modelsWithVerifiedEncoder", iot.SupportedCommandModels())
	}
	srv := &jt808Gateway{
		store: store,
		pipeline: &iot.Pipeline{
			Store: store, Hub: hub, Overspeed: overspeed,
			Commands: store, Interlock: iot.DefaultInterlockConfig(),
			CommandsEnabled: commandsEnabled,
		},
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

const maxTCPConns = 2048

type jt808Store interface {
	FindBySerial(ctx context.Context, serial string) (*iot.Device, error)
	MarkSeen(ctx context.Context, deviceID int64, ip string) error
	InsertPings(ctx context.Context, pings []iot.Ping) (int, error)
	ApplyVehicleHotState(ctx context.Context, p iot.Ping) (iot.StatusSyncResult, error)
	ApplyGeofenceTransitions(ctx context.Context, transitions []iot.GeofenceTransition) error
	SetDeviceProtocol(ctx context.Context, deviceID int64, protocol string) error
	ApplyOverspeed(ctx context.Context, p iot.Ping, cfg iot.OverspeedConfig) error
	NextPendingCommand(ctx context.Context, deviceID int64) (*iot.DeviceCommand, error)
	VehicleSnapshotFor(ctx context.Context, vehicleID string) (iot.VehicleSnapshot, error)
	MarkCommandSent(ctx context.Context, id int64, payload string, snap iot.VehicleSnapshot) error
	MarkCommandRefused(ctx context.Context, id int64, reason string, snap iot.VehicleSnapshot) error
}

type jt808Gateway struct {
	store    jt808Store
	pipeline *iot.Pipeline
	wg       sync.WaitGroup
	sem      chan struct{}
	serial   atomic.Uint32
}

func (g *jt808Gateway) nextSerial() uint16 {
	return uint16(g.serial.Add(1))
}

func (g *jt808Gateway) serve(l net.Listener) {
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
			slog.Warn("jt808 gateway at capacity; rejecting connection",
				"remote", conn.RemoteAddr().String(), "max", maxTCPConns)
			_ = conn.Close()
		}
	}
}

func (g *jt808Gateway) handle(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	defer conn.Close()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("jt808 connection panic recovered", "remote", remote, "panic", rec)
		}
	}()

	var (
		device  *iot.Device
		boundID string
		logger  = slog.With("remote", remote)
	)

	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		frame, err := iot.ReadJT808Frame(r)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrDeadlineExceeded) {
				logger.Debug("jt808 connection closed", "err", err)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

		msg, err := iot.ParseJT808Frame(frame)
		if err != nil {
			logger.Debug("drop malformed JT808 frame", "err", err)
			continue
		}
		if msg.Phone == "" {
			if msg.NeedsReply() {
				_ = iot.WriteJT808(conn, iot.BuildJT808Reply(msg, g.nextSerial()))
			}
			continue
		}

		if device == nil {
			hsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			device, err = findJT808Device(hsCtx, g.store, msg.Phone)
			cancel()
			if err != nil {
				logger.Info("unknown or inactive jt808 device, closing", "phone", msg.Phone, "err", err)
				if msg.NeedsReply() {
					_ = iot.WriteJT808(conn, iot.BuildJT808Reply(msg, g.nextSerial()))
				}
				return
			}
			boundID = device.Serial
			logger = logger.With("deviceId", boundID, "vehicleId", device.VehicleID)
			logger.Info("jt808 device connected", "model", device.Model, "framePhone", msg.Phone)
			psCtx, psCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := g.store.SetDeviceProtocol(psCtx, device.ID, iot.ProtocolJT808); err != nil {
				logger.Warn("record device protocol failed", "err", err)
			}
			psCancel()
			if device.VehicleID == "" {
				logger.Warn("device has no vehicle binding; positions will be dropped until it is bound to a vehicle")
			}
		} else if !jt808SerialMatch(boundID, msg.Phone) {
			logger.Warn("frame phone does not match bound device, dropping", "framePhone", msg.Phone)
			if msg.NeedsReply() {
				_ = iot.WriteJT808(conn, iot.BuildJT808Reply(msg, g.nextSerial()))
			}
			continue
		}

		if msg.NeedsReply() {
			if err := iot.WriteJT808(conn, iot.BuildJT808Reply(msg, g.nextSerial())); err != nil {
				logger.Warn("jt808 reply write failed", "err", err)
				return
			}
		}

		opCtx, opCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if !msg.UsablePosition() {
			if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
				logger.Warn("mark device seen failed", "err", err)
			}
			opCancel()
			continue
		}
		if device.VehicleID == "" {
			if err := g.store.MarkSeen(opCtx, device.ID, ipOnly(remote)); err != nil {
				logger.Warn("mark device seen failed", "err", err)
			}
			opCancel()
			continue
		}

		ping := iot.JT808MessageToPing(msg, device)
		if _, err := g.pipeline.Ingest(opCtx, device, []iot.Ping{ping}, ipOnly(remote), logger); err != nil {
			opCancel()
			logger.Error("insert ping failed", "err", err)
			return
		}
		g.pipeline.DeliverPendingCommand(opCtx, conn, device, logger)
		opCancel()
		logger.Info("ping persisted", "msgId", msg.MsgID, "lat", ping.Lat, "lng", ping.Lng)
	}
}

func ipOnly(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

func findJT808Device(ctx context.Context, store jt808Store, phone string) (*iot.Device, error) {
	var last error
	for _, cand := range iot.JT808SerialCandidates(phone) {
		d, err := store.FindBySerial(ctx, cand)
		if err == nil {
			return d, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no serial candidates")
	}
	return nil, last
}

func jt808SerialMatch(bound, phone string) bool {
	for _, cand := range iot.JT808SerialCandidates(phone) {
		if cand == bound {
			return true
		}
	}
	for _, cand := range iot.JT808SerialCandidates(bound) {
		if cand == phone {
			return true
		}
	}
	return bound == phone
}
