# Exercise 2: The run() int Wrapper — Make Teardown Actually Run Despite os.Exit

The most common `TestMain` bug in real code leaks resources in CI: teardown
written as a `defer` directly in `TestMain` never executes, because `os.Exit`
skips deferred functions. This exercise builds the fix — the `func run(m *testing.M) int`
wrapper — and, crucially, tests in-process that the deferred teardown actually
fires.

This module is fully self-contained: its own `go mod init`, fixture helper, demo,
and tests. Nothing here imports any other exercise.

## What you'll build

```text
runwrapper/                    independent module: example.com/runwrapper
  go.mod                       go 1.26
  fixture.go                   withFixture: temp dir + guaranteed defer teardown
  cmd/
    demo/
      main.go                  runnable demo: create a fixture, use it, watch it removed
  fixture_test.go              TestMain uses run(); tests prove teardown fired
```

Files: `fixture.go`, `cmd/demo/main.go`, `fixture_test.go`.
Implement: `withFixture(body func(dir string) int) (code int, dir string, tornDown bool)` that creates a temp fixture dir, seeds a file, runs `body`, and tears the dir down via `defer`.
Test: a one-line `TestMain` of `os.Exit(run(m))`; a test that reads the seeded fixture during the run; and a test that calls `withFixture` directly and asserts the dir was removed and the teardown flag set.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/02-run-wrapper-for-deferred-teardown/cmd/demo
cd go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/02-run-wrapper-for-deferred-teardown
```

### The bug, stated precisely

Consider the broken shape:

```go
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "fixtures")
	defer os.RemoveAll(dir) // never runs
	os.Exit(m.Run())
}
```

`os.Exit` terminates the process without unwinding the stack, so the `defer`
above is skipped. In a suite that starts a Docker container, opens a database, or
writes gigabytes of temp fixtures, this leaks on every single CI run until the
runner's disk fills or the container quota is hit. The fix is structural: no
teardown-critical `defer` may live in a function that itself calls `os.Exit`.

### The fix, and how to test it

Move everything into a helper that *returns* an `int`. Because the helper returns
normally, its defers unwind before control goes back to `TestMain`, and only then
does `TestMain` call `os.Exit`. `TestMain` shrinks to one line.

The teaching problem is that the effect of the fix — "the defer ran" — is normally
only observable *outside* the process (the temp dir is gone). To make it testable
in-process, we factor the deferred-teardown logic into `withFixture`, a function
that creates the temp dir, runs a `body`, and guarantees teardown via a named
return plus a `defer`. A named return matters here: a deferred closure can mutate
named results *after* the `return` statement has run, so `withFixture` can report
`tornDown = true` from inside its own defer, and a normal test can call
`withFixture` and observe both that the directory was removed and that the flag
flipped. That is a direct, in-process proof that the deferred teardown fires —
exactly the thing the broken `TestMain` fails to do.

Create `fixture.go`:

```go
package runwrapper

import (
	"fmt"
	"os"
	"path/filepath"
)

// SeedName is the file withFixture writes into the fixture dir.
const SeedName = "seed.txt"

// withFixture creates a temp fixture directory, seeds a file into it, runs body
// with the directory path, and guarantees teardown via a deferred RemoveAll.
// It reports the exit code from body, the directory it used, and whether the
// deferred teardown ran. Because tornDown is a named result, the deferred
// closure can set it after body returns and the caller still observes true.
func withFixture(body func(dir string) int) (code int, dir string, tornDown bool) {
	d, err := os.MkdirTemp("", "fixture-*")
	if err != nil {
		return 1, "", false
	}
	dir = d
	defer func() {
		os.RemoveAll(d)
		tornDown = true
	}()

	if err := os.WriteFile(filepath.Join(d, SeedName), []byte("seed"), 0o644); err != nil {
		return 1, dir, tornDown
	}

	code = body(d)
	return code, dir, tornDown
}

