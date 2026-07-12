# Exercise 6: Prove the tag keeps a heavy dependency out of the default build graph

The whole economic argument for build tags is that they keep expensive
dependencies — a Postgres driver, the AWS SDK, testcontainers — out of the
default build so PR CI stays cheap. This module makes that claim *observable*: a
heavy dependency is imported only under `//go:build integration`, and both a Go
test and `go list -deps` show it is absent from the default build graph and
present in the tagged one.

Self-contained module with a local `heavydep` package standing in for a real
driver, so the whole thing builds offline while demonstrating the technique.

## What you'll build

```text
buildgraph/                independent module: example.com/buildgraph
  go.mod
  registry/registry.go     package registry: Register/List (a linked-in-driver ledger)
  heavydep/heavydep.go     package heavydep: init() registers itself (stands in for pgx)
  store.go                 plain Store; no heavy import
  probe.go                 LoadedDrivers() -> registry.List()
  store_integration.go     //go:build integration: imports heavydep (pulls it into the graph)
  graph_test.go            untagged: TestHeavyDepAbsentByDefault, TestStoreRoundTrip
  graph_integration_test.go //go:build integration: TestHeavyDepPresent
  cmd/
    demo/
      main.go              prints LoadedDrivers() (empty by default)
```

- Files: `registry/registry.go`, `heavydep/heavydep.go`, `store.go`, `probe.go`, `store_integration.go`, `graph_test.go`, `graph_integration_test.go`, `cmd/demo/main.go`.
- Implement: a `registry` package whose `List` reports which heavy packages linked their `init`; a `heavydep` package that registers itself; a store that never imports it; and an integration file that does.
- Test: `TestHeavyDepAbsentByDefault` asserts the registry is empty in the default build; the tagged `TestHeavyDepPresent` asserts it contains `heavydep`.
- Verify: `go list -deps ./cmd/demo` (heavydep absent) vs `go list -tags=integration -deps ./cmd/demo` (present); `go build ./...` links without the heavy package.

Set up the module:

```bash
go mod edit -go=1.26
```

### Making "in the build graph" observable from Go

You cannot directly ask a running binary "is package X in my build graph?" — but
you can ask a proxy question that has the same answer: "did package X's `init`
run?" A package's `init` runs if and only if that package is linked into the
binary, which happens if and only if something in the build graph imports it. So
the design is a tiny `registry` package with a package-level slice; the heavy
package's `init` appends its own name to that slice; and a `probe.go` in the main
package reads the slice back. In the default build nothing imports `heavydep`, its
`init` never runs, and `LoadedDrivers()` is empty. Under `-tags=integration`,
`store_integration.go` imports `heavydep`, its `init` runs, and the name appears.
This is the exact same compile-time-selection idea as the `Tier` constant in
Exercise 3, applied to a whole package's presence rather than a constant's value.

The reason a local `heavydep` stands in for `pgx` or the AWS SDK is only so this
module builds offline; the mechanism is identical for a real driver. In
production, `store_integration.go` would carry `import _ "github.com/jackc/pgx/v5/stdlib"`
and the same reasoning would hold: the default `go build ./...` never fetches,
compiles, or links pgx.

### The distinction -short cannot make

Contrast this with `testing.Short()`. A `-short` run *skips the execution* of a
test, but the file still compiled, so every import in that file is already in the
build graph. If your slow integration test lived in an untagged file guarded by
`if testing.Short() { t.Skip() }` and imported `pgx`, then `go test -short ./...`
would still have compiled and linked `pgx` into the test binary — you saved the
run time but not the build cost, and PR CI still pays to fetch and compile the
driver. Only a build tag removes the file, and with it the import, from the graph.
That is the mechanical rule from the concepts file made concrete: exclude a file
to remove an import, use a tag; skip execution of a compiled file, use `-short`.

Create `registry/registry.go`:

```go
package registry

import "sort"

// loaded records the names of heavy dependency packages whose init ran, i.e.
// those actually linked into the binary. Populated only from package init
// functions, which the runtime serializes, so no locking is needed.
var loaded []string

// Register records that a heavy dependency was linked in. Call it from init.
func Register(name string) {
	loaded = append(loaded, name)
}

// List returns the registered names, sorted, as a fresh slice.
func List() []string {
	out := append([]string(nil), loaded...)
	sort.Strings(out)
	return out
}
```

Create `heavydep/heavydep.go` — the stand-in for a real driver package:

