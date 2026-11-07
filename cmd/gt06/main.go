// Command gt06 listens for Concox / SeeWorld GT06-protocol TCP connections
// (default :5023) and folds their position reports into the shared telemetry
// pipeline — telemetry_timeseries, vehicle hot-state, geofence transitions, and
// the live SSE hub.
//
// SeeWorld GPS trackers (and many other Chinese units) speak GT06 binary frames
// starting 0x7878 / 0x7979. They cannot use the SinoTrack HQ ASCII listener
// (:5013) or the Teltonika Codec 8 listener (:5027). Identity is the IMEI from
// the login packet, matched against iot_devices.serial.
//
//	DATABASE_URL=postgres://... GT06_ADDR=:5023 go run ./cmd/gt06
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
	addr := os.Getenv("GT06_ADDR")
	if addr == "" {
		addr = ":5023"
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
		slog.Info("gt06 gateway: split DB (registry + telemetry)")
	}
	hub := iot.NewHubFromEnv()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "err", err)
		os.Exit(1)
	}
	slog.Info("gt06 TCP gateway listening", "addr", addr, "protocol", "GT06")

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
	srv := &gt06Gateway{
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

type gt06Store interface {
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

type gt06Gateway struct {
	store    gt06Store
	pipeline *iot.Pipeline
	wg       sync.WaitGroup
	sem      chan struct{}
}

func (g *gt06Gateway) serve(l net.Listener) {
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
			slog.Warn("gt06 gateway at capacity; rejecting connection",
				"remote", conn.RemoteAddr().String(), "max", maxTCPConns)
			_ = conn.Close()
		}
	}
}

func (g *gt06Gateway) handle(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	defer conn.Close()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("gt06 connection panic recovered", "remote", remote, "panic", rec)
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
		frame, err := iot.ReadGT06Frame(r)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrDeadlineExceeded) {
				logger.Debug("gt06 connection closed", "err", err)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

		model := ""
		if device != nil {
			model = device.Model
		}
		msg, err := iot.ParseGT06Frame(frame, model)
		if err != nil {
			logger.Debug("drop malformed GT06 frame", "err", err)
			continue
		}

		// Login carries the IMEI; later frames do not. Authenticate on login.
		// Heartbeat/alarm before login still need an ACK or many firmwares
		// treat the session as dead and GPRS-reconnect in a loop.
		if device == nil {
			imei := msg.IMEI
			if imei == "" {
				if msg.NeedsACK() {
					if _, err := conn.Write(iot.BuildGT06ACK(msg.Protocol, msg.Serial)); err != nil {
						logger.Warn("gt06 pre-login ACK write failed", "err", err)
						return
					}
				}
				logger.Debug("gt06 frame before login, waiting", "protocol", msg.Protocol)
				continue
			}
			hsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			device, err = findGT06Device(hsCtx, g.store, imei)
			cancel()
			if err != nil {
				logger.Info("unknown or inactive gt06 device, closing", "imei", imei, "err", err)
				// Still ACK login so a mis-provisioned unit does not GPRS-storm
				// reconnect (Concox guidance). Then close.
				if msg.NeedsACK() {
					_, _ = conn.Write(iot.BuildGT06ACK(msg.Protocol, msg.Serial))
				}
				return
			}
			boundID = device.Serial
			logger = logger.With("deviceId", boundID, "vehicleId", device.VehicleID)
			logger.Info("gt06 device connected", "model", device.Model, "loginImei", imei)
			psCtx, psCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := g.store.SetDeviceProtocol(psCtx, device.ID, iot.ProtocolGT06); err != nil {
				logger.Warn("record device protocol failed", "err", err)
			}
			psCancel()
			if device.VehicleID == "" {
				logger.Warn("device has no vehicle binding; positions will be dropped until it is bound to a vehicle")
			}
		}

		if msg.NeedsACK() {
			if _, err := conn.Write(iot.BuildGT06ACK(msg.Protocol, msg.Serial)); err != nil {
				logger.Warn("gt06 ACK write failed", "err", err)
				return
			}
		}

		opCtx, opCancel := context.WithTimeout(context.Background(), 30*time.Second)

		if msg.TerminalInfo != nil {
			statusWord := strings.ToUpper(hexByte(*msg.TerminalInfo))
			frameType := strings.ToUpper(hexByte(msg.Protocol))
			if err := g.store.RecordStatusWord(opCtx, device.ID, frameType, statusWord, msg.RawHex, msg.UsablePosition()); err != nil {
				logger.Warn("record status word failed", "err", err)
			}
		}

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

		ping := iot.GT06MessageToPing(msg, device)
		if _, err := g.pipeline.Ingest(opCtx, device, []iot.Ping{ping}, ipOnly(remote), logger); err != nil {
			opCancel()
			logger.Error("insert ping failed", "err", err)
			return
		}
		g.pipeline.DeliverPendingCommand(opCtx, conn, device, logger)
		opCancel()
		logger.Info("ping persisted", "protocol", msg.Protocol, "lat", ping.Lat, "lng", ping.Lng)
	}
}

func hexByte(b byte) string {
	const hexdigits = "0123456789ABCDEF"
	return string([]byte{hexdigits[b>>4], hexdigits[b&0x0F]})
}

func ipOnly(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}

// findGT06Device tries the decoded IMEI and common zero-pad variants so a
// registry row stored as a 15-digit IMEI still matches a trimmed login id.
func findGT06Device(ctx context.Context, store gt06Store, imei string) (*iot.Device, error) {
	var last error
	for _, cand := range iot.GT06SerialCandidates(imei) {
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
