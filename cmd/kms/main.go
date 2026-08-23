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

	"github.com/redis/go-redis/v9"

	"kms/internal/api"
	"kms/internal/cache"
	"kms/internal/config"
	"kms/internal/queue"
	"kms/internal/store"
	"kms/internal/subscriber"
)

func main() {
	mode := flag.String("mode", "api", "api | subscriber")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := connectStore(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		slog.Error("redis url", "err", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("redis", "err", err)
		os.Exit(1)
	}
	q := queue.New(rdb, cfg.Stream, cfg.DLQStream, cfg.ConsumerGroup, cfg.MaxRetries, cfg.RetryDelay)

	switch *mode {
	case "api":
		if len(cfg.JWTSecret) < 32 {
			slog.Error("JWT_SECRET must be set (>= 32 chars)")
			os.Exit(1)
		}
		srvr := api.NewServer(st, cache.New(rdb, cfg.CacheTTL), q, cfg)
		if created, err := srvr.Bootstrap(cfg.AdminUsername, cfg.AdminPassword); err != nil {
			slog.Error("bootstrap admin", "err", err)
			os.Exit(1)
		} else if created {
			slog.Info("bootstrap admin user created", "username", cfg.AdminUsername)
		}
		h := srvr.Handler()
		srv := &http.Server{Addr: ":" + cfg.Port, Handler: h, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			slog.Info("api listening", "port", cfg.Port)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("listen", "err", err)
				os.Exit(1)
			}
		}()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	case "subscriber":
		host, _ := os.Hostname()
		slog.Info("subscriber started", "consumer", host, "stream", cfg.Stream)
		sub := subscriber.New(st, cfg.WebhookURL)
		if err := q.Consume(ctx, host, sub.Handle); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("consume", "err", err)
			os.Exit(1)
		}
	default:
		slog.Error("unknown mode", "mode", *mode)
		os.Exit(2)
	}
	slog.Info("bye")
}

// connectStore retries for ~60s so the container survives DB start-up ordering.
func connectStore(ctx context.Context, url string) (*store.Store, error) {
	var last error
	for i := 0; i < 30; i++ {
		st, err := store.New(ctx, url)
		if err == nil {
			return st, nil
		}
		last = err
		slog.Warn("waiting for postgres", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, last
}
