# Exercise 2: The Build-Tagged Integration Test With A DSN Env Gate

This module adds the second tier: a `store_integration_test.go` carrying
`//go:build integration` that the default build excludes entirely, connects to a
store addressed by `DATABASE_URL`, and skips cleanly when the variable is unset.
You see the two-line gate — compile-time tag plus runtime env skip — working
together in one file.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
intgstore/                 independent module: example.com/intgstore
  go.mod
  store.go                 the same untagged Store (Put/Get/Len, wrapped ErrNotFound)
  store_test.go            untagged unit test that always runs
  store_integration_test.go   //go:build integration + DATABASE_URL env gate
  cmd/
    demo/
      main.go              exercises the store
```

- Files: `store.go`, `store_test.go`, `store_integration_test.go`, `cmd/demo/main.go`.
- Implement: the store, plus `TestIntegrationStore` behind `//go:build integration` that skips unless `DATABASE_URL` is set.
- Test: `go test ./...` excludes the integration test; `DATABASE_URL=... go test -tags=integration -v ./...` runs it.
- Verify: the tagged file compiles only under `-tags=integration`; the env var gates the run; an unset env produces `SKIP`, never `FAIL`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/02-integration-tag-env-gate/cmd/demo
cd go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/02-integration-tag-env-gate
```

### Two independent gates stacked on one test

Read the integration file top to bottom and both gating axes are visible:

- The `//go:build integration` line at the very top is the *compile-time* gate.
  With no `-tags=integration`, the `go` tool drops the file before compilation, so
  `go test ./...` never even knows `TestIntegrationStore` exists. In a real service
  this file would import a Postgres driver; the tag is what keeps that driver out of
  the default build graph. Pass `-tags=integration` and the file joins the build.
- The `os.Getenv("DATABASE_URL") == ""` check plus `t.Skip` is the *run-time* gate.
  Once the file compiles, the test still refuses to do work unless the operator
  points it at a database. This is how a developer compiles the integration tier
  locally — to catch build breakage — without a live Postgres on their laptop, and
  how the same test runs for real in a CI stage that sets `DATABASE_URL`.

The body here does a store round-trip as a stand-in; the comment marks where a
production test would `sql.Open` the DSN and run a real query. The teaching point is
the gating, and the one detail that most often breaks it: the blank line between
`//go:build integration` and `package intgstore` is mandatory. Omit it and Go reads
the constraint as the package doc comment, the tag stops gating, and the file leaks
into every build.

Create `store.go`:

```go
package intgstore

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get when a key is absent.
var ErrNotFound = errors.New("intgstore: key not found")

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

Create the untagged unit test in `store_test.go` — this one always runs:

```go
package intgstore

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
```

Now the tagged integration test. Note the three-line header shape — constraint,
blank line, package — and the env gate:

Create `store_integration_test.go`:

```go
//go:build integration

package intgstore

import (
	"os"
	"testing"
)

// TestIntegrationStore stands in for a test that talks to a real database. The
// //go:build integration tag keeps it out of the default build; the DATABASE_URL
// env var keeps it from running even when compiled, unless the operator opts in.
func TestIntegrationStore(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration tier: set DATABASE_URL (with -tags=integration) to run")
	}
	t.Logf("running integration test against %s", dsn)

	// In production: db, err := sql.Open("pgx", dsn); ... real query here.
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

	"example.com/intgstore"
)

func main() {
	s := intgstore.NewStore()
	s.Put("acct:1", "alice")
	v, _ := s.Get("acct:1")
	fmt.Printf("stored and read back: %s (len=%d)\n", v, s.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
stored and read back: alice (len=1)
```

### Proving both gates

The default run compiles only the untagged files, so the integration test is
absent:

```bash
go test -v ./...
```

```text
=== RUN   TestStoreUnit
--- PASS: TestStoreUnit (0.00s)
PASS
ok      example.com/intgstore
```

With the tag *and* the env var, the integration test compiles and runs:

```bash
DATABASE_URL=postgres://localhost/test go test -tags=integration -v ./...
```

```text
=== RUN   TestStoreUnit
--- PASS: TestStoreUnit (0.00s)
=== RUN   TestIntegrationStore
    store_integration_test.go:18: running integration test against postgres://localhost/test
--- PASS: TestIntegrationStore (0.00s)
PASS
ok      example.com/intgstore
```

With the tag but *without* the env var (`go test -tags=integration -v ./...`),
`TestIntegrationStore` compiles but reports `--- SKIP` — the compile-time gate
opened, the run-time gate stayed shut. Crucially this is a `SKIP`, not a `FAIL`: a
machine without a database must not turn the suite red.

## Review

The tag and the env var do genuinely different jobs, and conflating them is the
classic error. The tag decides whether the file is *compiled*; drop it and
`go test ./...` cannot see the test no matter what environment variables are set —
and, more importantly, the Postgres driver a real version imports never enters the
default build. The env var decides whether the compiled test *does work*; it is the
escape hatch that lets a developer compile the integration tier for build-safety
without a live database, and it is what makes the unset-env path a `SKIP` rather
than a `FAIL`. The mandatory blank line after `//go:build integration` is the detail
that breaks silently: without it the file compiles in every build and the fast tier
is no longer fast. Confirm the default `go test -v ./...` output contains only
`TestStoreUnit`, then confirm the tagged run adds `TestIntegrationStore`.

## Resources

- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — the `//go:build` syntax and `-tags`.
- [testing: T.Skip](https://pkg.go.dev/testing#T.Skip) — the runtime skip that pairs with the env gate.
- [os.Getenv](https://pkg.go.dev/os#Getenv) — reading the `DATABASE_URL` opt-in.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-store-and-unit-tests.md](01-store-and-unit-tests.md) | Next: [03-run-and-verify-both-tiers.md](03-run-and-verify-both-tiers.md)
