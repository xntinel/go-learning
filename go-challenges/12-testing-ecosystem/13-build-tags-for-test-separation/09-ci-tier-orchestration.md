# Exercise 9: Wire the tiers into CI — Makefile targets and vet/build under every tag set

The tiers built across this chapter are worthless if CI never compiles them. A
tagged file that is never built under its tag rots silently: it can stop
compiling, grow a vet defect, or drift from the API it tests, and the default
green suite never notices. This closing module wires unit, integration, and e2e
into a `Makefile` whose targets run each tier and, crucially, `vet` and `build`
every tag set so tagged code cannot rot green.

Self-contained module: a small store with a hermetic unit tier plus tagged
integration and e2e skeletons, an untagged demo, and the orchestrating `Makefile`.

## What you'll build

```text
citiers/                   independent module: example.com/citiers
  go.mod
  store.go                 Store (Put/Get/Len, wrapped ErrNotFound)
  store_test.go            untagged: unit tests + a testing.Short-guarded slow test
  store_integration_test.go //go:build integration: skips without DATABASE_URL
  store_e2e_test.go        //go:build e2e: skips without BASE_URL
  Makefile                 test-unit / test-integration / test-e2e / vet-all / build-all
  cmd/
    demo/
      main.go              store round trip
```

- Files: `store.go`, `store_test.go`, `store_integration_test.go`, `store_e2e_test.go`, `cmd/demo/main.go`, `Makefile`.
- Implement: the hermetic default tier plus tagged skeletons for the two heavy tiers, and Make targets that compile and vet every tag set.
- Test: `make test-unit` runs the fast tier; `make test-integration` / `make test-e2e` compile under their tags and skip cleanly without their env; `make vet-all` vets every tagged file.
- Verify: `gofmt -l .` is empty; `go build -tags=integration ./...` and `go build -tags=e2e ./...` compile the tagged code.

Set up the module:

```bash
go mod edit -go=1.26
```

### The orchestration problem, stated precisely

The default `go test ./...`, `go vet ./...`, and `go build ./...` all operate on
the default tag set, so they never touch `//go:build integration` or
`//go:build e2e` files. That is exactly what keeps PR CI fast — and exactly what
lets a tagged file rot. Three failure modes hide in an un-exercised tier: the file
stops compiling (someone renamed a symbol it uses), it grows a vet-detectable bug
(a bad `Printf` verb, a lost `context`), or its assertions drift from the code it
tests. None surface until the day the integration stage finally runs, often long
after the change that broke it.

The fix is an operational contract encoded in the Makefile: every tag set the
project uses gets a `build` and a `vet` target, so CI compiles and vets the tagged
files on every run even when the slow *tests* run only in a later stage or only
when their environment is present. `build-all` and `vet-all` are cheap — they do
not need a database or a deployed service — yet they catch compile and vet
regressions in tagged code immediately. The heavy `test-integration` and
`test-e2e` targets then run in their own stages with `DATABASE_URL` / `BASE_URL`
set, and skip cleanly when those are absent so a developer can invoke them locally
without provisioning anything.

`store_test.go` also shows the run-time axis: `TestManyKeys` guards a larger fill
with `testing.Short()`, so `go test -short ./...` skips the slow *execution* while
still compiling the file — the orthogonal-axis point from the concepts file, now
inside the orchestration.

Create `store.go`:

```go
package citiers

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get for an absent key.
var ErrNotFound = errors.New("citiers: key not found")

// Store is a concurrency-safe key/value map.
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

Create `store_test.go`:

```go
package citiers

import (
	"errors"
	"fmt"
	"testing"
)

func TestPutGet(t *testing.T) {
	t.Parallel()
	s := NewStore()
	s.Put("k", "v")
	if v, err := s.Get("k"); err != nil || v != "v" {
		t.Fatalf("Get(k) = %q,%v; want v,nil", v, err)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	s := NewStore()
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(nope) = %v, want ErrNotFound", err)
	}
}

// TestManyKeys shows the run-time gating axis: -short skips the execution, but
// the file still compiles, so no import is removed from the graph.
func TestManyKeys(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping larger fill in -short mode")
	}
	s := NewStore()
	for i := range 10000 {
		s.Put(fmt.Sprintf("k%d", i), "v")
	}
	if s.Len() != 10000 {
		t.Fatalf("Len = %d, want 10000", s.Len())
	}
}

