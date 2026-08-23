package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port              string
	DatabaseURL       string
	RedisURL          string
	CacheTTL          time.Duration
	Stream            string
	DLQStream         string
	ConsumerGroup     string
	MaxRetries        int
	RetryDelay        time.Duration
	DefaultTermMonths int // default contract term when none given
	MaxTermMonths     int // sanity cap (0 = unlimited)
	RenewalWindowDays int // renewal allowed when expiry is within this many days
	MinRenewMonths    int // renewal term: flexible, at least this many months, no upper bound
	MaxQuantity       int
	WebhookURL        string

	// Auth
	JWTSecret      string        // required in api mode
	JWTTTL         time.Duration // session length
	APIKey         string        // optional machine access via X-API-Key
	AdminUsername  string        // bootstrap first admin when users table is empty
	AdminPassword  string
	LoginMaxFails  int
	LoginLockout   time.Duration
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(env(k, "")); err == nil {
		return n
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(env(k, "")); err == nil {
		return d
	}
	return def
}

func Load() Config {
	return Config{
		Port:              env("PORT", "8080"),
		DatabaseURL:       env("DATABASE_URL", "postgres://kms:kms@localhost:5432/kms?sslmode=disable"),
		RedisURL:          env("REDIS_URL", "redis://localhost:6379/0"),
		CacheTTL:          envDur("CACHE_TTL", 10*time.Minute),
		Stream:            env("QUEUE_STREAM", "kms.keys"),
		DLQStream:         env("QUEUE_DLQ", "kms.keys.dlq"),
		ConsumerGroup:     env("QUEUE_GROUP", "subscriber"),
		MaxRetries:        envInt("MAX_RETRIES", 5),
		RetryDelay:        envDur("RETRY_DELAY", 5*time.Second),
		DefaultTermMonths: envInt("DEFAULT_TERM_MONTHS", 12),
		MaxTermMonths:     envInt("MAX_TERM_MONTHS", 600),
		RenewalWindowDays: envInt("RENEWAL_WINDOW_DAYS", 60),
		MinRenewMonths:    envInt("MIN_RENEW_MONTHS", 1),
		MaxQuantity:       envInt("MAX_QUANTITY", 1000),
		WebhookURL:        env("WEBHOOK_URL", ""),

		JWTSecret:     env("JWT_SECRET", ""),
		JWTTTL:        envDur("JWT_TTL", 8*time.Hour),
		APIKey:        env("KMS_API_KEY", ""),
		AdminUsername: env("ADMIN_USERNAME", "admin"),
		AdminPassword: env("ADMIN_PASSWORD", ""),
		LoginMaxFails: envInt("LOGIN_MAX_FAILS", 5),
		LoginLockout:  envDur("LOGIN_LOCKOUT", 15*time.Minute),
	}
}
