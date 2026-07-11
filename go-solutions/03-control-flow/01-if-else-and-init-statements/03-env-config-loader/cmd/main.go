package main

import (
	"errors"
	"fmt"

	envconfig "github.com/sentinel/go-learning/go-solutions/03-control-flow/01-if-else-and-init-statements/03-env-config-loader"
)

func main() {
	full := map[string]string{
		"DB_DSN":       "postgres://localhost/app",
		"HTTP_PORT":    "8080",
		"READ_TIMEOUT": "3s",
		"MAX_CONNS":    "25",
		"DEBUG":        "true",
	}
	lookup := func(key string) (string, bool) {
		v, ok := full[key]
		return v, ok
	}
	cfg, err := envconfig.LoadConfig(lookup)
	if err != nil {
		fmt.Println("load failed:", err)
		return
	}
	fmt.Printf("port=%d timeout=%s maxconns=%d debug=%v\n",
		cfg.HTTPPort, cfg.ReadTimeout, cfg.MaxConns, cfg.Debug,
	)
	empty := func(string) (string, bool) {
		return "", false
	}
	if _, err := envconfig.LoadConfig(empty); errors.Is(err, envconfig.ErrMissing) {
		fmt.Println("empty env:", err)
	}
}