```go
package heavydep

import (
	"database/sql"

	"example.com/buildgraph/registry"
)

// init runs only when this package is linked into the binary, i.e. only when
// something in the build graph imports it. That is what makes package presence
// observable through the registry.
func init() {
	registry.Register("heavydep")
}

// OpenNull stands in for a driver-backed Open. It never connects; its only role
// is to give the integration file a reason to import this heavy package.
func OpenNull() *sql.DB {
	return nil
}
```

Create `store.go` — the default artifact, importing nothing heavy:

```go
package buildgraph

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get for an absent key.
var ErrNotFound = errors.New("buildgraph: key not found")

// Store is a concurrency-safe key/value map with no heavy dependencies.
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
```

Create `probe.go`:

```go
package buildgraph

import "example.com/buildgraph/registry"

// LoadedDrivers reports which heavy dependencies were linked into this binary.
// It is empty in the default build because nothing imports heavydep; under
// -tags=integration the integration file imports it and its init runs.
func LoadedDrivers() []string {
	return registry.List()
}
```

Create `store_integration.go` — the only importer of the heavy package:

```go
//go:build integration

package buildgraph

import (
	"database/sql"

	"example.com/buildgraph/heavydep"
)

// heavyPool exists so the heavy dependency is imported (and its init runs)
// whenever the integration tier is compiled.
func heavyPool() *sql.DB {
	return heavydep.OpenNull()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/buildgraph"
)

func main() {
	fmt.Println("loaded heavy deps:", buildgraph.LoadedDrivers())

	s := buildgraph.NewStore()
	s.Put("k", "v")
	v, _ := s.Get("k")
	fmt.Println("store round trip:", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
loaded heavy deps: []
store round trip: v
```

### The tests

Create `graph_test.go`:

```go
package buildgraph

import (
	"errors"
	"fmt"
	"testing"
)

func TestHeavyDepAbsentByDefault(t *testing.T) {
	t.Parallel()
	if got := LoadedDrivers(); len(got) != 0 {
		t.Fatalf("LoadedDrivers() = %v in the default build, want empty", got)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Put("k", "v")
	if v, err := s.Get("k"); err != nil || v != "v" {
		t.Fatalf("Get(k) = %q,%v; want v,nil", v, err)
	}
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func ExampleLoadedDrivers() {
	fmt.Println(len(LoadedDrivers()))
	// Output: 0
}
```

Create `graph_integration_test.go`:

```go
//go:build integration

package buildgraph

import (
	"slices"
	"testing"
)

func TestHeavyDepPresent(t *testing.T) {
	if got := LoadedDrivers(); !slices.Contains(got, "heavydep") {
		t.Fatalf("LoadedDrivers() = %v under -tags=integration, want it to contain heavydep", got)
	}
	// Reference heavyPool so the heavy import is unambiguously part of the tier.
	_ = heavyPool()
}
```

### Seeing it with go list

`go list -deps` prints every package in the build graph reachable from the named
package. Ask it about the binary's root package (`./cmd/demo`), not `./...`:
`./...` names *every* package in the module, so it lists `heavydep` itself
regardless of tags and tells you nothing about reachability. Rooted at the demo,
the default graph excludes `heavydep`; adding the tag includes it:

```bash
go list -deps ./cmd/demo | grep buildgraph/heavydep   # prints nothing (absent)
go list -tags=integration -deps ./cmd/demo | grep buildgraph/heavydep
# example.com/buildgraph/heavydep
```

That difference is the build-graph hygiene the whole scheme buys, and it is
verifiable in a CI check rather than taken on faith.

## Review

The isolation holds when `TestHeavyDepAbsentByDefault` passes and
`go list -deps ./cmd/demo` does not list `heavydep`, while `TestHeavyDepPresent` (under
`-tags=integration`) sees it and `go list -tags=integration -deps ./cmd/demo` does list
it. The point to internalize is the contrast with `-short`: reach for
`go list -deps` after a `-short` run of a driver-importing untagged file and the
driver is still there — the runtime skip never touched the graph. If you need a
heavy import gone from the default build, only a build constraint removes it. Run
`go vet -tags=integration ./...` so the tagged file, which the default vet never
compiles, is still checked.

## Resources

- [go command: compile packages and dependencies (go list -deps, -tags)](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules) — inspecting the build graph.
- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — how a tag removes a file and its imports.
- [testing: Short](https://pkg.go.dev/testing#Short) — why a runtime skip does not shrink the build graph.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-e2e-tag-second-tier.md](05-e2e-tag-second-tier.md) | Next: [07-platform-filename-constraints.md](07-platform-filename-constraints.md)
