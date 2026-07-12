# Exercise 2: Gate an integration test behind //go:build integration

This module adds the second tier: a tagged integration test that the default build
excludes entirely and that only compiles under `-tags=integration`. It layers a
runtime environment gate on top of the compile-time tag so you can see the two
independent axes side by side in one file.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
storegate/                 independent module: example.com/storegate
  go.mod
  store.go                 the same untagged Store (Put/Get/Len, wrapped ErrNotFound)
  store_test.go            untagged unit test that always runs
  store_integration_test.go   //go:build integration + INTEGRATION=1 env gate
  cmd/
    demo/
      main.go              exercises the store
```

- Files: `store.go`, `store_test.go`, `store_integration_test.go`, `cmd/demo/main.go`.
- Implement: the store, plus `TestStoreIntegration` behind `//go:build integration` that skips unless `INTEGRATION=1`.
- Test: `go test ./...` excludes the integration test; `INTEGRATION=1 go test -tags=integration -v ./...` runs it.
- Verify: the tagged file compiles only under `-tags=integration`; the env var gates the run.

### Two independent gates on one test

Read the integration file top to bottom and you see both gating axes stacked:

- The `//go:build integration` line at the very top is the *compile-time* gate.
  With no `-tags=integration`, the `go` tool drops the file before compilation;
  `go test ./...` never even knows `TestStoreIntegration` exists. Pass
  `-tags=integration` and the file joins the build.
- The `os.Getenv("INTEGRATION") != "1"` check plus `t.Skip` is the *run-time*
  gate. Once the file compiles, the test still refuses to do real work unless the
  operator opts in with the environment variable. This is how a developer can
  compile the integration tier locally (to catch build breakage) without needing
  a live database on their laptop.

In production this file would `sql.Open` a real Postgres, run a query, and assert
on the result. Here the body is a stand-in that puts and gets against the store,
because the teaching point is the *gating*, not the query. The blank line between
`//go:build integration` and `package storegate` is mandatory: omit it and Go
reads the constraint as the package doc comment, the tag stops gating, and the
file leaks into every build.

Create `store.go`:

```go
package storegate

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get when a key is absent.
var ErrNotFound = errors.New("storegate: key not found")

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
package storegate

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

package storegate

import (
	"os"
	"testing"
)

// TestStoreIntegration stands in for a test that talks to a real database. The
// //go:build integration tag keeps it out of the default build; the INTEGRATION
// env var keeps it from running even when compiled, unless the operator opts in.
func TestStoreIntegration(t *testing.T) {
	if os.Getenv("INTEGRATION") != "1" {
		t.Skip("integration tier: set INTEGRATION=1 (with -tags=integration) to run")
	}
	t.Log("integration tier running against the real dependency")

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

	"example.com/storegate"
)

func main() {
	s := storegate.NewStore()
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

Two commands make the axes visible. The default run compiles only the untagged
files, so the integration test is absent:

```bash
go test -v ./...
```

```text
=== RUN   TestStoreUnit
--- PASS: TestStoreUnit (0.00s)
PASS
ok      example.com/storegate
```

With the tag *and* the env var, the integration test compiles and runs:

```bash
INTEGRATION=1 go test -tags=integration -v ./...
```

```text
=== RUN   TestStoreUnit
--- PASS: TestStoreUnit (0.00s)
=== RUN   TestStoreIntegration
    store_integration_test.go:15: integration tier running against the real dependency
--- PASS: TestStoreIntegration (0.00s)
PASS
ok      example.com/storegate
```

With the tag but *without* the env var (`go test -tags=integration -v ./...`),
`TestStoreIntegration` compiles but reports `--- SKIP` — the compile-time gate
opened, the run-time gate stayed shut.

## Review

The tag and the env var are doing genuinely different jobs, and conflating them is
the classic error. The tag decides whether the file is *compiled*; drop it and
`go test ./...` cannot see the test no matter what environment variables you set.
The env var decides whether the compiled test *does work*; it is the escape hatch
that lets a developer compile the integration tier for build-safety without a live
database. The mandatory blank line after `//go:build integration` is the detail
that most often breaks silently: without it the file compiles in *every* build and
your fast tier is no longer fast. Confirm the default `go test -v ./...` output
contains only `TestStoreUnit`, then confirm the tagged run adds
`TestStoreIntegration`.

## Resources

- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — the `//go:build` syntax and `-tags`.
- [testing: T.Skip](https://pkg.go.dev/testing#T.Skip) — the runtime skip that pairs with the env gate.
- [os.Getenv](https://pkg.go.dev/os#Getenv) — reading the `INTEGRATION` opt-in.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-inmemory-store-unit-tests.md](01-inmemory-store-unit-tests.md) | Next: [03-run-and-verify-gates.md](03-run-and-verify-gates.md)
