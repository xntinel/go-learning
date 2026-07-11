# Exercise 3: Distinguish Missing From Empty For Required Secrets And Optional Flags

`os.Getenv` collapses "unset" and "set-to-empty" into the same empty string; for
a required secret that collapse causes real incidents. This exercise builds a
startup reader where `DATABASE_URL` is required — unset and set-but-empty are
*different*, explicit misconfigurations — and `LOG_LEVEL` is optional with a
default, using `os.LookupEnv`'s `(value, ok)` to tell the cases apart.

## What you'll build

```text
startup/                   independent module: example.com/startup
  go.mod                   go directive supplied by the gate
  startup.go               Settings; Load(); ErrSecretUnset, ErrSecretEmpty
  cmd/
    demo/
      main.go              runnable demo: unset, empty, and valid DATABASE_URL
  startup_test.go          three-way table; proof Getenv cannot distinguish
```

Files: `startup.go`, `cmd/demo/main.go`, `startup_test.go`.
Implement: `Load() (Settings, error)` where `DATABASE_URL` unset -> `ErrSecretUnset`, empty -> `ErrSecretEmpty`, and `LOG_LEVEL` defaults to `"info"`.
Test: a three-way table (unset via a restore-safe helper, set-empty via `t.Setenv`, set-value); prove `os.Getenv` erases the first two.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/startup/cmd/demo
cd ~/go-exercises/startup
go mod init example.com/startup
```

## Unset and empty are different failures

Consider two deployments. In the first, someone forgot to wire `DATABASE_URL`
into the container at all: the variable is *unset*. In the second, a templating
bug rendered `DATABASE_URL=` with an empty value: the variable is *set to empty*.
These are different root causes with different fixes, and a loader that uses
`os.Getenv("DATABASE_URL") == ""` cannot tell them apart — worse, if it then
falls back to a baked-in default, it silently connects to the wrong database and
the incident surfaces as data in the wrong place, not as a boot failure.

`os.LookupEnv` returns `(value, ok)`. Branch on `ok` first: `!ok` is unset
(`ErrSecretUnset`); `ok && value == ""` is the explicit-empty misconfiguration
(`ErrSecretEmpty`). Only a genuine non-empty value proceeds. For an *optional*
setting like `LOG_LEVEL`, the opposite policy is right: unset or empty both mean
"use the default", so `info` is returned in either case.

Testing the *unset* path is itself a small lesson. `t.Setenv` can only set a
variable, never unset one, so to exercise the unset branch you must remove the
variable and restore it afterward. Restoring correctly requires the same
`(orig, ok)` snapshot from Exercise 2, because a variable that was unset must be
returned to unset, not to `""`. The `unsetEnv` helper below does exactly that,
and it is safe only because these tests are serial (no `t.Parallel`).

Create `startup.go`:

```go
package startup

import (
	"errors"
	"fmt"
	"os"
)

// ErrSecretUnset and ErrSecretEmpty are distinct on purpose: an absent secret
// and a blanked-out secret are different deployment mistakes.
var (
	ErrSecretUnset = errors.New("required secret is not set")
	ErrSecretEmpty = errors.New("required secret is set but empty")
)

// Settings is the resolved startup configuration.
type Settings struct {
	DatabaseURL string
	LogLevel    string
}

// Load reads a required secret (DATABASE_URL) and an optional flag (LOG_LEVEL).
// DATABASE_URL must be present and non-empty; the two failures are separate
// sentinels. LOG_LEVEL defaults to "info" when unset or empty.
func Load() (Settings, error) {
	dbURL, ok := os.LookupEnv("DATABASE_URL")
	if !ok {
		return Settings{}, fmt.Errorf("DATABASE_URL: %w", ErrSecretUnset)
	}
	if dbURL == "" {
		return Settings{}, fmt.Errorf("DATABASE_URL: %w", ErrSecretEmpty)
	}

	logLevel, ok := os.LookupEnv("LOG_LEVEL")
	if !ok || logLevel == "" {
		logLevel = "info"
	}

	return Settings{DatabaseURL: dbURL, LogLevel: logLevel}, nil
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/startup"
)

