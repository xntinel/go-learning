# 8. Config Loading

Configuration is a precedence problem. Defaults, files, environment variables, and flags all produce values; the CLI must merge them predictably and validate the final result.

## Concepts

### Precedence Must Be Explicit

Use one order and document it: defaults, then config file, then environment, then flags. Flags should win because they are the most explicit user input for this invocation.

### Zero Values Need Source Tracking

A file field with the zero value may mean “not set” or a real value. Pointer fields in the file struct distinguish absent values from present values.

### Environment Parsing Can Fail

Environment variables are strings. Parse them with `strconv.Atoi` or `time.ParseDuration` and return wrapped sentinel errors instead of silently ignoring bad values.

### Validate After Merge

Validate only after all sources have been applied. Otherwise a later source can make a previously valid config invalid.

## Exercises

Set up the module:

```bash
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Load and Merge Configuration

Create `main.go`:

```go
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

var (
	ErrInvalidPort    = errors.New("port must be between 1 and 65535")
	ErrInvalidLog     = errors.New("log level must be debug, info, warn, or error")
	ErrInvalidTimeout = errors.New("timeout must be positive")
	ErrBadEnv         = errors.New("invalid environment value")
)

type config struct {
	Host     string        `json:"host"`
	Port     int           `json:"port"`
	LogLevel string        `json:"log_level"`
	Timeout  time.Duration `json:"timeout"`
}

type fileConfig struct {
	Host     *string `json:"host"`
	Port     *int    `json:"port"`
	LogLevel *string `json:"log_level"`
	Timeout  *string `json:"timeout"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "JSON config file")
	host := fs.String("host", "", "host override")
	port := fs.Int("port", 0, "port override")
	level := fs.String("log-level", "", "log level override")
	timeout := fs.Duration("timeout", 0, "timeout override")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := defaultConfig()
	if *configPath != "" {
		if err := mergeFile(&cfg, *configPath); err != nil {
			return err
		}
	}
	if err := mergeEnv(&cfg, getenv); err != nil {
		return err
	}
	if fs.Lookup("host") != nil && flagChanged(fs, "host") {
		cfg.Host = *host
	}
	if flagChanged(fs, "port") {
		cfg.Port = *port
	}
	if flagChanged(fs, "log-level") {
		cfg.LogLevel = *level
	}
	if flagChanged(fs, "timeout") {
		cfg.Timeout = *timeout
	}
	if err := validate(cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "host=%s port=%d log=%s timeout=%s\n", cfg.Host, cfg.Port, cfg.LogLevel, cfg.Timeout)
	return nil
}

func defaultConfig() config {
	return config{Host: "localhost", Port: 8080, LogLevel: "info", Timeout: 30 * time.Second}
}

func mergeFile(cfg *config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return err
	}
	if fc.Host != nil {
		cfg.Host = *fc.Host
	}
	if fc.Port != nil {
		cfg.Port = *fc.Port
	}
	if fc.LogLevel != nil {
		cfg.LogLevel = *fc.LogLevel
	}
	if fc.Timeout != nil {
		d, err := time.ParseDuration(*fc.Timeout)
		if err != nil {
			return fmt.Errorf("timeout in config: %w", err)
		}
		cfg.Timeout = d
	}
	return nil
}

func mergeEnv(cfg *config, getenv func(string) string) error {
	if v := getenv("APP_HOST"); v != "" {
		cfg.Host = v
	}
	if v := getenv("APP_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%w APP_PORT: %v", ErrBadEnv, err)
		}
		cfg.Port = p
	}
	if v := getenv("APP_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := getenv("APP_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%w APP_TIMEOUT: %v", ErrBadEnv, err)
		}
		cfg.Timeout = d
	}
	return nil
}

func validate(cfg config) error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("%w: got %d", ErrInvalidPort, cfg.Port)
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidLog, cfg.LogLevel)
	}
	if cfg.Timeout <= 0 {
		return fmt.Errorf("%w: got %s", ErrInvalidTimeout, cfg.Timeout)
	}
	return nil
}

func flagChanged(fs *flag.FlagSet, name string) bool {
	changed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			changed = true
		}
	})
	return changed
}
```

### Exercise 2: Test Precedence

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRunAppliesPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"host":"file","port":3000,"log_level":"debug","timeout":"1m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == "APP_PORT" {
			return "4000"
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-config", path, "-port=5000"}, &stdout, &stderr, getenv); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	want := "host=file port=5000 log=debug timeout=1m0s\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config
		want error
	}{
		{"port", config{Port: 0, LogLevel: "info", Timeout: 1}, ErrInvalidPort},
		{"log", config{Port: 80, LogLevel: "trace", Timeout: 1}, ErrInvalidLog},
		{"timeout", config{Port: 80, LogLevel: "info"}, ErrInvalidTimeout},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validate(tc.cfg); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBadEnvironmentValue(t *testing.T) {
	t.Parallel()

	getenv := func(key string) string {
		if key == "APP_PORT" {
			return "bad"
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	err := run(nil, &stdout, &stderr, getenv)
	if !errors.Is(err, ErrBadEnv) {
		t.Fatalf("err = %v, want ErrBadEnv", err)
	}
}

func Example_defaultConfig() {
	cfg := defaultConfig()
	fmt.Print(cfg.Host)
	// Output: localhost
}
```

### Exercise 3: Track Sources

Add a `sources` map that records `default`, `file`, `env`, or `flag` for each field, then print it in deterministic key order.

## Common Mistakes

### Ignoring Environment Parse Errors

Wrong: silently keeping the old port when `APP_PORT=bad`.

What happens: deployment misconfiguration is hidden.

Fix: return a wrapped `ErrBadEnv`.

### Letting File Zero Values Erase Defaults

Wrong: unmarshalling directly into the final config.

What happens: absent fields can become empty strings or zero durations.

Fix: unmarshal into pointer fields and merge only present values.

### Validating Too Early

Wrong: validating defaults before env and flags are applied.

What happens: later sources can invalidate the config without detection.

Fix: validate after the final merge.

## Verification

From `~/go-exercises/config-loading`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add source tracking and rerun the same commands.

## Summary

- Merge configuration in a documented precedence order.
- Use pointer fields to distinguish absent file values.
- Parse environment variables with errors.
- Validate the final config, not each layer in isolation.

## What's Next

Next: [Shell Completion Generation](../09-shell-completion-generation/09-shell-completion-generation.md).

## Resources

- [Package encoding/json](https://pkg.go.dev/encoding/json)
- [Package flag: Visit](https://pkg.go.dev/flag#FlagSet.Visit)
- [Package time: ParseDuration](https://pkg.go.dev/time#ParseDuration)
