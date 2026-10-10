// Package config loads runtime settings from environment variables.
// Secrets (database password, service-role key, GSP credentials) only ever
// come from the environment — Railway in deployed environments, .env locally.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env            string // development | production
	Port           string
	DatabaseURL    string // billing_api role via Supabase session pooler
	SupabaseURL    string // https://<ref>.supabase.co
	ServiceRoleKey string // Auth admin (user invites) and Storage; never sent to the browser
	StorageBucket  string // private bucket for PDFs, logos, exports
	AuthRedirect   string // web page the invite / reset emails link to
	EncryptionKey  string // base64 32-byte key for GSP credentials
	AllowedOrigins []string
	RunAPI         bool // serve HTTP
	RunWorker      bool // process River jobs in this process
	MigrateRiver   bool // apply River's queue migrations on start
	LogLevel       slog.Level
}

func (c Config) IsProduction() bool { return c.Env == "production" }

// JWKSURL is where Supabase publishes the public keys that sign access tokens.
func (c Config) JWKSURL() string { return c.SupabaseURL + "/auth/v1/.well-known/jwks.json" }

// JWTIssuer is the "iss" claim of Supabase access tokens.
func (c Config) JWTIssuer() string { return c.SupabaseURL + "/auth/v1" }

func Load() (Config, error) {
	c := Config{
		Env:            getenv("APP_ENV", "development"),
		Port:           getenv("PORT", "8080"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		SupabaseURL:    strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		ServiceRoleKey: os.Getenv("SUPABASE_SERVICE_ROLE_KEY"),
		StorageBucket:  getenv("STORAGE_BUCKET", "documents"),
		AuthRedirect:   os.Getenv("AUTH_REDIRECT_URL"),
		EncryptionKey:  os.Getenv("APP_ENCRYPTION_KEY"),
		AllowedOrigins: splitList(getenv("CORS_ALLOWED_ORIGINS", "http://localhost:5173")),
	}

	var errs []error
	var err error
	if c.RunAPI, err = getbool("RUN_API", true); err != nil {
		errs = append(errs, err)
	}
	if c.RunWorker, err = getbool("RUN_WORKER", true); err != nil {
		errs = append(errs, err)
	}
	if c.MigrateRiver, err = getbool("RIVER_MIGRATE", true); err != nil {
		errs = append(errs, err)
	}
	if err = c.LogLevel.UnmarshalText([]byte(getenv("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.RunAPI && c.SupabaseURL == "" {
		errs = append(errs, errors.New("SUPABASE_URL is required when RUN_API is true"))
	}
	if c.Env != "development" && c.Env != "production" {
		errs = append(errs, fmt.Errorf("APP_ENV must be development or production, got %q", c.Env))
	}
	if !c.RunAPI && !c.RunWorker {
		errs = append(errs, errors.New("at least one of RUN_API or RUN_WORKER must be true"))
	}
	return c, errors.Join(errs...)
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getbool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
