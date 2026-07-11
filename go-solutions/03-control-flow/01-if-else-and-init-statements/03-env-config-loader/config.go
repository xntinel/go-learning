package envconfig

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	ErrMissing = errors.New("required variable not set")
	ErrInvalid = errors.New("variable has invalid value")
)

type Config struct {
	DBDSN       string
	HTTPPort    int
	ReadTimeout time.Duration
	MaxConns    int
	Debug       bool
}

const (
	defaultReadTimeout = 5 * time.Second
	defaultMaxConns    = 10
)

func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	var cfg Config

	if raw, ok := lookup("DB_DSN"); !ok || raw == "" {
		return Config{}, fmt.Errorf("DB_DSN: %w", ErrMissing)
	} else {
		cfg.DBDSN = raw
	}

	if raw, ok := lookup("HTTP_PORT"); !ok || raw == "" {
		return Config{}, fmt.Errorf("HTTP_PORT: %w", ErrMissing)
	} else if port, err := strconv.Atoi(raw); err != nil {
		return Config{}, fmt.Errorf("HTTP_PORT: %w: %q", ErrInvalid, raw)
	} else if port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("HTTP_PORT: %w: out of range %d", ErrInvalid, port)
	} else {
		cfg.HTTPPort = port
	}

	cfg.ReadTimeout = defaultReadTimeout

	if raw, ok := lookup("READ_TIMEOUT"); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("READ_TIMEOUT: %w: %q", ErrInvalid, raw)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("READ_TIMEOUT: %w must be positive", ErrInvalid)
		}
		cfg.ReadTimeout = d
	}
	cfg.MaxConns = defaultMaxConns
	if raw, ok := lookup("MAX_CONNS"); ok && raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("MAX_CONNS: %w: %q", ErrInvalid, raw)
		}
		if n < 1 {
			return Config{}, fmt.Errorf("MAX_CONNS: %w: must be >= 1", ErrInvalid)
		}
		cfg.MaxConns = int(n)
	}
	cfg.Debug = false

	if raw, ok := lookup("DEBUG"); ok && raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("DEBUG: %w: %q", ErrInvalid, raw)
		}
		cfg.Debug = b
	}
	return cfg, nil
}
