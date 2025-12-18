// -------------------------------------------------------------------------------
// S3 Proxy - Unified S3 Endpoint
//
// Project: Munchbox / Author: Alex Freidah
//
// Entry point for the S3 proxy service. Loads configuration, initializes the
// backend client, and starts the HTTP server. Handles graceful shutdown on
// SIGINT/SIGTERM to allow in-flight requests to complete.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// --- Load configuration ---
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// --- Initialize tracing ---
	ctx := context.Background()
	shutdownTracer, err := InitTracer(ctx, cfg.Telemetry.Tracing)
	if err != nil {
		log.Fatalf("Failed to initialize tracer: %v", err)
	}

	// --- Set build info metric ---
	BuildInfo.WithLabelValues(Version, runtime.Version()).Set(1)

	// --- Initialize backend ---
	backend, err := NewS3Backend(cfg.Backend)
	if err != nil {
		log.Fatalf("Failed to initialize backend: %v", err)
	}

	// --- Create server ---
	server := &Server{
		Backend:       backend,
		BackendConfig: cfg.Backend,
		VirtualBucket: cfg.Server.VirtualBucket,
		AuthToken:     cfg.Auth.Token,
	}

	// --- Setup HTTP mux ---
	mux := http.NewServeMux()

	// Metrics endpoint
	if cfg.Telemetry.Metrics.Enabled {
		mux.Handle(cfg.Telemetry.Metrics.Path, promhttp.Handler())
		log.Printf("Metrics endpoint: %s", cfg.Telemetry.Metrics.Path)
	}

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// S3 proxy handler (all other paths)
	mux.Handle("/", server)

	httpServer := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	// --- Handle graceful shutdown ---
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		log.Println("Shutting down...")

		// Shutdown HTTP server with timeout
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}

		// Flush traces
		if err := shutdownTracer(shutdownCtx); err != nil {
			log.Printf("Tracer shutdown error: %v", err)
		}
	}()

	// --- Log startup info ---
	log.Printf("S3 Proxy v%s starting on %s", Version, cfg.Server.ListenAddr)
	log.Printf("Virtual bucket: %s", cfg.Server.VirtualBucket)
	log.Printf("Backend [%s]: %s/%s", cfg.Backend.Name, cfg.Backend.Endpoint, cfg.Backend.Bucket)

	if cfg.Auth.Token == "" {
		log.Println("WARNING: Authentication is disabled")
	}

	if cfg.Telemetry.Tracing.Enabled {
		log.Printf("Tracing enabled: %s (sample rate: %.2f)", cfg.Telemetry.Tracing.Endpoint, cfg.Telemetry.Tracing.SampleRate)
	}

	// --- Start server ---
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log.Println("Server stopped")
}
