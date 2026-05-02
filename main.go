// Cloudpulse / Global Health Monitor entrypoint.
//
// Wires together the zap logger, OpenTelemetry meter provider (with a
// Prometheus exporter), and a Gin HTTP server that exposes:
//
//	POST /check    - concurrent URL health checks
//	GET  /healthz  - liveness probe
//	GET  /metrics  - Prometheus scrape endpoint
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/wayne-ngo/cloudpulse/internal/metrics"
	"github.com/wayne-ngo/cloudpulse/internal/monitor"
)

const (
	defaultAddr           = ":8080"
	defaultClientTimeout  = 5 * time.Second
	shutdownGraceDeadline = 10 * time.Second
	serviceVersion        = "0.1.0"
)

// CheckRequest is the inbound POST /check payload.
type CheckRequest struct {
	URLs []string `json:"urls" binding:"required,min=1,dive,required"`
}

// CheckResponse wraps the slice of per-URL outcomes.
type CheckResponse struct {
	Results []monitor.CheckResult `json:"results"`
}

func main() {
	if err := run(); err != nil {
		// We can't rely on the structured logger here because the failure
		// may have been initializing it; stderr is the safest fallback.
		_, _ = os.Stderr.WriteString("fatal: " + err.Error() + "\n")
		os.Exit(1)
	}
}

func run() error {
	logger, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	provider, err := metrics.NewProvider(serviceVersion)
	if err != nil {
		logger.Error("init metrics", zap.Error(err))
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGraceDeadline)
		defer cancel()
		if err := provider.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics shutdown", zap.Error(err))
		}
	}()

	pinger := monitor.New(
		&http.Client{Timeout: defaultClientTimeout},
		logger,
		provider.Instruments,
	)

	router := newRouter(logger, pinger)

	addr := envOrDefault("CLOUDPULSE_ADDR", defaultAddr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		logger.Info("shutdown signal received", zap.String("signal", sig.String()))
	case err := <-serverErr:
		logger.Error("server error", zap.Error(err))
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGraceDeadline)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown failed", zap.Error(err))
		return err
	}
	logger.Info("server stopped cleanly")
	return nil
}

// newRouter builds the Gin engine. Exported indirectly via tests so handler
// behavior can be exercised without booting the full server.
func newRouter(logger *zap.Logger, pinger *monitor.Pinger) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.POST("/check", func(c *gin.Context) {
		var req CheckRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			logger.Warn("invalid /check payload", zap.Error(err))
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		results := pinger.CheckURLs(c.Request.Context(), req.URLs)
		c.JSON(http.StatusOK, CheckResponse{Results: results})
	})

	return r
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
