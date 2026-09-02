package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := LoadConfig()
	if err != nil {
		logger.Error("config error", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	console := NewConsole(cfg.ConsoleAddr, cfg.ConsolePassword, cfg.CommandTimeout, logger)
	go console.Run(ctx)

	srv := &server{cfg: cfg, console: console, logger: logger}
	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: newMux(srv),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("starting", "http_addr", cfg.HTTPAddr, "console_addr", cfg.ConsoleAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server error", "error", err)
		os.Exit(1)
	}
}
