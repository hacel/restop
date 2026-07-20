package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"restop/internal/restic"
	"restop/internal/web"
)

type config struct {
	addr            string
	resticPath      string
	metadataTimeout time.Duration
	maxCommands     int
	maxDownloads    int
	shutdownTimeout time.Duration
	logLevel        slog.Level
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func positiveDuration(name, fallback string) (time.Duration, error) {
	value, err := time.ParseDuration(env(name, fallback))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func positiveInt(name, fallback string) (int, error) {
	value, err := strconv.Atoi(env(name, fallback))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("RESTOP_LOG_LEVEL must be debug, info, warn, or error")
	}
}

func loadConfig() (config, error) {
	metadataTimeout, err := positiveDuration("RESTOP_METADATA_TIMEOUT", "1m")
	if err != nil {
		return config{}, err
	}
	maxCommands, err := positiveInt("RESTOP_MAX_COMMANDS", "8")
	if err != nil {
		return config{}, err
	}
	maxDownloads, err := positiveInt("RESTOP_MAX_DOWNLOADS", "2")
	if err != nil {
		return config{}, err
	}
	if maxDownloads > maxCommands {
		return config{}, errors.New("RESTOP_MAX_DOWNLOADS cannot exceed RESTOP_MAX_COMMANDS")
	}
	shutdownTimeout, err := positiveDuration("RESTOP_SHUTDOWN_TIMEOUT", "30s")
	if err != nil {
		return config{}, err
	}
	logLevel, err := parseLogLevel(env("RESTOP_LOG_LEVEL", "info"))
	if err != nil {
		return config{}, err
	}
	return config{
		addr: env("RESTOP_ADDR", "127.0.0.1:8080"), resticPath: env("RESTOP_RESTIC_PATH", "restic"),
		metadataTimeout: metadataTimeout, maxCommands: maxCommands, maxDownloads: maxDownloads,
		shutdownTimeout: shutdownTimeout, logLevel: logLevel,
	}, nil
}

func run() error {
	configuration, err := loadConfig()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: configuration.logLevel}))
	client := restic.New(configuration.resticPath, configuration.metadataTimeout, configuration.maxCommands, configuration.maxDownloads)
	if err := client.Preflight(context.Background()); err != nil {
		return fmt.Errorf("startup preflight failed: %w", err)
	}
	application, err := web.New(client, logger)
	if err != nil {
		return err
	}
	baseContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{
		Addr: configuration.addr, Handler: application.Handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(_ net.Listener) context.Context { return baseContext },
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("server started", "address", configuration.addr)
		serverErrors <- server.ListenAndServe()
	}()

	// Graceful shutdown lets active downloads finish before cancellation becomes mandatory.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case signal := <-signals:
		logger.Info("shutdown requested", "signal", signal.String())
	}
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), configuration.shutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownContext); err != nil {
		cancelRequests()
		_ = server.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "restop:", err)
		os.Exit(1)
	}
}