func ExampleStore() {
	s := NewStore()
	s.Put("answer", "42")
	v, _ := s.Get("answer")
	fmt.Println(v)
	// Output: 42
}
```

Create `store_integration_test.go`:

```go
//go:build integration

package citiers

import (
	"os"
	"testing"
)

// TestIntegrationSkeleton stands in for the database-backed tier: it compiles
// only under -tags=integration and skips unless DATABASE_URL is set, so
// build-all and vet-all check it without a live database.
func TestIntegrationSkeleton(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping integration tier")
	}
	s := NewStore()
	s.Put("k", "v")
	if _, err := s.Get("k"); err != nil {
		t.Fatalf("Get: %v", err)
	}
}
```

Create `store_e2e_test.go`:

```go
//go:build e2e

package citiers

import (
	"os"
	"testing"
)

// TestE2ESkeleton stands in for the end-to-end tier: it compiles only under
// -tags=e2e and skips unless BASE_URL is set.
func TestE2ESkeleton(t *testing.T) {
	if os.Getenv("BASE_URL") == "" {
		t.Skip("BASE_URL not set; skipping e2e tier")
	}
	_ = NewStore()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/citiers"
)

func main() {
	s := citiers.NewStore()
	s.Put("k", "v")
	v, _ := s.Get("k")
	fmt.Println("store round trip:", v)
	fmt.Println("len:", s.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
store round trip: v
len: 1
```

### The Makefile

This is the orchestration. `test-unit` is the fast gate; `test-integration` and
`test-e2e` are separate stages; `vet-all` and `build-all` exercise every tag set
so the tagged files are compiled and vetted on every CI run. Save it as `Makefile`
(Make requires tab-indented recipe lines):

```make
.PHONY: test-unit test-integration test-e2e vet-all build-all fmt-check ci

# Fast hermetic tier: gates every PR and pre-commit.
test-unit:
	go test -race -count=1 ./...

# Slow, stateful tier: its own CI stage with a live DSN.
test-integration:
	go test -tags=integration -race -count=1 ./...

# End-to-end tier: hits a deployed service; no race detector.
test-e2e:
	go test -tags=e2e -count=1 ./...

# Vet every tag set, so tagged files cannot rot green.
vet-all:
	go vet ./...
	go vet -tags=integration ./...
	go vet -tags=e2e ./...

# Build every tag set for the same reason.
build-all:
	go build ./...
	go build -tags=integration ./...
	go build -tags=e2e ./...

# Formatting gate across all files, tagged or not.
fmt-check:
	test -z "$$(gofmt -l .)"

# The composite check a CI job runs: format, vet and build all tags, run the
# fast tier. The heavy tiers run in their own stages.
ci: fmt-check vet-all build-all test-unit
```

The `$$` in `fmt-check` escapes Make's variable expansion so the shell runs
`gofmt -l .`; `test -z` fails the target when `gofmt` lists any unformatted file,
including tagged ones — the formatting gate has to see every file, not just the
default build.

## Review

The orchestration is correct when `make test-unit` runs only the hermetic tier,
`make test-integration` and `make test-e2e` compile under their tags and skip
cleanly without their environment, and `make vet-all` / `make build-all` compile
and vet the tagged files that the default `go vet ./...` and `go build ./...`
never see. The whole chapter's recurring failure — a tier that reports PASS only
because it was never compiled — is what `vet-all` and `build-all` inoculate
against: they turn "the tagged file still builds and vets" into a check that runs
on every push, cheaply, without a database or a deployed service. Keep `gofmt -l`
in the gate so formatting covers tagged files too, and remember the two axes stay
distinct — the tag decides what compiles, `-short` and env checks decide what runs.

## Resources

- [go command: Testing flags (-tags, -race, -count, -short)](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — the flags each Make target uses.
- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — why `vet`/`build` must run under each tag set.
- [testing: Short](https://pkg.go.dev/testing#Short) — the run-time gate `TestManyKeys` uses.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-ignore-tag-seed-generator.md](08-ignore-tag-seed-generator.md) | Next: [../14-parallel-tests/00-concepts.md](../14-parallel-tests/00-concepts.md)
