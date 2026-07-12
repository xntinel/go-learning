# Exercise 17: Configuration Loader with a Fallback Retry Chain

**Nivel: Intermedio** — validacion rapida (un test corto).

Production services routinely try configuration from several places in
priority order — an environment variable override, a mounted YAML file,
then a built-in default — and take the first one that actually works. A
parser bug in one source (a malformed YAML file, a corrupt env value that
trips an unguarded index or type assertion) must not stop the loader from
falling through to the next source; it also must not lose *why* that source
failed, filename and all, because that context is what an operator needs
when the fallback silently masked a real misconfiguration. This module
builds `LoadConfig`, which tries each source in order, isolates every
source's panic, and returns the full chain of attempts alongside whichever
config finally won. It is fully self-contained: its own module and tests.

## What you'll build

```text
configloader/                independent module: example.com/configloader
  go.mod                     go 1.24
  configloader.go             Source, Attempt, LoadConfig, loadOne
  cmd/
    demo/
      main.go                runnable demo: env panics, yaml fails, defaults win
  configloader_test.go         defaults win after two failures, first-source-wins, all fail
```

Files: `configloader.go`, `cmd/demo/main.go`, `configloader_test.go`.
Implement: `LoadConfig(sources []Source) (cfg map[string]string, attempts []Attempt, err error)` that tries each `Source` in order, isolating each one's panic with a per-source `loadOne`, and stops at the first source that returns a config with no error.
Test: three sources where the first panics and the second returns an ordinary error (with a filename embedded via `%w`), asserting the third (defaults) wins, all three attempts are recorded with correct `Panicked` flags, and `errors.Is` still reaches the second source's wrapped sentinel; a table confirming the chain stops at the first success without trying later sources; a case where every source fails, asserting a nil config and a non-nil joined error.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/17-config-loader-fallback-chain/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/17-config-loader-fallback-chain
go mod edit -go=1.24
```

### Why the panic's error context has to survive as a wrapped error, not a string

The whole point of returning `[]Attempt` instead of just the winning config
is observability: when the environment source's parser panics because
`PORT` is non-numeric, an operator debugging "why did we fall back to
defaults in production" needs to see that exact failure, not a generic
"source failed" line. `loadOne`'s recover wraps a recovered `error` value
with `%w`, never `%v`, specifically so that whatever the source's own error
already carried — a wrapped `*os.PathError` with a filename, a sentinel a
caller wants to `errors.Is` against — keeps working through `Attempt.Err`.
Flattening it to a string with `%v` would still be human-readable in a log
line, but it would make `errors.Is`/`errors.As` blind, and a caller that
wants to alert specifically on "config.yaml not found" versus "config.yaml
malformed" needs that structure preserved all the way out.

`LoadConfig` itself never inspects *why* a source failed — it just tries
the next one — which keeps the fallback policy (try env, then file, then
defaults) completely decoupled from what any individual source's failure
mode looks like. Adding a fourth source later means adding one `Source`
value; it does not mean touching the loop.

Create `configloader.go`:

```go
package configloader

import (
	"errors"
	"fmt"
)

// Source is one place configuration might come from, tried in order —
// environment variables, a YAML file, then built-in defaults.
type Source struct {
	Name string
	Load func() (map[string]string, error)
}

// Attempt records what happened when one Source was tried, successful or
// not, so a caller can see the whole fallback chain after the fact.
type Attempt struct {
	Source   string
	Err      error
	Panicked bool
}

// LoadConfig tries each source in order. The first source that returns a
// config without error or panic wins immediately. A source that panics
// while parsing (a malformed YAML file, a corrupt env value) is isolated
// with its own recover so the chain falls through to the next source
// instead of crashing the whole loader — and the panic's original error
// context (including a filename embedded by the source itself) survives
// through %w wrapping rather than being flattened into a string. Every
// attempt is recorded, in order, whether it panicked, returned an error, or
// (for the winner) succeeded.
func LoadConfig(sources []Source) (cfg map[string]string, attempts []Attempt, err error) {
	var failures []error
	for _, s := range sources {
		c, attemptErr, panicked := loadOne(s)
		attempts = append(attempts, Attempt{Source: s.Name, Err: attemptErr, Panicked: panicked})
		if attemptErr == nil {
			return c, attempts, nil
		}
		failures = append(failures, attemptErr)
	}
	return nil, attempts, fmt.Errorf("configloader: all sources failed: %w", errors.Join(failures...))
}

// loadOne is the recover boundary: exactly one source's worth of untrusted
// parsing logic.
func loadOne(s Source) (cfg map[string]string, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			if e, ok := r.(error); ok {
				err = fmt.Errorf("source %q panicked: %w", s.Name, e)
				return
			}
			err = fmt.Errorf("source %q panicked: %v", s.Name, r)
		}
	}()
	cfg, err = s.Load()
	return cfg, err, false
}
```

### The runnable demo

`env` panics on a non-numeric `PORT`, `config.yaml` fails with a wrapped
"no such file" error, and `defaults` wins.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/configloader"
)

func main() {
	sources := []configloader.Source{
		{Name: "env", Load: func() (map[string]string, error) {
			panic(fmt.Errorf("env: PORT value %q is not numeric", "abc"))
		}},
		{Name: "config.yaml", Load: func() (map[string]string, error) {
			return nil, fmt.Errorf("config.yaml: %w", errors.New("no such file"))
		}},
		{Name: "defaults", Load: func() (map[string]string, error) {
			return map[string]string{"HOST": "0.0.0.0", "PORT": "8080"}, nil
		}},
	}

	cfg, attempts, err := configloader.LoadConfig(sources)

	for _, a := range attempts {
		status := "ok"
		switch {
		case a.Panicked:
			status = "panicked"
		case a.Err != nil:
			status = "failed"
		}
		fmt.Printf("%s: %s\n", a.Source, status)
	}
	fmt.Printf("final cfg: %v\n", cfg)
	fmt.Printf("final error: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
env: panicked
config.yaml: failed
defaults: ok
final cfg: map[HOST:0.0.0.0 PORT:8080]
final error: <nil>
```

