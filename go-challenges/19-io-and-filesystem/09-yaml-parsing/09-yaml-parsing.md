# 9. YAML Parsing

Build a tiny YAML-subset loader for flat `key: value` configuration. Go has no YAML parser in the standard library; production systems usually use a maintained external module, while this exercise keeps the parser intentionally small so the code is testable offline and the supported grammar is explicit.

## Concepts

### YAML Is Not In The Standard Library

The standard library includes JSON, CSV, XML, and archive formats, but not YAML. Do not invent a nonexistent `encoding/yaml` package. If a real application needs full YAML, choose a maintained module and pin it in `go.mod`.

### A Subset Parser Needs A Contract

A small parser is only honest if it says what it supports. This lesson supports comments, blank lines, and unquoted scalar `key: value` pairs. It rejects nested values and malformed lines.

### Errors Should Identify The Line

Configuration errors are operational errors. Include the line number and wrap a sentinel error so callers can both display useful context and test with `errors.Is`.

## Exercises

### Exercise 1: Implement A Flat YAML Loader

Create `yaml.go`:

```go
package simpleyaml

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type Config struct {
	Values map[string]string
}

func Parse(r io.Reader) (Config, error) {
	if r == nil {
		return Config{}, fmt.Errorf("parse yaml: %w", ErrNilReader)
	}
	cfg := Config{Values: map[string]string{}}
	s := bufio.NewScanner(r)
	for line := 1; s.Scan(); line++ {
		raw := strings.TrimSpace(s.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		key, value, ok := strings.Cut(raw, ":")
		if !ok {
			return Config{}, fmt.Errorf("line %d: %w", line, ErrMalformedLine)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return Config{}, fmt.Errorf("line %d: %w", line, ErrEmptyKey)
		}
		if strings.HasSuffix(key, "-") || strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") {
			return Config{}, fmt.Errorf("line %d: %w", line, ErrUnsupportedValue)
		}
		cfg.Values[key] = value
	}
	if err := s.Err(); err != nil {
		return Config{}, fmt.Errorf("scan yaml: %w", err)
	}
	return cfg, nil
}

func (c Config) Require(key string) (string, error) {
	value, ok := c.Values[key]
	if !ok || value == "" {
		return "", fmt.Errorf("require %q: %w", key, ErrMissingKey)
	}
	return value, nil
}
```

Create `errors.go`:

```go
package simpleyaml

import "errors"

var (
	ErrNilReader        = errors.New("reader must not be nil")
	ErrMalformedLine    = errors.New("line must be key: value")
	ErrEmptyKey         = errors.New("key must not be empty")
	ErrUnsupportedValue = errors.New("value is outside the supported flat scalar subset")
	ErrMissingKey       = errors.New("required key is missing")
)
```

### Exercise 2: Test The Contract

Create `yaml_test.go`:

```go
package simpleyaml

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseReadsFlatScalars(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(strings.NewReader("# comment\nname: demo\nport: 8080\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Values["name"] != "demo" || cfg.Values["port"] != "8080" {
		t.Fatalf("values = %+v", cfg.Values)
	}
}

func TestParseValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "malformed", input: "name demo\n", want: ErrMalformedLine},
		{name: "empty key", input: ": demo\n", want: ErrEmptyKey},
		{name: "unsupported", input: "items: [a, b]\n", want: ErrUnsupportedValue},
	}
	for _, tt := range tests {
		_, err := Parse(strings.NewReader(tt.input))
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestRequireReportsMissingKey(t *testing.T) {
	t.Parallel()

	_, err := (Config{Values: map[string]string{}}).Require("name")
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
}

func ExampleParse() {
	cfg, _ := Parse(strings.NewReader("name: demo\n"))
	fmt.Println(cfg.Require("name"))
	// Output: demo <nil>
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

	"example.com/simpleyaml"
)

func main() {
	cfg, err := simpleyaml.Parse(strings.NewReader("name: demo\nmode: local\n"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(cfg.Values["name"])
}
```

## Common Mistakes

### Importing A Nonexistent Package

Wrong: `import "encoding/yaml"`.

Fix: use a real external YAML module in production, or clearly document a tiny supported subset as this exercise does.

### Accepting More YAML Than You Test

Wrong: claim support for nested mappings, anchors, or lists without tests.

Fix: reject unsupported shapes with `ErrUnsupportedValue`.

### Losing Line Numbers

Wrong: return only `ErrMalformedLine`.

Fix: wrap it with `line N: %w` so the caller can locate the problem.

## Verification

Run this from `~/go-exercises/simpleyaml`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test for `Parse(nil)` and assert `errors.Is(err, ErrNilReader)`.

## Summary

- Go has no standard-library YAML parser.
- A subset parser must document and test its supported grammar.
- Configuration errors should include line context and wrap sentinel errors.
- Full YAML support belongs in a pinned external module, not a hand-rolled partial parser hidden as a complete one.

## What's Next

Next: [TOML Config Files](../10-toml-config-files/10-toml-config-files.md).

## Resources

- [Go standard library packages](https://pkg.go.dev/std)
- [bufio.Scanner](https://pkg.go.dev/bufio#Scanner)
- [strings.Cut](https://pkg.go.dev/strings#Cut)
