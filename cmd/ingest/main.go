// Command ingest is the HTTP telemetry ingest API (device API keys).
//
//	DATABASE_URL=... ADDR=:4080 go run ./cmd/ingest
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/iag/fleet-iot/iot"
	"github.com/iag/fleet-iot/pg"
)

func main() {
	configureLogger()
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		if p := os.Getenv("PORT"); p != "" {
			addr = ":" + p
		} else {
			addr = ":4080"
		}
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
		slog.Info("telemetry ingest: split DB (registry + telemetry)")
	}
	hub := iot.NewHubFromEnv()

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(securityHeaders())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.POST("/v1/pings", func(c *gin.Context) { handlePings(c, store, hub) })
	// Legacy path alias for relays configured against fleet /api/iot/pings.
	r.POST("/api/iot/pings", func(c *gin.Context) { handlePings(c, store, hub) })

	srv := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("telemetry HTTP ingest listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	sig := <-stop
	slog.Info("shutdown", "signal", sig.String())
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdownCtx)
}

func handlePings(c *gin.Context, store *iot.Store, hub *iot.Hub) {
	apiKey := bearerToken(c)
	if apiKey == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing Authorization: Bearer <api-key>"})
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := iot.IngestHTTPBatch(c.Request.Context(), store, hub, apiKey, body, c.ClientIP())
	if err != nil {
		if errors.Is(err, iot.ErrInvalidAPIKey) || errors.Is(err, iot.ErrInactiveDevice) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device api key"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, res)
}

// securityHeaders emits the platform-wide baseline browser security header
// set. CSP is strict (no inline scripts, no framing, no form submission, no
// external resource loading) because the telemetry ingest API is JSON-only:
// it returns either {"accepted": n} or an error object, never HTML or static
// assets.
//
// HSTS is gated on TLS termination (direct TLS or trusted X-Forwarded-Proto)
// so plain-HTTP dev environments (http://localhost) do not lock browsers
// into HTTPS for the developer's whole domain.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "geolocation=(), microphone=(), camera=(), interest-cohort=()")
		c.Header("X-XSS-Protection", "1; mode=block")
		if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		}
		c.Next()
	}
}

func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
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
