# Exercise 30: Dispatch Config Change Signals (Full Reload, Patch, Validation)

**Nivel: Intermedio** — validacion rapida (un test corto).

A service that reads its configuration once at startup and never again
forces every change — a new timeout, a rotated credential, a feature flag
— through a full restart, which for anything serving live traffic means a
connection drop for every in-flight request. A hot-reload path avoids that,
but it opens a new failure mode: what happens when the new configuration is
itself broken? A reload dispatcher has to distinguish between three
fundamentally different operator intents — replace the configuration
wholesale, merge a partial delta into what is already running, or just
check whether a candidate configuration would be accepted without ever
touching the live one — and it must never let a rejected change leave the
service half-configured. This module is fully self-contained: its own `go
mod init`, all code inline, its own demo and tests.

## What you'll build

```text
graceful-config-reload-dispatcher/   independent module: example.com/graceful-config-reload-dispatcher
  go.mod                             go 1.24
  reloader.go                        (*Reloader).Handle(signal any) error
  cmd/
    demo/
      main.go                        a patch applies, a full reload rolls back, a dry-run validates
  reloader_test.go                     table of cases asserting both the error and the resulting state
```

- Files: `reloader.go`, `cmd/demo/main.go`, `reloader_test.go`.
- Implement: `(*Reloader).Handle(signal any) error`, type-switching on
  `FullReload`, `Patch`, and `ValidateOnly` to decide what to validate and
  whether to commit it.
- Test: a full reload committing a valid configuration, a full reload with
  an invalid configuration rolling back and leaving the previous
  configuration untouched, a patch merging into and validating the whole
  result (not just the delta), a patch that would blank a required key
  rolling back, a validate-only signal that never mutates state whether it
  passes or fails, and an unsupported signal type.

Set up the module:

```bash
go mod edit -go=1.24
```

The one property every branch of `Handle` shares, and the one worth
reading the code for even before the type switch itself, is that `current`
is only ever reassigned *after* `validate` has already succeeded — never
before, and never conditionally rolled back after the fact. That ordering
is what makes a rejected reload a true no-op rather than a service running
on a configuration nobody approved. It is also why `Patch` validates the
*merged* result instead of just the incoming `Changes` map: a patch that
only sets `timeout_seconds` looks perfectly valid in isolation, but if the
live configuration it is merging into never had `listen_addr` set — which
should never happen in practice, but a validator's job is to not have to
trust that — validating the delta alone would miss it, while validating
the full merged candidate catches it unconditionally. `ValidateOnly`
reuses that same `validate` function against a config the caller has no
intention of committing yet, which is what makes it a genuine dry run
rather than a second, subtly different implementation of the same rules
that could drift from the real one over time.

Create `reloader.go`:

```go
package reloader

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrInvalidConfig is returned when a candidate configuration fails
// validation, whether it arrived as a full reload, a merged patch, or a
// dry-run validation request.
var ErrInvalidConfig = errors.New("reloader: invalid configuration")

// FullReload replaces the entire live configuration with Config.
type FullReload struct {
	Config map[string]string
}

// Patch merges Changes into the live configuration without touching keys
// it does not mention.
type Patch struct {
	Changes map[string]string
}

// ValidateOnly checks Config against the same rules a reload would apply,
// without ever touching the live configuration — the dry-run an operator
// runs before pushing a change that would otherwise only be validated at
// the moment it goes live.
type ValidateOnly struct {
	Config map[string]string
}

// Reloader holds the live configuration a running service reads from. All
// mutation goes through Handle, which validates a complete candidate before
// ever assigning into current, so a rejected reload or patch leaves the
// service running on its last-known-good configuration instead of a
// half-applied one.
type Reloader struct {
	current map[string]string
}

// New returns a Reloader seeded with an already-valid initial
// configuration.
func New(initial map[string]string) *Reloader {
	return &Reloader{current: cloneMap(initial)}
}

// Current returns a copy of the live configuration.
func (r *Reloader) Current() map[string]string {
	return cloneMap(r.current)
}

// Handle applies one reload signal. FullReload and Patch both validate a
// complete candidate configuration before committing it — Patch validates
// the merged result, not just the delta, because a patch that looks fine in
// isolation can still leave the merged configuration missing a required key
// that was never set before this patch, and validating only the delta would
// miss that. ValidateOnly runs the identical check against a candidate the
// caller is not yet committing.
func (r *Reloader) Handle(signal any) error {
	switch s := signal.(type) {
	case FullReload:
		if err := validate(s.Config); err != nil {
			return err
		}
		r.current = cloneMap(s.Config)
		return nil

	case Patch:
		merged := cloneMap(r.current)
		for k, v := range s.Changes {
			merged[k] = v
		}
		if err := validate(merged); err != nil {
			return err
		}
		r.current = merged
		return nil

	case ValidateOnly:
		return validate(s.Config)

	default:
		return fmt.Errorf("%w: unsupported signal type %T", ErrInvalidConfig, signal)
	}
}

func cloneMap(m map[string]string) map[string]string {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// validate enforces the two rules every live configuration must satisfy: a
// non-empty listen_addr, and a timeout_seconds that parses as a positive
// integer.
func validate(cfg map[string]string) error {
	if cfg["listen_addr"] == "" {
		return fmt.Errorf("%w: listen_addr is required", ErrInvalidConfig)
	}
	timeout, ok := cfg["timeout_seconds"]
	if !ok {
		return fmt.Errorf("%w: timeout_seconds is required", ErrInvalidConfig)
	}
	n, err := strconv.Atoi(timeout)
	if err != nil || n <= 0 {
		return fmt.Errorf("%w: timeout_seconds must be a positive integer, got %q", ErrInvalidConfig, timeout)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/graceful-config-reload-dispatcher"
)

func main() {
	r := reloader.New(map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"})

	signals := []any{
		reloader.Patch{Changes: map[string]string{"timeout_seconds": "45"}},
		reloader.FullReload{Config: map[string]string{"listen_addr": ":9090"}}, // missing timeout_seconds
		reloader.ValidateOnly{Config: map[string]string{"listen_addr": ":9090", "timeout_seconds": "60"}},
	}

	for _, s := range signals {
		err := r.Handle(s)
		if err != nil {
			fmt.Printf("%-20T -> rejected: %v\n", s, err)
			continue
		}
		fmt.Printf("%-20T -> applied\n", s)
	}

	fmt.Println("final config:", r.Current())
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
reloader.Patch       -> applied
reloader.FullReload  -> rejected: reloader: invalid configuration: timeout_seconds is required
reloader.ValidateOnly -> applied
final config: map[listen_addr::8080 timeout_seconds:45]
```

