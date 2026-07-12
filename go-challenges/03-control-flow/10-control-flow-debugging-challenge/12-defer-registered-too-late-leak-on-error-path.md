# Exercise 12: The Config Loader Whose defer Never Ran on the Error Path

**Nivel: Intermedio** — validacion rapida (un test corto).

A config loader opens a handle, then runs two validation checks before
scheduling the cleanup with `defer`. Each validation check returns early on
failure — and because `defer r.Close(h)` sits *after* both checks, neither
error path ever registers it. The handle leaks on exactly the inputs that
should have been rejected fastest. You will reproduce it against each error
path, diagnose the ordering, and fix it by registering the `defer`
immediately after the open.

## What you'll build

```text
config/                     module example.com/config
  go.mod
  config.go                 Registry (open-handle counter), LoadConfig
  config_test.go             table over both error paths + the success path
```

- Files: `config.go`, `config_test.go`.
- Implement: `LoadConfig(*Registry, raw string, size int) (*Config, error)` that opens a handle and closes it on every return, including both validation failures.
- Test: a table with an empty-source row, an empty-config row, and a success row, each asserting the returned error *and* that `Registry.OpenCount()` is `0` afterward.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p go-solutions/03-control-flow/10-control-flow-debugging-challenge/12-defer-registered-too-late-leak-on-error-path
cd go-solutions/03-control-flow/10-control-flow-debugging-challenge/12-defer-registered-too-late-leak-on-error-path
```

### The artifact and the planted bug

```go
func LoadConfig(r *Registry, raw string, size int) (*Config, error) {
	h := r.Open()

	if raw == "" {
		return nil, ErrEmptySource // BUG: h is never closed on this path
	}
	if size == 0 {
		return nil, ErrEmptyConfig // BUG: same
	}
	defer r.Close(h) // registered too late to cover the checks above
	return &Config{Raw: raw}, nil
}
```

`defer` only takes effect from the line it executes onward; it does not
retroactively cover returns that already happened above it. Both validation
checks return before the `defer` statement ever runs, so on either error path
the handle opened at the top of the function is never released — this is
cleanup that is never scheduled at all, not cleanup that runs too early. It
reads fine in review because the happy path is exactly correct; only the
rejection paths leak, and rejections are the inputs nobody stares at twice.

The failing rows read:

```text
--- FAIL: TestLoadConfigClosesHandleOnEveryErrorPath/empty_source
    config_test.go:27: open handles after LoadConfig = 1, want 0
--- FAIL: TestLoadConfigClosesHandleOnEveryErrorPath/empty_config
    config_test.go:27: open handles after LoadConfig = 1, want 0
```

The fix moves `defer r.Close(h)` to the line immediately after `r.Open()`, so
every return below it — success or failure — is covered:

```go
func LoadConfig(r *Registry, raw string, size int) (*Config, error) {
	h := r.Open()
	defer r.Close(h)

	if raw == "" {
		return nil, ErrEmptySource
	}
	if size == 0 {
		return nil, ErrEmptyConfig
	}
	return &Config{Raw: raw}, nil
}
```

Create `config.go`:

```go
package config

import "errors"

var ErrEmptySource = errors.New("empty source")
var ErrEmptyConfig = errors.New("empty config")

// Handle stands in for an open file descriptor, a DB cursor, or a lease token.
type Handle struct{ closed bool }

// Registry tracks how many handles are currently open.
type Registry struct{ openCount int }

func (r *Registry) Open() *Handle {
	r.openCount++
	return &Handle{}
}

func (r *Registry) Close(h *Handle) {
	if h.closed {
		return
	}
	h.closed = true
	r.openCount--
}

func (r *Registry) OpenCount() int { return r.openCount }

// Config is the decoded configuration.
type Config struct{ Raw string }

// LoadConfig opens a handle, validates the source, and decodes it, closing
// the handle on every return path — success or failure.
func LoadConfig(r *Registry, raw string, size int) (*Config, error) {
	h := r.Open()
	defer r.Close(h)

	if raw == "" {
		return nil, ErrEmptySource
	}
	if size == 0 {
		return nil, ErrEmptyConfig
	}
	return &Config{Raw: raw}, nil
}
```

### Tests

Create `config_test.go`:

```go
package config

import (
	"errors"
	"testing"
)

func TestLoadConfigClosesHandleOnEveryErrorPath(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		size    int
		wantErr error
	}{
		{name: "empty source", raw: "", size: 4, wantErr: ErrEmptySource},
		{name: "empty config", raw: "x=1", size: 0, wantErr: ErrEmptyConfig},
		{name: "ok", raw: "x=1", size: 4, wantErr: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Registry{}
			_, err := LoadConfig(r, tc.raw, tc.size)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("LoadConfig error = %v, want %v", err, tc.wantErr)
			}
			if got := r.OpenCount(); got != 0 {
				t.Fatalf("open handles after LoadConfig = %d, want 0", got)
			}
		})
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`defer` schedules a call from the point it executes, not from function entry
— a validation check that returns above it bypasses it entirely. The rule
that generalizes: register cleanup on the very next line after the resource
is acquired, before any branch that can return, so no future edit to the
validation logic can outrun it again. The test exercises both error paths
specifically, since a table that only checked the success path would never
have caught this.

## Resources

- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — `defer` arguments are evaluated when the statement runs, and the call fires at function return.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a `defer` before it executes has no effect on returns that precede it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-batch-chunking-off-by-one-drops-tail.md](11-batch-chunking-off-by-one-drops-tail.md) | Next: [13-range-over-map-assumed-ordered-pagination.md](13-range-over-map-assumed-ordered-pagination.md)
