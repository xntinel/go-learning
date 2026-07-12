# Exercise 6: Inject The Env Reader So Config Tests Run Pure And Parallel

`t.Setenv` makes one serial test correct, but it cannot make an env-reading test
parallel — the environment is global. The durable fix is dependency inversion:
make config parsing a pure function of an injected `getenv func(string)(string,bool)`.
Production passes `os.LookupEnv`; tests pass a map-backed closure and call
`t.Parallel()` freely.

## What you'll build

```text
envinject/                 independent module: example.com/envinject
  go.mod                   go directive supplied by the gate
  config.go                LookupFunc; LoadFrom(getenv); Load() = LoadFrom(os.LookupEnv)
  cmd/
    demo/
      main.go              runnable demo: LoadFrom over a static map
  config_test.go           fully parallel table via map-backed getenv; one serial t.Setenv test
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `LoadFrom(getenv LookupFunc) (Config, error)` touching no process state; `Load()` a thin wrapper over `os.LookupEnv`.
Test: a table-driven suite where every subtest calls `t.Parallel()` over a map-backed `getenv`; one serial `t.Setenv`-based test to justify the injection.
Verify: `go test -count=1 -race ./...`

## Invert the dependency on `os`

The parser's job is to turn a lookup function into a `Config`. It does not need to
know that the lookup function reads the process environment; it only needs
something with the shape `func(string) (string, bool)` — the exact signature of
`os.LookupEnv`. Naming that shape as a type, `LookupFunc`, and taking it as a
parameter is the whole move:

```text
type LookupFunc func(string) (string, bool)
func LoadFrom(getenv LookupFunc) (Config, error)   // pure: no os access
func Load() (Config, error) { return LoadFrom(os.LookupEnv) }
```

`Load` is the thin production wrapper that supplies the real reader; `LoadFrom` is
the pure core. Because `LoadFrom` touches no global state, a test can build a
`getenv` from a `map[string]string` and every subtest can call `t.Parallel()` —
there is nothing shared to race on. This is the senior answer to "env tests can't
be parallel": they can, once you stop reading the environment from inside the
function under test. `t.Setenv` is still correct for the small production-edge
test of `Load` itself, which must stay serial; but the bulk of the table — every
parsing rule, every failure mode — runs pure and parallel.

Keeping the map-to-`LookupFunc` adapter in the test file (not the library) is
deliberate: production never needs it, and a helper that exists only for tests
belongs beside the tests.

Create `config.go`:

```go
package envinject

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

var (
	ErrMissingHost = errors.New("missing host")
	ErrInvalidPort = errors.New("invalid port")
)

// LookupFunc is the shape of os.LookupEnv: it returns a value and whether the
// key was set. Injecting it makes config parsing pure.
type LookupFunc func(string) (string, bool)

// Config is the resolved service configuration.
type Config struct {
	Host string
	Port int
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(os.LookupEnv)
}

// LoadFrom parses configuration from an injected lookup function. It touches no
// global state, so its tests need none and can run fully parallel.
func LoadFrom(getenv LookupFunc) (Config, error) {
	host, ok := getenv("APP_HOST")
	if !ok || host == "" {
		return Config{}, fmt.Errorf("APP_HOST: %w", ErrMissingHost)
	}

	portStr, ok := getenv("APP_PORT")
	if !ok {
		return Config{}, fmt.Errorf("APP_PORT unset: %w", ErrInvalidPort)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return Config{}, fmt.Errorf("APP_PORT=%q: %w", portStr, ErrInvalidPort)
	}

	return Config{Host: host, Port: port}, nil
}
```

## The runnable demo

The demo passes a static map as the reader, so it does not depend on the ambient
environment at all — the same purity the tests exploit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/envinject"
)

func main() {
	env := map[string]string{
		"APP_HOST": "db.internal",
		"APP_PORT": "5432",
	}
	getenv := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}

	cfg, err := envinject.LoadFrom(getenv)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("host=%s port=%d\n", cfg.Host, cfg.Port)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
host=db.internal port=5432
```

## Tests

`mapGetenv` turns a `map[string]string` into a `LookupFunc`; every subtest in
`TestLoadFrom` calls `t.Parallel()` and asserts on the returned value with no
shared state, so the whole table runs concurrently under `-race`.
`TestLoadSerial` is the one env-touching test: it uses `t.Setenv` and therefore
must not call `t.Parallel` — its presence, next to the parallel table, is the
justification for the injection.

Create `config_test.go`:

```go
package envinject

import (
	"errors"
	"fmt"
	"testing"
)

// mapGetenv adapts a map to a LookupFunc for pure, parallel tests.
func mapGetenv(m map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoadFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr error
	}{
		{
			name: "valid",
			env:  map[string]string{"APP_HOST": "db.local", "APP_PORT": "5432"},
			want: Config{Host: "db.local", Port: 5432},
		},
		{
			name:    "missing host",
			env:     map[string]string{"APP_PORT": "5432"},
			wantErr: ErrMissingHost,
		},
		{
			name:    "empty host",
			env:     map[string]string{"APP_HOST": "", "APP_PORT": "5432"},
			wantErr: ErrMissingHost,
		},
		{
			name:    "port unset",
			env:     map[string]string{"APP_HOST": "db.local"},
			wantErr: ErrInvalidPort,
		},
		{
			name:    "port non-numeric",
			env:     map[string]string{"APP_HOST": "db.local", "APP_PORT": "abc"},
			wantErr: ErrInvalidPort,
		},
		{
			name:    "port negative",
			env:     map[string]string{"APP_HOST": "db.local", "APP_PORT": "-1"},
			wantErr: ErrInvalidPort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel() // safe: LoadFrom touches no global state

			got, err := LoadFrom(mapGetenv(tt.env))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("LoadFrom() err = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("LoadFrom() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadSerial(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it. This is the production edge that
	// injection lets us keep small.
	t.Setenv("APP_HOST", "db.local")
	t.Setenv("APP_PORT", "5432")

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := (Config{Host: "db.local", Port: 5432}); got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func ExampleLoadFrom() {
	getenv := mapGetenv(map[string]string{
		"APP_HOST": "cache.internal",
		"APP_PORT": "6379",
	})
	cfg, _ := LoadFrom(getenv)
	fmt.Printf("%s:%d\n", cfg.Host, cfg.Port)
	// Output: cache.internal:6379
}
```

## Review

The injection is correct when `LoadFrom` reads nothing but its `getenv` argument,
so the whole `TestLoadFrom` table runs with `t.Parallel()` and passes under
`-race` — if a subtest ever flakes, some hidden global read snuck back in. `Load`
stays a one-line wrapper passing `os.LookupEnv`, and its single serial test with
`t.Setenv` marks the exact boundary where the process environment is touched.
The lesson: `t.Setenv` fixes the symptom for one test; inverting the dependency on
`os` removes the cause for the whole suite.

## Resources

- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — the `func(string)(string,bool)` shape you inject.
- [testing.T.Parallel](https://pkg.go.dev/testing#T.Parallel) — safe once the function under test is pure.
- [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) — accepting interfaces/functions to decouple from globals.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-accumulated-validation-errors.md](05-accumulated-validation-errors.md) | Next: [07-env-var-expansion-dsn.md](07-env-var-expansion-dsn.md)
