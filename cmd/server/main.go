package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"koya-booking/internal/api"
	"koya-booking/internal/booking"
	"koya-booking/internal/store"
	"koya-booking/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		return errors.New("API_KEY must be set")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	var st booking.Store
	if url := os.Getenv("DATABASE_URL"); url != "" {
		pg, err := store.NewPostgres(ctx, url)
		if err != nil {
			return err
		}
		defer pg.Close()
		st = pg
		log.Info("using postgres store")
	} else {
		st = store.NewMemory()
		log.Warn("DATABASE_URL not set: using in-memory store, data is lost on restart")
	}

	srv := api.NewServer(st, apiKey, web.IndexHTML, log)

	// Keep the rate limiter's memory bounded.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				srv.Limiter().Sweep(now)
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpSrv.Addr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
