# Exercise 18: Configuration Loading with Priority Fallback Chain

**Nivel: Intermedio** — validacion rapida (un test corto).

A service that reads its port from a file, then an environment variable,
then a hard-coded default needs that priority order to be deterministic and
bounded: exactly one source wins, the rest are never consulted once it does,
and startup never blocks waiting on a source that will never answer. This
module builds that fallback chain as a `for` loop with an early `return` —
the whole priority policy fits in one small function.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
config/                        module example.com/config
  go.mod                       go 1.24
  config.go                   Source; Load(sources) (map, name, error); ErrNoSource
  config_test.go                first wins, fallthrough on empty/error, all fail, never over-consults
  cmd/demo/
    main.go                     file missing, env present, defaults unused
```

- Files: `config.go`, `config_test.go`, `cmd/demo/main.go`.
- Implement: `Source{Name string; Load func() (map[string]string, error)}` and `Load(sources []Source) (map[string]string, string, error)` — a `for range` loop that returns the first source whose `Load` produces a non-nil map, treating both an error and a nil map as "try the next source."
- Test: the first source wins; an empty (nil map, nil error) first source falls through to the second; an erroring first source falls through; every source failing returns `ErrNoSource`; an empty source list returns `ErrNoSource`; a source after the winner is never consulted.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/18-config-loader-fallback-chain/cmd/demo
cd go-solutions/03-control-flow/02-for-loops/18-config-loader-fallback-chain
go mod edit -go=1.24
```

### Why "empty" and "error" are both just "try the next source"

A real fallback chain has to distinguish three outcomes per source, not two:
a config was found; the source is broken (should probably be logged); and
the source simply has nothing to offer, which is not an error at all — a
config file that does not exist is the *expected* state for a container that
gets all its configuration from the environment. `Load`'s loop treats both
"broken" and "empty" identically for control-flow purposes (`continue` to the
next source) precisely because the caller only cares about one thing: did
*any* source produce a usable configuration. Collapsing the decision to "does
`cfg` come back non-nil" keeps the loop body flat — no nested `if err != nil
{ if cfg != nil { ... } }` — and the bound is visible at the top: this can run
at most `len(sources)` times, so a misconfigured deployment fails fast with
`ErrNoSource` instead of hanging.

Create `config.go`:

```go
package config

import "errors"

// ErrNoSource means every source was tried and none produced a config.
var ErrNoSource = errors.New("config: no source produced a configuration")

// Source is one place to try loading configuration from, in priority order.
// Load returns a nil map (with a nil error) to mean "this source has nothing
// to offer," which is distinct from an error (this source is broken).
type Source struct {
	Name string
	Load func() (map[string]string, error)
}

// Load tries each source in order and returns the first one that produces a
// non-nil configuration. It is a for loop with an early return: the bound is
// visible at a glance (at most len(sources) attempts, never more), and the
// moment one source succeeds the search stops -- no source after it is ever
// consulted, so startup is deterministic regardless of how many fallback
// sources are configured.
func Load(sources []Source) (map[string]string, string, error) {
	for _, s := range sources {
		if s.Load == nil {
			continue
		}
		cfg, err := s.Load()
		if err != nil {
			continue
		}
		if cfg == nil {
			continue
		}
		return cfg, s.Name, nil
	}
	return nil, "", ErrNoSource
}
```

### The runnable demo

The demo lists the three usual sources in priority order — `file`, `env`,
`defaults` — with the file source returning "nothing here," so the loop
falls through to `env` and never touches `defaults`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/config"
)

func main() {
	sources := []config.Source{
		{Name: "file", Load: func() (map[string]string, error) {
			return nil, nil // no config file present on disk
		}},
		{Name: "env", Load: func() (map[string]string, error) {
			return map[string]string{"port": "9090"}, nil
		}},
		{Name: "defaults", Load: func() (map[string]string, error) {
			return map[string]string{"port": "3000"}, nil
		}},
	}

	cfg, source, err := config.Load(sources)
	if err != nil {
		fmt.Println("load failed:", err)
		return
	}
	fmt.Printf("loaded from %q: port=%s\n", source, cfg["port"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
loaded from "env": port=9090
```