func main() {
	os.Unsetenv("DATABASE_URL")
	_, err := startup.Load()
	fmt.Println("unset:", errors.Is(err, startup.ErrSecretUnset))

	os.Setenv("DATABASE_URL", "")
	_, err = startup.Load()
	fmt.Println("empty:", errors.Is(err, startup.ErrSecretEmpty))

	os.Setenv("DATABASE_URL", "postgres://db.internal/app")
	os.Unsetenv("LOG_LEVEL")
	s, _ := startup.Load()
	fmt.Printf("ok: url=%s level=%s\n", s.DatabaseURL, s.LogLevel)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
unset: true
empty: true
ok: url=postgres://db.internal/app level=info
```

## Tests

The table has three states for the required secret and two for the optional flag.
The unset cases use `unsetEnv`; the set cases use `t.Setenv`. `TestGetenvCannotDistinguish`
is the motivating proof: with the variable unset and with it set-to-empty,
`os.Getenv` returns the same `""`, which is precisely why `Load` must use
`os.LookupEnv`.

Create `startup_test.go`:

```go
package startup

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// unsetEnv removes key for the duration of the test and restores its exact prior
// state (including unset) afterward. Safe only in serial tests.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	os.Unsetenv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, orig)
		} else {
			os.Unsetenv(key)
		}
	})
}

func TestLoadRequiredSecret(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		wantErr error
	}{
		{name: "unset", set: false, wantErr: ErrSecretUnset},
		{name: "empty", set: true, value: "", wantErr: ErrSecretEmpty},
		{name: "valid", set: true, value: "postgres://db/app", wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", "debug") // hold the optional field constant
			if tt.set {
				t.Setenv("DATABASE_URL", tt.value)
			} else {
				unsetEnv(t, "DATABASE_URL")
			}

			s, err := Load()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Load() err = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if s.DatabaseURL != tt.value {
				t.Fatalf("DatabaseURL = %q, want %q", s.DatabaseURL, tt.value)
			}
		})
	}
}

func TestLoadOptionalDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://db/app")

	t.Run("unset defaults to info", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://db/app")
		unsetEnv(t, "LOG_LEVEL")
		s, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if s.LogLevel != "info" {
			t.Fatalf("LogLevel = %q, want info", s.LogLevel)
		}
	})

	t.Run("explicit value wins", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://db/app")
		t.Setenv("LOG_LEVEL", "warn")
		s, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if s.LogLevel != "warn" {
			t.Fatalf("LogLevel = %q, want warn", s.LogLevel)
		}
	})
}

func TestGetenvCannotDistinguish(t *testing.T) {
	unsetEnv(t, "DATABASE_URL")
	whenUnset := os.Getenv("DATABASE_URL")

	t.Setenv("DATABASE_URL", "")
	whenEmpty := os.Getenv("DATABASE_URL")

	if whenUnset != whenEmpty {
		t.Fatalf("os.Getenv distinguished unset (%q) from empty (%q); it should not", whenUnset, whenEmpty)
	}
	if whenUnset != "" {
		t.Fatalf("os.Getenv of unset var = %q, want empty string", whenUnset)
	}
}

func ExampleLoad() {
	os.Setenv("DATABASE_URL", "postgres://db.internal/app")
	os.Unsetenv("LOG_LEVEL")

	s, _ := Load()
	fmt.Printf("%s %s\n", s.DatabaseURL, s.LogLevel)
	// Output: postgres://db.internal/app info
}
```

## Review

The loader is correct when `!ok` and `ok && ""` map to *different* sentinels and
a real value proceeds; `TestGetenvCannotDistinguish` is the standing proof that
`os.Getenv` could not implement this contract. The `unsetEnv` helper is the piece
worth internalizing: `t.Setenv` cannot express "unset", so exercising the
unset branch requires a manual `os.Unsetenv` plus a `(orig, had)`-aware restore —
which is exactly why it must stay in a serial test. For the optional field, unset
and empty deliberately share the default path, the mirror image of the required
field's policy.

## Resources

- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — `(value, ok)` separates unset from empty.
- [os.Unsetenv](https://pkg.go.dev/os#Unsetenv) — removing a variable, used to test the unset branch.
- [The Twelve-Factor App: Config](https://12factor.net/config) — why configuration belongs in the environment.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-hermetic-env-restore.md](02-hermetic-env-restore.md) | Next: [04-typed-env-parsing.md](04-typed-env-parsing.md)