// DemoReport exercises the fixture lifecycle and returns a human-readable report
// showing the seed file existed during the run and the dir was torn down after.
// It exists so package main can observe the behavior without touching internals.
func DemoReport() string {
	var seenSeed bool
	_, dir, tornDown := withFixture(func(d string) int {
		_, err := os.Stat(filepath.Join(d, SeedName))
		seenSeed = err == nil
		return 0
	})
	_, statErr := os.Stat(dir)
	return fmt.Sprintf("seed present during run: %v\nteardown ran: %v\ndir removed after: %v",
		seenSeed, tornDown, os.IsNotExist(statErr))
}
```

### The runnable demo

The demo shows the whole arc: `withFixture` creates a dir, the body confirms the
seed file exists, and after `withFixture` returns the dir is gone.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/runwrapper"
)

func main() {
	fmt.Println(runwrapper.DemoReport())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
seed present during run: true
teardown ran: true
dir removed after: true
```

### Tests

`TestMain` is the one-line wrapper. `run` uses `withFixture`: the `body` it passes
publishes the fixture dir into a package var and returns `m.Run()`, so during the
whole suite the fixture exists, and after `m.Run()` the deferred teardown removes
it. `TestFixtureSeededDuringRun` reads that shared fixture during the run.
`TestWithFixtureTearsDown` is the direct proof: it calls `withFixture` with its own
body and asserts the returned directory no longer exists and `tornDown` is true.

Create `fixture_test.go`:

```go
package runwrapper

import (
	"os"
	"path/filepath"
	"testing"
)

// fixtureDir is published by run() during the suite.
var fixtureDir string

func run(m *testing.M) int {
	code, dir, _ := withFixture(func(d string) int {
		fixtureDir = d
		return m.Run()
	})
	// After this point the fixture dir has been torn down by withFixture's defer.
	_ = dir
	return code
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func TestFixtureSeededDuringRun(t *testing.T) {
	t.Parallel()
	if fixtureDir == "" {
		t.Fatal("run() did not publish a fixture dir")
	}
	data, err := os.ReadFile(filepath.Join(fixtureDir, SeedName))
	if err != nil {
		t.Fatalf("reading seed during run: %v", err)
	}
	if string(data) != "seed" {
		t.Fatalf("seed contents = %q, want %q", data, "seed")
	}
}

func TestWithFixtureTearsDown(t *testing.T) {
	t.Parallel()

	var innerExists bool
	_, dir, tornDown := withFixture(func(d string) int {
		_, err := os.Stat(d) // dir exists while body runs
		innerExists = err == nil
		return 0
	})

	if !innerExists {
		t.Error("fixture dir did not exist during body")
	}
	if !tornDown {
		t.Error("deferred teardown did not run (tornDown == false)")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("fixture dir still present after teardown: Stat err = %v", err)
	}
}
```

## Review

The wrapper is correct when `TestMain` is a single `os.Exit(run(m))` and every
teardown-critical `defer` lives in `run`/`withFixture`, never in `TestMain`. The
proof is `TestWithFixtureTearsDown`: it observes the fixture dir existing during
the body and gone afterward, and reads `tornDown == true`, which only happens
because the deferred closure mutates a named return value after the body returns.
The demo reinforces the same arc end-to-end. The mistake this exercise exists to
kill is `defer os.RemoveAll(dir)` sitting directly above `os.Exit(m.Run())` in
`TestMain`: it compiles, it reads as correct, and it silently never runs. Move it
into the wrapper.

## Resources

- [`os.Exit`](https://pkg.go.dev/os#Exit) — "The program terminates immediately; deferred functions are not run."
- [`testing`: Main](https://pkg.go.dev/testing#hdr-Main) — where the `TestMain`/`os.Exit(m.Run())` contract is specified.
- [`os.MkdirTemp` / `os.RemoveAll`](https://pkg.go.dev/os#MkdirTemp) — creating and removing the fixture directory.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-silence-default-logger.md](01-silence-default-logger.md) | Next: [03-shared-postgres-pool-and-migrations.md](03-shared-postgres-pool-and-migrations.md)
