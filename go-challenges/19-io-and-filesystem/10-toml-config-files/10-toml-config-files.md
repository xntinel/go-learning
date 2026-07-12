# 10. TOML Config Files

Build a small TOML-subset config loader for `key = "value"` and `key = 123` assignments. TOML is not part of the Go standard library, so this lesson keeps the grammar narrow and explicit instead of pretending to be a complete TOML implementation.

## Concepts

### TOML Is A Configuration Language

TOML is designed for human-edited configuration. Real TOML supports tables, arrays, dates, booleans, quoted strings, and more. This exercise supports only top-level string and integer assignments so the failure modes are visible.

### Type Conversion Is Part Of Parsing

Configuration files are text. A loader should convert fields to the types its API promises and return typed errors when conversion fails.

### Defaults And Required Fields Are Separate

A default fills an omitted optional field. A required field must be present. Mixing those ideas hides configuration mistakes.

## Exercises

### Exercise 1: Implement A Config Loader

Create `config.go`:

```go
package simpletoml

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type Config struct {
	Host string
	Port int
}

func Load(r io.Reader) (Config, error) {
	if r == nil {
		return Config{}, fmt.Errorf("load toml: %w", ErrNilReader)
	}
	values := map[string]string{}
	s := bufio.NewScanner(r)
	for line := 1; s.Scan(); line++ {
		raw := strings.TrimSpace(s.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		key, value, ok := strings.Cut(raw, "=")
		if !ok {
			return Config{}, fmt.Errorf("line %d: %w", line, ErrMalformedLine)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return Config{}, fmt.Errorf("line %d: %w", line, ErrEmptyKey)
		}
		values[key] = strings.Trim(value, `"`)
	}
	if err := s.Err(); err != nil {
		return Config{}, fmt.Errorf("scan toml: %w", err)
	}

	host := values["host"]
	if host == "" {
		return Config{}, fmt.Errorf("host: %w", ErrMissingRequired)
	}
	port := 8080
	if raw := values["port"]; raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("port: %w", ErrBadInteger)
		}
		port = parsed
	}
	if port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("port: %w", ErrInvalidPort)
	}
	return Config{Host: host, Port: port}, nil
}
```

Create `errors.go`:

```go
package simpletoml

import "errors"

var (
	ErrNilReader       = errors.New("reader must not be nil")
	ErrMalformedLine   = errors.New("line must be key = value")
	ErrEmptyKey        = errors.New("key must not be empty")
	ErrMissingRequired = errors.New("required field is missing")
	ErrBadInteger      = errors.New("value must be an integer")
	ErrInvalidPort     = errors.New("port must be between 1 and 65535")
)
```

### Exercise 2: Test Defaults And Errors

Create `config_test.go`:

```go
package simpletoml

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLoadReadsHostAndPort(t *testing.T) {
	t.Parallel()

	cfg, err := Load(strings.NewReader("host = \"localhost\"\nport = 9090\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" || cfg.Port != 9090 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadAppliesDefaultPort(t *testing.T) {
	t.Parallel()

	cfg, err := Load(strings.NewReader("host = \"localhost\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 {
		t.Fatalf("port = %d, want 8080", cfg.Port)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "missing host", input: "port = 80\n", want: ErrMissingRequired},
		{name: "bad integer", input: "host = \"x\"\nport = nope\n", want: ErrBadInteger},
		{name: "bad port", input: "host = \"x\"\nport = 70000\n", want: ErrInvalidPort},
		{name: "malformed", input: "host \"x\"\n", want: ErrMalformedLine},
	}
	for _, tt := range tests {
		_, err := Load(strings.NewReader(tt.input))
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func ExampleLoad() {
	cfg, _ := Load(strings.NewReader("host = \"localhost\"\n"))
	fmt.Println(cfg.Host, cfg.Port)
	// Output: localhost 8080
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"example.com/simpletoml"
)

func main() {
	cfg, err := simpletoml.Load(strings.NewReader("host = \"demo.local\"\nport = 8081\n"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s:%d\n", cfg.Host, cfg.Port)
}
```

## Common Mistakes

### Treating This As Full TOML

Wrong: accept arrays and tables in prose while the code only parses `key = value`.

Fix: document the subset and reject malformed input.

### Defaulting Required Fields

Wrong: silently use `localhost` when `host` is missing.

Fix: return `ErrMissingRequired`; missing host is probably a broken config.

### Matching Conversion Error Text

Wrong: assert the exact `strconv.Atoi` error string.

Fix: wrap `ErrBadInteger` and use `errors.Is`.

## Verification

Run this from `~/go-exercises/simpletoml`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test for `Load(nil)` and assert `errors.Is(err, ErrNilReader)`.

## Summary

- TOML is not in the Go standard library.
- A subset parser must be explicit about what it supports.
- Convert text to typed fields at the boundary.
- Required fields and defaults should be handled separately.

## What's Next

Next: [stdin/stdout Piping](../11-stdin-stdout-piping/11-stdin-stdout-piping.md).

## Resources

- [Go standard library packages](https://pkg.go.dev/std)
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi)
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner)
