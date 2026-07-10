// Command paperplane is the Paper Plane server entrypoint: it loads
// configuration, opens the store, wires the HTTP server, and runs it with
// graceful shutdown.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/kalfian/paper-plane/internal/config"
	"github.com/kalfian/paper-plane/internal/server"
	"github.com/kalfian/paper-plane/internal/sitefs"
	"github.com/kalfian/paper-plane/internal/store"
)

func main() {
	// healthcheck subcommand: probe the local server's health endpoint and
	// exit 0/1 accordingly. Used by the Docker HEALTHCHECK because the
	// distroless runtime image has no shell or curl. Handled before any config
	// loading so the probe stays a cheap, dependency-free HTTP GET.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

// healthcheck performs an HTTP GET to the local server's health endpoint and
// returns a process exit code: 0 if the endpoint responds 200 OK, 1 otherwise.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/_app/healthz")
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// run performs startup wiring and blocks until shutdown, returning any fatal
// error. Splitting this out of main keeps error handling in one place.
func run(log *slog.Logger) error {
	// Best-effort load of a local .env for development convenience. Missing file
	// is not an error: production supplies real environment variables directly.
	// Load never overrides variables already set in the environment.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("could not load .env", slog.Any("error", err))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()

	dbPath := filepath.Join(cfg.DataDir, "paperplane.db")
	st, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	fs := sitefs.New(cfg.DataDir)

	srv, err := server.New(ctx, cfg, st, fs, log)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Listen for interrupt/terminate to trigger graceful shutdown.
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("server listening", slog.String("addr", httpSrv.Addr), slog.String("data_dir", cfg.DataDir))
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	select {
	case serveErr := <-errCh:
		return serveErr
	case <-shutdownCtx.Done():
		log.Info("shutdown signal received")
	}

	graceCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(graceCtx); err != nil {
		return err
	}
	log.Info("server stopped")
	return nil
}
