# Exercise 3: Running And Verifying Both Tiers Like CI

An integration tier is only real if CI actually compiles and runs it. This module
exercises the default tier and the integration tier exactly as two CI stages would,
and pins the skip-without-env contract with a test that runs in the *default* build
— so a regression in the gating logic is caught by the fast gate, not discovered
when the integration stage finally runs.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
twotiers/                  independent module: example.com/twotiers
  go.mod
  store.go                 Store + ShouldRunIntegration(dsn) gate helper
  store_test.go            unit tests + TestIntegrationStoreSkipWithoutEnv
  store_integration_test.go   //go:build integration, uses ShouldRunIntegration
  cmd/
    demo/
      main.go              exercises the store
```

- Files: `store.go`, `store_test.go`, `store_integration_test.go`, `cmd/demo/main.go`.
- Implement: the store plus a testable `ShouldRunIntegration(dsn string) bool` gate helper, used by both the integration test and a default-build contract test.
- Test: `TestIntegrationStoreSkipWithoutEnv` (default build) asserts the gate is closed when `DATABASE_URL` is empty; the integration test uses the same helper.
- Verify: the four-command CI gate — `gofmt -l .`, `go test -count=1 -race ./...`, `go vet ./...`, then `go test -tags=integration -v ./...` with a DSN — and note that `go vet -tags=integration` must also run or the tagged file is never checked.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/03-run-and-verify-both-tiers/cmd/demo
cd go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/03-run-and-verify-both-tiers
```

### Make the gating logic itself testable

The skip decision — "run the integration body only when a DSN is present" — is
logic, and logic that lives only inside an `//go:build integration` file is never
exercised by the fast gate. If someone inverts the condition (skips when the DSN
*is* set), the default `go test ./...` cannot catch it, because it never compiles
the file. The fix is to lift the decision into a plain, default-build function,
`ShouldRunIntegration(dsn) bool`, and have both the integration test and a
default-build contract test call it. Now `TestIntegrationStoreSkipWithoutEnv` runs
in the sub-second gate and pins the contract: empty DSN means do not run.

`TestIntegrationStoreSkipWithoutEnv` uses `t.Setenv` to clear `DATABASE_URL` for the
duration of the test — which is why it must *not* be parallel. `t.Setenv` panics if
the test or any ancestor called `t.Parallel`, because a process-wide environment
mutation is not safe to interleave with other tests. That constraint is not a
nuisance; it is the toolchain refusing to let you write a flaky env-dependent test.

Create `store.go`:

```go
package twotiers

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get when a key is absent.
var ErrNotFound = errors.New("twotiers: key not found")

// ShouldRunIntegration reports whether the integration body should execute for a
// given DSN. It is deliberately a plain function so the fast default gate can test
// the skip contract without the integration build tag.
func ShouldRunIntegration(dsn string) bool {
	return strings.TrimSpace(dsn) != ""
}

// Store is the fast in-memory stand-in the unit tier uses.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

func (s *Store) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *Store) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return "", fmt.Errorf("get %q: %w", key, ErrNotFound)
	}
	return v, nil
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
```

Create the tagged integration test, which reuses the gate helper:

Create `store_integration_test.go`:

```go
//go:build integration

package twotiers

import (
	"os"
	"testing"
)

func TestIntegrationStore(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if !ShouldRunIntegration(dsn) {
		t.Skip("integration tier: set DATABASE_URL (with -tags=integration) to run")
	}
	t.Logf("running integration test against %s", dsn)

	s := NewStore()
	s.Put("acct:1", "alice")
	if got, err := s.Get("acct:1"); err != nil || got != "alice" {
		t.Fatalf("Get(acct:1) = %q, %v; want alice, nil", got, err)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/twotiers"
)

func main() {
	fmt.Printf("empty DSN  -> run integration? %v\n", twotiers.ShouldRunIntegration(""))
	fmt.Printf("real DSN   -> run integration? %v\n", twotiers.ShouldRunIntegration("postgres://localhost/test"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
empty DSN  -> run integration? false
real DSN   -> run integration? true
```

### Tests

The unit tests run in the default build. `TestIntegrationStoreSkipWithoutEnv` is the
add-on that pins the skip contract in the fast gate: it clears `DATABASE_URL` and
asserts the gate is closed.

Create `store_test.go`:

```go
package twotiers

import (
	"errors"
	"testing"
)

func TestStoreUnit(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Put("k", "v")

	if got, err := s.Get("k"); err != nil || got != "v" {
		t.Fatalf("Get(k) = %q, %v; want v, nil", got, err)
	}
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
}

// TestIntegrationStoreSkipWithoutEnv runs in the DEFAULT build (no tag) and pins
// the "skip when DATABASE_URL is unset" contract, so an inverted gate is caught by
// the fast tier instead of hiding until the integration stage compiles the file.
// It is not parallel: t.Setenv forbids t.Parallel.
func TestIntegrationStoreSkipWithoutEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if ShouldRunIntegration("") {
		t.Fatal("ShouldRunIntegration(\"\") = true; integration body must skip without a DSN")
	}
	if !ShouldRunIntegration("postgres://localhost/test") {
		t.Fatal("ShouldRunIntegration(dsn) = false; must run when a DSN is set")
	}
}
```

### The CI gate, command by command

A real pipeline runs the default tier and the integration tier as separate stages.
The default stage:

```bash
gofmt -l .                       # must print nothing
go vet ./...                     # default build only
go test -count=1 -race ./...     # integration file excluded, sub-second
```

The integration stage sets a DSN and adds the tag — and vets *under the tag* so the
tagged file is actually compiled and checked:

```bash
go vet -tags=integration ./...
DATABASE_URL=postgres://localhost/test go test -tags=integration -count=1 -v ./...
```

The `go vet -tags=integration` line is not optional. `go vet ./...` never compiles
the tagged file, so a vet violation or a compile error inside it is invisible to the
default stage. Only the tagged vet run sees it. Skip it and the integration file can
rot until the day it finally fails to build in CI.

## Review

The point of this module is that gating is itself code, and code that only lives
behind a tag is untested by the fast gate. Lifting the skip decision into
`ShouldRunIntegration` lets `TestIntegrationStoreSkipWithoutEnv` run in the default
build and fail loudly if someone inverts the condition. Watch two traps: do not mark
that test parallel, because `t.Setenv` panics under `t.Parallel`; and do not assume
`go test ./...` is enough for the integration tier — it never compiles the tagged
file, so the integration stage must run its own `go vet -tags=integration` and
`go test -tags=integration`. Confirm the default `-v` run shows `TestStoreUnit` and
`TestIntegrationStoreSkipWithoutEnv` but not `TestIntegrationStore`, and that adding
`-tags=integration` with a DSN pulls `TestIntegrationStore` in.

## Resources

- [testing: T.Setenv](https://pkg.go.dev/testing#T.Setenv) — the env mutation that forbids `t.Parallel`.
- [go command: Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-tags`, `-count`, `-race`, `-run`.
- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — why `go vet` and `go build` must also carry `-tags`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-integration-tag-env-gate.md](02-integration-tag-env-gate.md) | Next: [04-testmain-integration-fixture.md](04-testmain-integration-fixture.md)