The full reload is rejected because it omits `timeout_seconds` entirely,
and the live configuration remains exactly what the earlier patch left it
at — `:8080` with the patched `45`-second timeout — proving the rejected
reload never touched `current`. The validate-only signal reports success
without ever being reflected in the final config, since it was checking a
candidate, not committing one.

### Tests

Create `reloader_test.go`:

```go
package reloader

import (
	"errors"
	"testing"
)

func TestHandle(t *testing.T) {
	t.Parallel()

	newReloader := func() *Reloader {
		return New(map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"})
	}

	tests := []struct {
		name        string
		signal      any
		wantErr     bool
		wantCurrent map[string]string
	}{
		{
			name:        "full reload with a valid config replaces current",
			signal:      FullReload{Config: map[string]string{"listen_addr": ":9090", "timeout_seconds": "10"}},
			wantCurrent: map[string]string{"listen_addr": ":9090", "timeout_seconds": "10"},
		},
		{
			name:        "full reload with an invalid config rolls back, leaving current untouched",
			signal:      FullReload{Config: map[string]string{"listen_addr": ":9090"}},
			wantErr:     true,
			wantCurrent: map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"},
		},
		{
			name:        "patch merges into current and validates the merged result",
			signal:      Patch{Changes: map[string]string{"timeout_seconds": "60"}},
			wantCurrent: map[string]string{"listen_addr": ":8080", "timeout_seconds": "60"},
		},
		{
			name:        "patch that would blank a required key rolls back",
			signal:      Patch{Changes: map[string]string{"listen_addr": ""}},
			wantErr:     true,
			wantCurrent: map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"},
		},
		{
			name:        "patch with a non-numeric timeout rolls back",
			signal:      Patch{Changes: map[string]string{"timeout_seconds": "soon"}},
			wantErr:     true,
			wantCurrent: map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"},
		},
		{
			name:        "validate-only never touches current, even when valid",
			signal:      ValidateOnly{Config: map[string]string{"listen_addr": ":9999", "timeout_seconds": "5"}},
			wantCurrent: map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"},
		},
		{
			name:        "validate-only reports an error without mutating current",
			signal:      ValidateOnly{Config: map[string]string{"listen_addr": ":9999"}},
			wantErr:     true,
			wantCurrent: map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"},
		},
		{
			name:        "unsupported signal type is an error",
			signal:      "bogus",
			wantErr:     true,
			wantCurrent: map[string]string{"listen_addr": ":8080", "timeout_seconds": "30"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newReloader()
			err := r.Handle(tt.signal)
			if tt.wantErr && !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Handle err = %v, want ErrInvalidConfig", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Handle unexpected error: %v", err)
			}
			got := r.Current()
			if len(got) != len(tt.wantCurrent) {
				t.Fatalf("Current() = %v, want %v", got, tt.wantCurrent)
			}
			for k, v := range tt.wantCurrent {
				if got[k] != v {
					t.Fatalf("Current() = %v, want %v", got, tt.wantCurrent)
				}
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Handle` is correct because `r.current` is reassigned in exactly one place
per branch, and always strictly after `validate` has already returned
successfully — there is no code path where a partially-checked or
partially-merged map ever becomes the live configuration. The test table's
"rolls back" cases are the ones doing the real work here: they assert not
just that `Handle` returns an error, but that `Current()` afterward is
byte-for-byte identical to what it was before the rejected call, which is
what would catch a mutation-before-validation bug that a test only
checking the returned error would miss entirely. Validating the *merged*
result inside the `Patch` case, rather than validating `s.Changes` in
isolation, is the detail most likely to be cut under time pressure, and it
is exactly the detail that keeps a technically-valid-looking patch from
silently producing an invalid live configuration whenever the key it
depends on was never set to begin with.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [NGINX: Configuration reload without downtime](https://nginx.org/en/docs/control.html)
- [Kubernetes: ConfigMap live updates](https://kubernetes.io/docs/concepts/configuration/configmap/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-connection-pool-route-selection.md](29-connection-pool-route-selection.md) | Next: [31-vector-clock-causal-ordering.md](31-vector-clock-causal-ordering.md)
