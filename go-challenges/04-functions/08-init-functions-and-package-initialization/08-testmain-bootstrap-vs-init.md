# Exercise 8: Test Bootstrap with TestMain Instead of Leaking Setup into init

Shared test setup that needs ordering, teardown, or the run's exit code cannot live
in a test-file `init()` — `init` has no teardown hook, cannot see the result, and
cannot control ordering. `func TestMain(m *testing.M)` is the correct place. This
exercise builds a small file-backed store whose suite sets up a seeded temp
directory in `TestMain`, runs, and tears it down.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
seedstore/                   independent module: example.com/seedstore
  go.mod                     module example.com/seedstore
  seedstore.go               List/Read/Add/Remove over a directory
  cmd/demo/main.go           seeds a temp dir and lists it
  seedstore_test.go          TestMain sets up + tears down a seeded fixture; per-test t.Cleanup
```

Files: `seedstore.go`, `cmd/demo/main.go`, `seedstore_test.go`.
Implement: `List`, `Read`, `Add`, `Remove` over a directory of seed files.
Test: `TestMain` builds a seeded temp dir, runs `m.Run()`, removes the dir, and exits with the run's code; tests read the fixture; a mutating test uses `t.Cleanup` so its change does not leak to the next test.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/08-testmain-bootstrap-vs-init/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/08-testmain-bootstrap-vs-init
```

### Why TestMain and not a test init()

A test-file `init()` runs before any test, exactly once, with no hook to run after
the suite and no access to the exit code. That makes it structurally unable to own
setup that must be torn down (a temp directory, a seeded database, a listening
socket): whatever it creates leaks for the life of the test process, and if setup
fails it can only panic, with no chance to clean up partial state. `TestMain` is the
answer. When a test file defines `func TestMain(m *testing.M)`, the test binary
calls it instead of running tests directly; you do setup, call `m.Run()` (which runs
all the tests and returns an exit code), do teardown, and finally call
`os.Exit(code)`. Setup happens before any test, teardown happens after all of them,
and the exit code is preserved — none of which a test `init()` can do.

The pattern in this exercise: `TestMain` creates a temp directory with
`os.MkdirTemp`, seeds it with two files, stashes the path in a package-level
`testDir`, runs the suite, then `os.RemoveAll(testDir)` and `os.Exit(code)`. Note
the discipline around `os.Exit`: because `os.Exit` skips deferred functions, the
teardown must happen *before* it, not in a `defer`. That is the one sharp edge of
`TestMain`.

Per-test teardown is a different tool: `t.Cleanup`. A test that mutates the shared
fixture (adds a scratch file) registers a `t.Cleanup` to undo the change, so the
mutation is reverted when that test returns and does not leak into the next test.
`TestMutationIsCleanedUp` writes a scratch file and cleans it;
`TestFixtureStillSeeded`, which runs after, confirms the fixture is back to its
seeded state. Because
these tests share one mutable directory, they run sequentially (no `t.Parallel`) —
parallel tests over a shared mutable fixture would race, which is itself a reason
some suites keep the fixture read-only and clone per test.

Create `seedstore.go`:

```go
// seedstore.go
package seedstore

import (
	"os"
	"path/filepath"
	"sort"
)

// List returns the sorted names of the regular files in dir.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Read returns the contents of dir/name.
func Read(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	return string(b), err
}

// Add writes content to dir/name, creating or overwriting it.
func Add(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

// Remove deletes dir/name.
func Remove(dir, name string) error {
	return os.Remove(filepath.Join(dir, name))
}
```

### The runnable demo

The demo does its own setup and teardown with a real temp dir, so you can run it
outside the test harness.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"os"

	"example.com/seedstore"
)

func main() {
	dir, err := os.MkdirTemp("", "seedstore-demo-*")
	if err != nil {
		fmt.Println("mkdir:", err)
		return
	}
	defer os.RemoveAll(dir)

	for name, content := range map[string]string{
		"alpha.txt": "first",
		"beta.txt":  "second",
	} {
		if err := seedstore.Add(dir, name, content); err != nil {
			fmt.Println("add:", err)
			return
		}
	}

	names, _ := seedstore.List(dir)
	fmt.Println("seeded:", names)

	body, _ := seedstore.Read(dir, "alpha.txt")
	fmt.Println("alpha.txt:", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
seeded: [alpha.txt beta.txt]
alpha.txt: first
```

### Tests

`TestMain` owns the shared fixture. The mutation test proves per-test cleanup keeps
the fixture stable across tests.

Create `seedstore_test.go`:

```go
// seedstore_test.go
package seedstore

import (
	"os"
	"slices"
	"testing"
)

// testDir is the shared fixture directory, created by TestMain. A test-file
// init() could create it but could never remove it or see the run's exit code.
var testDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "seedstore-test-*")
	if err != nil {
		panic("setup: " + err.Error())
	}
	testDir = dir

	for name, content := range map[string]string{
		"alpha.txt": "a",
		"beta.txt":  "b",
	} {
		if err := Add(dir, name, content); err != nil {
			os.RemoveAll(dir)
			panic("seed: " + err.Error())
		}
	}

	code := m.Run() // runs every test against the seeded fixture

	// Teardown must run before os.Exit, which skips deferred calls.
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestFixtureSeeded(t *testing.T) {
	names, err := List(testDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha.txt", "beta.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("List = %v, want %v", names, want)
	}
}

func TestReadSeededFile(t *testing.T) {
	got, err := Read(testDir, "beta.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" {
		t.Fatalf("Read(beta.txt) = %q, want %q", got, "b")
	}
}

func TestMutationIsCleanedUp(t *testing.T) {
	if err := Add(testDir, "scratch.txt", "temp"); err != nil {
		t.Fatal(err)
	}
	// Revert the mutation when this test returns, so it does not leak.
	t.Cleanup(func() {
		if err := Remove(testDir, "scratch.txt"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	names, _ := List(testDir)
	if !slices.Contains(names, "scratch.txt") {
		t.Fatal("scratch.txt not present during its own test")
	}
}

// TestFixtureStillSeeded runs after TestMutationIsCleanedUp and proves the
// per-test cleanup reverted the mutation: the fixture is back to its seed set.
func TestFixtureStillSeeded(t *testing.T) {
	names, err := List(testDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha.txt", "beta.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("fixture leaked mutation: List = %v, want %v", names, want)
	}
}
```

## Review

The suite is correct when the shared fixture is created before any test, removed
after all of them, and the process exits with the run's own code — the three things
a test `init()` cannot deliver. `TestMain` provides them: setup before `m.Run()`,
teardown after, and `os.Exit(code)` with the returned status. The sharp edge to
respect is that `os.Exit` skips deferred functions, so teardown is called
explicitly before it, never via `defer`.

Per-test isolation is the other half. `TestMutationIsCleanedUp` adds a file and
registers a `t.Cleanup` to remove it; `TestFixtureStillSeeded`, running afterward,
confirms the mutation did not leak. The mistake to avoid is putting this setup in a
test `init()` and then discovering there is nowhere to remove the temp dir and no
way to fail the run cleanly — or marking these tests `t.Parallel()` while they share
one mutable directory, which reintroduces the race the sequential ordering avoids.

## Resources

- [testing: TestMain / Main](https://pkg.go.dev/testing#hdr-Main) — the bootstrap hook with `m.Run()` and `os.Exit`.
- [testing.T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — per-test teardown that runs when the test returns.
- [os.MkdirTemp](https://pkg.go.dev/os#MkdirTemp) — creating the isolated fixture directory.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-refactor-init-env-to-explicit-constructor.md](09-refactor-init-env-to-explicit-constructor.md)
