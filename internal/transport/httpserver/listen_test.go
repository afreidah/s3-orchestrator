// -------------------------------------------------------------------------------
// HTTP Server - Listen and Run Tests
//
// Author: Alex Freidah
//
// Binding is separate from serving so an address conflict is a startup error
// rather than a goroutine that logs and exits after the process has already
// announced itself ready. These pin that, and the rule that a dead metrics
// listener takes the process with it: an orchestrator serving S3 traffic while
// Prometheus silently receives nothing looks healthy from every angle except
// the graph nobody is watching yet.
// -------------------------------------------------------------------------------

package httpserver

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// addrFor returns an address that was bindable a moment ago. With hold set the
// listener stays open, so the caller can create a bind conflict.
func addrFor(t *testing.T, hold bool) (addr string, occupied net.Listener) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr = ln.Addr().String()
	if hold {
		t.Cleanup(func() { _ = ln.Close() })
		return addr, ln
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return addr, nil
}

// serverOn builds a Server bound to the given addresses without going through
// New, which needs a full injector. Only Listen and Run are under test here.
func serverOn(t *testing.T, mainAddr, metricsAddr string, requireMetrics bool) *Server {
	t.Helper()
	s := &Server{
		main:           &http.Server{Addr: mainAddr, Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second},
		requireMetrics: requireMetrics,
		log:            slog.Default(),
	}
	if metricsAddr != "" {
		s.metrics = &http.Server{Addr: metricsAddr, Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second}
	}
	return s
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestListen_MetricsConflictFailsStartup is the defect: a bind conflict on the
// metrics port used to leave the S3 service running and healthy while
// Prometheus received nothing.
func TestListen_MetricsConflictFailsStartup(t *testing.T) {
	t.Parallel()
	mainAddr, _ := addrFor(t, false)
	metricsAddr, _ := addrFor(t, true) // held, so the bind must fail

	s := serverOn(t, mainAddr, metricsAddr, true)
	err := s.Listen(context.Background())
	if err == nil {
		t.Fatal("a metrics bind conflict must fail startup under the default")
	}

	// The main socket must not be left bound by a failed startup, or a retry or
	// a supervisor restart meets a conflict of the process's own making.
	if s.mainLn != nil {
		t.Error("main listener was left bound after a failed Listen")
	}
	var probe net.ListenConfig
	if ln, lnErr := probe.Listen(t.Context(), "tcp", mainAddr); lnErr != nil {
		t.Errorf("main address still bound after failed Listen: %v", lnErr)
	} else {
		_ = ln.Close()
	}
}

// TestListen_MetricsConflictToleratedWhenOptedOut covers the dev and embedded
// case: startup proceeds without metrics rather than refusing to run.
func TestListen_MetricsConflictToleratedWhenOptedOut(t *testing.T) {
	t.Parallel()
	mainAddr, _ := addrFor(t, false)
	metricsAddr, _ := addrFor(t, true)

	s := serverOn(t, mainAddr, metricsAddr, false)
	if err := s.Listen(context.Background()); err != nil {
		t.Fatalf("with require_listener false, startup should proceed: %v", err)
	}
	t.Cleanup(func() { _ = s.mainLn.Close() })

	if s.metrics != nil || s.metricsLn != nil {
		t.Error("the unusable metrics listener should have been dropped, not kept")
	}
	if s.mainLn == nil {
		t.Error("the main listener should still be bound")
	}
}

// TestListen_MainConflictFails covers the same rule for the listener that
// actually serves traffic.
func TestListen_MainConflictFails(t *testing.T) {
	t.Parallel()
	mainAddr, _ := addrFor(t, true)

	s := serverOn(t, mainAddr, "", true)
	if err := s.Listen(context.Background()); err == nil {
		t.Fatal("a main bind conflict must fail startup")
	}
}

// TestRun_ShutdownIsNotAnError asserts a normal Shutdown of either listener is
// reported as a clean return. http.ErrServerClosed is how a graceful stop
// surfaces and must not be mistaken for a failure.
func TestRun_ShutdownIsNotAnError(t *testing.T) {
	t.Parallel()
	mainAddr, _ := addrFor(t, false)
	metricsAddr, _ := addrFor(t, false)

	s := serverOn(t, mainAddr, metricsAddr, true)
	if err := s.Listen(context.Background()); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	waitUntilServing(t, mainAddr)

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Shutdown(shutCtx)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil for a graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
}

// TestRun_MetricsListenerDeathStopsTheServer is the second half of the defect.
// A metrics listener that dies must take the process down rather than leave it
// serving S3 traffic with no metrics, which is indistinguishable from healthy.
func TestRun_MetricsListenerDeathStopsTheServer(t *testing.T) {
	t.Parallel()
	mainAddr, _ := addrFor(t, false)
	metricsAddr, _ := addrFor(t, false)

	s := serverOn(t, mainAddr, metricsAddr, true)
	if err := s.Listen(context.Background()); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	waitUntilServing(t, mainAddr)

	// Killing the socket out from under Serve is how an unexpected listener
	// death looks; Serve returns an error that is not ErrServerClosed.
	if err := s.metricsLn.Close(); err != nil {
		t.Fatalf("close metrics listener: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("a metrics listener death must surface as an error, not a silent log")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept serving after the metrics listener died")
	}

	// And the main listener must be down with it, not orphaned.
	var d net.Dialer
	if _, err := d.DialContext(t.Context(), "tcp", mainAddr); err == nil {
		t.Error("main listener still accepting after the metrics listener died")
	}
}

// waitUntilServing blocks until addr accepts a connection, so a test does not
// race the goroutine that started serving.
func waitUntilServing(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var d net.Dialer
		conn, err := d.DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never started accepting", addr)
}
