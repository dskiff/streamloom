package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/routes"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/joho/godotenv"
)

// Injected at build time
var Version string = "development"

var loggerLevel = new(slog.LevelVar)
var logger *slog.Logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
	Level: loggerLevel,
}))

func main() {
	isDevMode := Version == "development"
	if isDevMode {
		for _, f := range []string{".env", ".env.development"} {
			if err := godotenv.Load(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Warn("failed to load env file", "file", f, "error", err)
			}
		}
	}

	env, err := config.GetEnv()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	isDebugLogsEnabled := env.DEBUG || isDevMode
	if isDebugLogsEnabled {
		loggerLevel.Set(slog.LevelDebug)
	} else {
		loggerLevel.Set(slog.LevelInfo)
	}

	slog.SetDefault(logger)
	logger.Info("starting", "version", Version, "debug", isDebugLogsEnabled)

	var requestLogger *slog.Logger
	if env.REQUEST_LOG_FILE != "" {
		f, err := os.OpenFile(env.REQUEST_LOG_FILE, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			logger.Error("failed to open request log file", "path", env.REQUEST_LOG_FILE, "error", err)
			os.Exit(1)
		}
		defer f.Close()
		requestLogger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
		logger.Info("request logging enabled", "file", env.REQUEST_LOG_FILE)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if len(env.STREAM_TOKENS) == 0 {
		logger.Error("no stream tokens configured. Set SL_STREAM_<id>_TOKEN env vars.")
		os.Exit(1)
	}
	for id := range env.STREAM_TOKENS {
		logger.Info("stream token configured", "streamID", id)
	}

	clk := clock.Real{}
	store := stream.NewStore(clk)
	tracker := watcher.NewTracker(clk)

	go func() {
		ticker := time.NewTicker(watcher.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tracker.Cleanup()
			}
		}
	}()

	serverWg := sync.WaitGroup{}

	// Bind address priority: SL_BIND_ADDR env var > mode default.
	// In containers, binding to all interfaces (0.0.0.0) is correct — restrict
	// host exposure via port mapping (e.g. podman run -p 127.0.0.1:8080:8080).
	var addr string
	if env.BIND_ADDR != "" {
		addr = env.BIND_ADDR
	} else if isDevMode {
		addr = "127.0.0.1"
	}

	if env.STREAM_PORT == env.API_PORT {
		logger.Error("SL_STREAM_PORT and SL_API_PORT must be different", "port", env.STREAM_PORT)
		os.Exit(1)
	}

	streamAddr := fmt.Sprintf("%s:%d", addr, env.STREAM_PORT)
	streamRouter := routes.Stream(logger, env, store, requestLogger, tracker)
	runServerThread(ctx, &serverWg, streamAddr, streamRouter)

	apiAddr := fmt.Sprintf("%s:%d", addr, env.API_PORT)
	apiRouter := routes.API(logger, env, store, requestLogger, tracker)
	runServerThread(ctx, &serverWg, apiAddr, apiRouter)

	<-ctx.Done()
	logger.Info("shutting down...")

	go func() {
		<-time.After(config.SHUTDOWN_TIMEOUT)
		logger.Error("timeout waiting for server to shutdown")
		os.Exit(1)
	}()

	serverWg.Wait()
}

func runServerThread(ctx context.Context, wg *sync.WaitGroup, addr string, handler http.Handler) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: config.READ_HEADER_TIMEOUT,
		ReadTimeout:       config.READ_TIMEOUT,
		WriteTimeout:      config.WRITE_TIMEOUT,
		IdleTimeout:       config.IDLE_TIMEOUT,
		MaxHeaderBytes:    config.MAX_HEADER_BYTES,
	}

	go func() {
		logger.Info("starting server", "address", "http://"+addr)

		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			logger.Error("failed to start http server", "error", err)
			os.Exit(1)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), config.SHUTDOWN_TIMEOUT-1*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("server threw when shutting down:", "error", err)
		}

		logger.Info("server shutdown complete", "address", addr)
	}()
}
