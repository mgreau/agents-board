// Command agents-board runs the board (default) or drives its admin API.
//
//	agents-board                 serve using the BOARD_* environment (see docs/CONTRACT.md (d))
//	agents-board serve           same
//	agents-board admin ...       thin HTTP client for /admin/* (see admin.go)
//	agents-board version         print the build version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mgreau/agents-board/internal/server"
	"github.com/mgreau/agents-board/internal/store"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// shutdownGrace is how long Shutdown waits for in-flight requests. Cloud Run allows 10 s
// after SIGTERM; the remainder is reserved for the final snapshot.
const shutdownGrace = 6 * time.Second

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "admin":
		err = runAdmin(args, os.Stdout, os.Stderr)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agents-board:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: agents-board [serve|admin|version]")
	fmt.Fprintln(w, "  serve    run the board (configured via BOARD_* env vars)")
	fmt.Fprintln(w, "  admin    --url U [--token T] invite|revoke|hide|lock|flags|dismiss ...")
	fmt.Fprintln(w, "  version  print the build version")
}

// serve wires config -> store -> server -> http.Server and handles SIGTERM/SIGINT with a
// graceful shutdown followed by a final snapshot flush.
func serve() error {
	log := newLogger()
	slog.SetDefault(log)

	cfg, err := server.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	cfg.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var blob store.Blob
	var generation int64
	if cfg.SnapshotsEnabled() {
		blob = store.NewGCSBlob(cfg.SnapshotBucket, cfg.SnapshotObject)
		restored, gen, err := store.RestoreIfMissing(ctx, cfg.DBPath, blob, log)
		if err != nil {
			return fmt.Errorf("restore snapshot: %w", err)
		}
		generation = gen
		log.Info("snapshot restore", "restored", restored, "generation", gen,
			"bucket", cfg.SnapshotBucket, "object", cfg.SnapshotObject)
	}

	st, err := store.Open(ctx, cfg.DBPath, store.Options{Blob: blob, Logger: log})
	if err != nil {
		return fmt.Errorf("open store %s: %w", cfg.DBPath, err)
	}
	defer st.Close()
	if blob != nil {
		st.SetGeneration(generation)
	}

	srv, err := server.New(cfg, st, log)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "db", cfg.DBPath,
			"snapshots", cfg.SnapshotsEnabled(), "edge_key", cfg.EdgeKey != "", "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	case <-ctx.Done():
	}
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}

	// Final flush: whatever the coalescing window left behind goes out now. An idle instance
	// (nothing dirty) does not touch the object, so a deploy overlap never sees a needless Put.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelFlush()
	if err := st.SnapshotIfDirty(flushCtx); err != nil {
		log.Error("final snapshot", "err", err)
	}
	return nil
}

// newLogger returns a text logger on a TTY and a JSON logger otherwise (Cloud Run). The JSON
// handler writes Cloud Logging's field names (severity, message) so log-based severity
// filters and alerts see ERROR/WARNING instead of DEFAULT.
func newLogger() *slog.Logger {
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: cloudLoggingAttr}))
}

// cloudLoggingAttr renames slog's level and msg to the keys Cloud Logging parses.
func cloudLoggingAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		level, _ := a.Value.Any().(slog.Level)
		severity := "DEFAULT"
		switch {
		case level >= slog.LevelError:
			severity = "ERROR"
		case level >= slog.LevelWarn:
			severity = "WARNING"
		case level >= slog.LevelInfo:
			severity = "INFO"
		default:
			severity = "DEBUG"
		}
		return slog.String("severity", severity)
	case slog.MessageKey:
		return slog.String("message", a.Value.String())
	}
	return a
}