### Tests

The table covers every combination of empty/broken/successful sources in
every position, plus both all-fail and empty-list producing `ErrNoSource`.
`TestLoadNeverConsultsSourceAfterSuccess` is the sharpest check: it puts a
source after the winner that would flip a boolean if it ran, and asserts it
never does.

Create `config_test.go`:

```go
package config

import (
	"errors"
	"testing"
)

var errBroken = errors.New("source broken")

func TestLoad(t *testing.T) {
	t.Parallel()

	fileOK := Source{Name: "file", Load: func() (map[string]string, error) {
		return map[string]string{"port": "8080"}, nil
	}}
	fileMissing := Source{Name: "file", Load: func() (map[string]string, error) {
		return nil, nil
	}}
	fileBroken := Source{Name: "file", Load: func() (map[string]string, error) {
		return nil, errBroken
	}}
	envOK := Source{Name: "env", Load: func() (map[string]string, error) {
		return map[string]string{"port": "9090"}, nil
	}}
	defaultsOK := Source{Name: "defaults", Load: func() (map[string]string, error) {
		return map[string]string{"port": "3000"}, nil
	}}

	tests := []struct {
		name       string
		sources    []Source
		wantSource string
		wantErr    error
		wantPort   string
	}{
		{
			name:       "first source succeeds",
			sources:    []Source{fileOK, envOK, defaultsOK},
			wantSource: "file",
			wantPort:   "8080",
		},
		{
			name:       "first source empty falls through to second",
			sources:    []Source{fileMissing, envOK, defaultsOK},
			wantSource: "env",
			wantPort:   "9090",
		},
		{
			name:       "first source errors falls through to second",
			sources:    []Source{fileBroken, envOK, defaultsOK},
			wantSource: "env",
			wantPort:   "9090",
		},
		{
			name:       "falls all the way to defaults",
			sources:    []Source{fileMissing, fileBroken, defaultsOK},
			wantSource: "defaults",
			wantPort:   "3000",
		},
		{
			name:    "all sources fail",
			sources: []Source{fileMissing, fileBroken},
			wantErr: ErrNoSource,
		},
		{
			name:    "empty source list",
			sources: nil,
			wantErr: ErrNoSource,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, source, err := Load(tc.sources)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if source != tc.wantSource {
				t.Fatalf("source = %q, want %q", source, tc.wantSource)
			}
			if cfg["port"] != tc.wantPort {
				t.Fatalf("port = %q, want %q", cfg["port"], tc.wantPort)
			}
		})
	}
}

func TestLoadNeverConsultsSourceAfterSuccess(t *testing.T) {
	t.Parallel()

	consulted := false
	sources := []Source{
		{Name: "file", Load: func() (map[string]string, error) {
			return map[string]string{"port": "8080"}, nil
		}},
		{Name: "unreachable", Load: func() (map[string]string, error) {
			consulted = true
			return map[string]string{"port": "9999"}, nil
		}},
	}

	_, source, err := Load(sources)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if source != "file" {
		t.Fatalf("source = %q, want file", source)
	}
	if consulted {
		t.Fatal("second source must not be consulted once the first succeeds")
	}
}
```

## Review

`Load` is correct when it returns the *first* source's configuration in
priority order and never touches a later source once one succeeds, and when
every all-failure path reaches `ErrNoSource` rather than a nil map with a nil
error (which a caller could easily mistake for success). The common mistake
this design avoids is writing the chain as nested `if`/`else` per source
(`if fileCfg != nil { use it } else if envCfg != nil { ... }`), which does not
scale past two or three sources and, worse, evaluates *every* source
regardless of order because each branch's condition is computed up front.
The loop form only ever calls as many `Load` functions as it needs to. Run
`go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the `for range` form with an early `return` used here.
- [The Twelve-Factor App: Config](https://12factor.net/config) — the file/env/default priority this module's shape is built around.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-health-check-aggregator.md](17-health-check-aggregator.md) | Next: [19-checkpoint-recovery-retries.md](19-checkpoint-recovery-retries.md)
