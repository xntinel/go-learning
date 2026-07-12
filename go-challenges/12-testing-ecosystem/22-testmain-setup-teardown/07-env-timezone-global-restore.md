# Exercise 7: Normalize Timezone and Process Env Once in TestMain, Then Restore

Time-formatting code that reads `time.Local` is nondeterministic across machines:
a test that passes in UTC-based CI fails on a developer's laptop in another zone.
The fix is to pin the timezone once for the whole package in `TestMain` — and to
save and restore the prior state so it does not leak into other packages. This
exercise does exactly that, and explains why `t.Setenv` cannot be used for it.

This module is fully self-contained: its own `go mod init`, formatter, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
tznorm/                        independent module: example.com/tznorm
  go.mod                       go 1.26
  stamp.go                     StampLocal: format an instant in the local zone
  cmd/
    demo/
      main.go                  runnable demo: pins UTC, stamps a few instants
  stamp_test.go                TestMain pins UTC and restores; tests assert stable strings
```

Files: `stamp.go`, `cmd/demo/main.go`, `stamp_test.go`.
Implement: `StampLocal(t time.Time) string` — format an instant in `time.Local`.
Test: a `TestMain`/`run()` that saves `TZ` and `time.Local`, sets both to UTC, restores them in a defer; tests assert deterministic formatted strings.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/07-env-timezone-global-restore/cmd/demo
cd go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/07-env-timezone-global-restore
```

### Why this belongs in TestMain, not in a test

Timezone is *process* state: `time.Local` is a package-level variable, and `TZ` is
a process environment variable. Setting it must happen once, before any test reads
the clock, and it must be undone afterward so the mutation does not bleed into
sibling packages compiled into the same test run. `TestMain` runs before any test
and on the main goroutine, so it is the right and only place to do process-wide
normalization.

Crucially, you cannot use `t.Setenv` for this. `t.Setenv` requires a `*testing.T`,
and inside `TestMain` there is no `T` — the tests have not started. And even from
within a test, `t.Setenv` is per-test (it restores after that one test) and it
*panics* if the test has called `t.Parallel()`. So a once-per-package env/zone
change belongs in `TestMain` with manual `os.Setenv`/`os.LookupEnv` and explicit
save/restore. This is one of the clearest cases where the `TestMain` lifecycle is
not a convenience but a necessity.

### Save, set, restore — and why UTC

`run()` captures the prior `TZ` (distinguishing "unset" from "empty" with the
two-value `os.LookupEnv`) and the prior `time.Local`, sets `TZ=UTC` and
`time.Local = UTC`, and restores both in a deferred closure so they run even
though `TestMain` ends in `os.Exit`. Setting `time.Local` directly is what makes
in-process formatting deterministic; setting `TZ` keeps any child process or
library that reads the env consistent with it. UTC is chosen deliberately:
`time.LoadLocation("UTC")` needs no timezone database on disk, so the harness works
on a minimal CI image where `America/New_York` might fail to load.

Create `stamp.go`:

```go
package tznorm

import "time"

// StampLocal formats t in the process local zone. Its output is deterministic
// only if time.Local is pinned; production main and the test TestMain both pin
// it to UTC so logs and assertions are stable across machines.
func StampLocal(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02 15:04:05 MST")
}
```

### The runnable demo

The demo pins UTC itself (as a service's `main` would when it normalizes logging)
so its output is deterministic, then stamps an instant given in UTC and the same
instant given in a fixed +8 zone — both print as UTC.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/tznorm"
)

func main() {
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		panic(err)
	}
	time.Local = loc

	utc := time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC)
	plus8 := time.Date(2026, 7, 2, 23, 4, 5, 0, time.FixedZone("UTC+8", 8*3600))

	fmt.Println(tznorm.StampLocal(utc))
	fmt.Println(tznorm.StampLocal(plus8))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
2026-07-02 15:04:05 UTC
2026-07-02 15:04:05 UTC
```

### Tests

`TestMain` pins UTC through `run()` and restores the prior state. Because the zone
is pinned, `StampLocal` is deterministic, so the tests assert exact strings. The
second case proves the conversion: an instant in a +8 zone stamps to its UTC wall
time.

Create `stamp_test.go`:

```go
package tznorm

import (
	"os"
	"testing"
	"time"
)

func run(m *testing.M) int {
	oldTZ, hadTZ := os.LookupEnv("TZ")
	oldLocal := time.Local

	if err := os.Setenv("TZ", "UTC"); err != nil {
		os.Stderr.WriteString("set TZ: " + err.Error() + "\n")
		return 1
	}
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		os.Stderr.WriteString("load UTC: " + err.Error() + "\n")
		return 1
	}
	time.Local = loc

	defer func() {
		time.Local = oldLocal
		if hadTZ {
			_ = os.Setenv("TZ", oldTZ)
		} else {
			_ = os.Unsetenv("TZ")
		}
	}()

	return m.Run()
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func TestStampLocalIsUTC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "utc instant",
			in:   time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC),
			want: "2026-07-02 15:04:05 UTC",
		},
		{
			name: "plus8 instant converts to utc wall time",
			in:   time.Date(2026, 7, 2, 23, 4, 5, 0, time.FixedZone("UTC+8", 8*3600)),
			want: "2026-07-02 15:04:05 UTC",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StampLocal(tc.in); got != tc.want {
				t.Fatalf("StampLocal(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLocalIsPinnedToUTC(t *testing.T) {
	t.Parallel()
	if name := time.Local.String(); name != "UTC" {
		t.Fatalf("time.Local = %q during tests, want UTC (TestMain must pin it)", name)
	}
}
```

## Review

The normalization is correct when `TestMain` pins both `TZ` and `time.Local` to
UTC before any test runs and restores the prior values in a defer inside `run()`
(so `os.Exit` does not skip the restore). `TestLocalIsPinnedToUTC` verifies the
pin took effect; `TestStampLocalIsUTC` verifies deterministic formatting and the
+8 to UTC conversion. Two things to remember: `t.Setenv` cannot express this — it
needs a `T` that does not exist in `TestMain` and panics under `t.Parallel()` — and
UTC is chosen so the harness needs no tzdata on disk. Leave the restore out and a
sibling package that formats time will see a UTC `time.Local` it never asked for.

## Resources

- [`time.LoadLocation` / `time.Local`](https://pkg.go.dev/time#LoadLocation) — loading a zone and the process-local zone variable.
- [`os.LookupEnv` / `os.Setenv` / `os.Unsetenv`](https://pkg.go.dev/os#LookupEnv) — distinguishing unset from empty when saving env.
- [`testing.T.Setenv`](https://pkg.go.dev/testing#T.Setenv) — per-test env, and why it panics under `t.Parallel()`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-golden-update-flag-wiring.md](06-golden-update-flag-wiring.md) | Next: [08-testing-os-exit-with-subprocess.md](08-testing-os-exit-with-subprocess.md)