### Tests

`TestLoadConfigFallsThroughToDefaults` drives a three-source chain where the
first panics and the second fails with a wrapped sentinel, asserting
defaults wins, all three attempts are recorded correctly, and `errors.Is`
still reaches the sentinel through the wrapping. `TestLoadConfigFirstSourceWins`
proves the chain stops immediately at the first success — a second source
is never even called. `TestLoadConfigAllSourcesFail` covers total failure.

Create `configloader_test.go`:

```go
package configloader

import (
	"errors"
	"fmt"
	"testing"
)

var errNotFound = errors.New("no such file")

func TestLoadConfigFallsThroughToDefaults(t *testing.T) {
	sources := []Source{
		{Name: "env", Load: func() (map[string]string, error) {
			raw := map[string]string{"PORT": "abc"}
			port := raw["PORT"]
			var digits []byte
			for _, c := range port {
				if c < '0' || c > '9' {
					panic(fmt.Errorf("env: PORT value %q is not numeric", port))
				}
				digits = append(digits, byte(c))
			}
			return nil, nil
		}},
		{Name: "config.yaml", Load: func() (map[string]string, error) {
			return nil, fmt.Errorf("config.yaml: %w", errNotFound)
		}},
		{Name: "defaults", Load: func() (map[string]string, error) {
			return map[string]string{"HOST": "0.0.0.0", "PORT": "8080"}, nil
		}},
	}

	cfg, attempts, err := LoadConfig(sources)
	if err != nil {
		t.Fatalf("err = %v, want nil (defaults must win)", err)
	}
	if cfg["HOST"] != "0.0.0.0" || cfg["PORT"] != "8080" {
		t.Fatalf("cfg = %+v, want defaults", cfg)
	}
	if len(attempts) != 3 {
		t.Fatalf("len(attempts) = %d, want 3", len(attempts))
	}
	if !attempts[0].Panicked {
		t.Fatal("env attempt should be marked Panicked")
	}
	if attempts[1].Panicked {
		t.Fatal("config.yaml attempt returned an ordinary error, not a panic")
	}
	if attempts[2].Err != nil {
		t.Fatalf("defaults attempt should have no error, got %v", attempts[2].Err)
	}
	if !errors.Is(attempts[1].Err, errNotFound) {
		t.Fatalf("config.yaml error %v does not wrap errNotFound", attempts[1].Err)
	}
}

func TestLoadConfigFirstSourceWins(t *testing.T) {
	calls := 0
	sources := []Source{
		{Name: "env", Load: func() (map[string]string, error) {
			calls++
			return map[string]string{"HOST": "env-host"}, nil
		}},
		{Name: "config.yaml", Load: func() (map[string]string, error) {
			calls++
			return map[string]string{"HOST": "yaml-host"}, nil
		}},
	}

	cfg, attempts, err := LoadConfig(sources)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if cfg["HOST"] != "env-host" {
		t.Fatalf("cfg[HOST] = %q, want env-host (first source should win)", cfg["HOST"])
	}
	if len(attempts) != 1 {
		t.Fatalf("len(attempts) = %d, want 1 (chain should stop at the first success)", len(attempts))
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (config.yaml should never have been tried)", calls)
	}
}

func TestLoadConfigAllSourcesFail(t *testing.T) {
	sources := []Source{
		{Name: "env", Load: func() (map[string]string, error) {
			return nil, errors.New("no env vars set")
		}},
		{Name: "config.yaml", Load: func() (map[string]string, error) {
			panic("unexpected EOF")
		}},
	}

	cfg, attempts, err := LoadConfig(sources)
	if cfg != nil {
		t.Fatalf("cfg = %+v, want nil", cfg)
	}
	if err == nil {
		t.Fatal("err = nil, want a joined failure error")
	}
	if len(attempts) != 2 {
		t.Fatalf("len(attempts) = %d, want 2", len(attempts))
	}
	if !attempts[1].Panicked {
		t.Fatal("config.yaml attempt should be marked Panicked")
	}
}
```

## Review

`LoadConfig` is correct when a panic in any one source never stops the chain
from reaching a later, working source, and when the recorded `[]Attempt`
tells the truth about every source that was tried — panicked, failed, or
won. The recover lives in `loadOne`, one source wide, the same discipline
this chapter applies to one job, one batch item, one plugin call: wrapping
the whole loop in a single recover would let the first panic abort every
source after it, defeating the entire point of a fallback chain. Wrapping a
recovered error with `%w` rather than formatting it with `%v` is what keeps
`errors.Is`/`errors.As` working for a caller further up the stack — losing
that structure to a flattened string is the single easiest way to make a
fallback chain's failures undiagnosable in production.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-source recover pattern this reuses.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating every source's failure into one error whose Is/As walks each.
- [fmt.Errorf and %w](https://pkg.go.dev/fmt#Errorf) — preserving a panicking source's original error context.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-rate-limiter-panic-containment.md](16-rate-limiter-panic-containment.md) | Next: [18-message-batch-consumer.md](18-message-batch-consumer.md)
