# Exercise 3: Prove the gate — default excludes, -tags includes

Knowing that a tag gates a file is one thing; *proving* it in a way a CI script can
assert is another. This module makes the compile-time nature observable from Go
itself: a constant whose value is selected at build time by a `!integration` /
`integration` file pair, so the demo and the tests can read which tier was
compiled. It then adds the `TestIntegrationCheckEnv` contract from the original
lesson.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
gateproof/                 independent module: example.com/gateproof
  go.mod
  store.go                 Store with wrapped ErrNotFound (the artifact under test)
  tier_default.go          //go:build !integration -> const Tier = "unit"
  tier_integration.go      //go:build integration  -> const Tier = "integration"
  gate_test.go             untagged: TestBuildTierIsUnit, TestGetReturnsNotFound
  gate_integration_test.go //go:build integration: TestIntegrationCheckEnv
  cmd/
    demo/
      main.go              prints the compiled-in Tier and a store round trip
```

- Files: `store.go`, `tier_default.go`, `tier_integration.go`, `gate_test.go`, `gate_integration_test.go`, `cmd/demo/main.go`.
- Implement: a compile-time `Tier` constant split across a `!integration`/`integration` file pair, plus the store.
- Test: `TestBuildTierIsUnit` asserts `Tier == "unit"` under the default build; `TestIntegrationCheckEnv` (tagged) skips when `INTEGRATION` is unset and asserts `Tier == "integration"` when it runs.
- Verify: `go test ./...` runs the unit tests only; `go test -tags=integration` compiles and adds the tagged test; `gofmt -l` and `go vet` are clean under both tag sets.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/13-build-tags-for-test-separation/03-run-and-verify-gates/cmd/demo
cd go-solutions/12-testing-ecosystem/13-build-tags-for-test-separation/03-run-and-verify-gates
```

### Making the compile-time gate observable

The verification problem is that "the file was excluded" leaves no runtime trace by
default — the test simply is not there. A clean way to make the gate *observable*
is a mutually-exclusive file pair that defines the same identifier under opposite
constraints:

- `tier_default.go` carries `//go:build !integration` and sets `const Tier = "unit"`.
- `tier_integration.go` carries `//go:build integration` and sets `const Tier = "integration"`.

Exactly one of the two compiles in any given build, so `Tier` is always defined
exactly once — no redeclaration, no missing symbol. Now the demo can print `Tier`
and a test can assert on it: under the default build `Tier` is `"unit"`, and only
under `-tags=integration` does it become `"integration"`. This is the same
build-time selection pattern real code uses to compile a stub adapter by default
and a real one behind a tag. It also demonstrates a `!tag` negation constraint,
which is how you write "everything *except* the integration build".

The verification you would script in CI is a set difference: capture
`go test -v ./...` and `go test -tags=integration -v ./...`, and assert the second
run's executed-test set is exactly the first plus the tagged tests. Run `go vet`
under *both* tag sets so the tagged file is vetted too — a common blind spot,
because the default `go vet ./...` never sees tagged files.

Create `store.go`:

```go
package gateproof

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound is returned (wrapped) by Get when a key is absent.
var ErrNotFound = errors.New("gateproof: key not found")

// Store is the artifact under test in both tiers.
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

Create `tier_default.go` — compiled when the `integration` tag is *absent*:

```go
//go:build !integration

package gateproof

// Tier names the build tier chosen at compile time. With no build tags the
// !integration file compiles and Tier is "unit".
const Tier = "unit"
```

Create `tier_integration.go` — compiled only under `-tags=integration`:

```go
//go:build integration

package gateproof

// Tier is "integration" when this file is compiled with -tags=integration.
const Tier = "integration"
```

### Tests: one per tier

`gate_test.go` is untagged and runs in the default build. `TestBuildTierIsUnit`
proves the default build selected the `!integration` file. `gate_integration_test.go`
carries the tag and holds `TestIntegrationCheckEnv`, which skips when `INTEGRATION`
is unset (the contract from the original lesson) and asserts `Tier == "integration"`
when it does run.

Create `gate_test.go`:

```go
package gateproof

import (
	"errors"
	"testing"
)

func TestBuildTierIsUnit(t *testing.T) {
	t.Parallel()
	if Tier != "unit" {
		t.Fatalf("Tier = %q under the default build, want %q", Tier, "unit")
	}
}

func TestGetReturnsNotFound(t *testing.T) {
	t.Parallel()
	s := NewStore()
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
}
```

Create `gate_integration_test.go`:

```go
//go:build integration

package gateproof

import (
	"os"
	"testing"
)

// TestIntegrationCheckEnv pins the "integration tests respect the env var"
// contract: it skips unless INTEGRATION=1, and when it runs it also confirms the
// integration file pair was the one compiled.
func TestIntegrationCheckEnv(t *testing.T) {
	if os.Getenv("INTEGRATION") != "1" {
		t.Skip("set INTEGRATION=1 to run the integration tier")
	}
	if Tier != "integration" {
		t.Fatalf("Tier = %q under -tags=integration, want %q", Tier, "integration")
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gateproof"
)

func main() {
	fmt.Println("build tier:", gateproof.Tier)

	s := gateproof.NewStore()
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
build tier: unit
store round trip: v
```

Build the same program with the tag and it prints `build tier: integration` —
proof that the constant was selected at *compile* time, not at run time:

```bash
go run -tags=integration ./cmd/demo
```

```text
build tier: integration
store round trip: v
```

## Review

The gate is proven when the same source produces two different `Tier` values
depending only on `-tags`, with nothing read at run time. That is the defining
property of a build constraint and the reason `go build -tags=` and
`go test -tags=` must be part of CI: a tier that is never compiled under its tag is
never vetted, never built, and never run, so bugs in it stay hidden while the
default suite is green. The `!integration` negation is worth internalizing — it is
how you express "the default everything-else build". Keep exactly one `//go:build`
line per file; a second line, or a stale `// +build` that disagrees, is an error.
Run `go vet` under both tag sets to prove the tagged file is clean too.

## Resources

- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — negation and the `-tags` flag on build/test/vet.
- [go/build: Build Constraints](https://pkg.go.dev/go/build#hdr-Build_Constraints) — the constraint grammar including `!`.
- [testing: T.Skip](https://pkg.go.dev/testing#T.Skip) — the runtime contract in `TestIntegrationCheckEnv`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-integration-tag-env-gate.md](02-integration-tag-env-gate.md) | Next: [04-testmain-postgres-fixture.md](04-testmain-postgres-fixture.md)
