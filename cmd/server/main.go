// Command server runs the discussion service.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aliassar/discussion-service/internal/discussion"
	"github.com/aliassar/discussion-service/internal/httpapi"
	"github.com/aliassar/discussion-service/internal/store/memory"
)

const shutdownTimeout = 10 * time.Second

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "address to listen on")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*addr, log); err != nil {
		log.Error("server stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(addr string, log *slog.Logger) error {
	svc := discussion.NewService(memory.New())

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewRouter(svc, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		log.Info("shutting down", slog.Duration("timeout", shutdownTimeout))

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown", slog.Any("error", err))
		}
	}()

	log.Info("listening", slog.String("addr", addr))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
