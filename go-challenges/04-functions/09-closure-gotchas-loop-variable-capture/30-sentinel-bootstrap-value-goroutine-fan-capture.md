# Exercise 30: Bootstrap Sentinel Registration: Loop Variable Captured by Goroutines

**Nivel: Intermedio** — validacion rapida (un test corto).

A bootstrap sequencer launches one goroutine per sentinel marker to run
setup logic for it. The trap is reusing a pre-declared variable as the range
target — `for _, s = range sentinels` instead of `for _, s := range
sentinels` — so every goroutine's closure shares that ONE variable no matter
what Go version compiles it. A barrier makes the worst case deterministic:
every goroutine ends up running bootstrap with the same, final, sentinel.

## What you'll build

```text
bootstrap/                   independent module: example.com/bootstrap
  go.mod                     go 1.24
  bootstrap.go                Sentinel, BuggyRegisterAll, RegisterAll
  cmd/
    demo/
      main.go                runnable demo: register 3 sentinels, print names seen
  bootstrap_test.go           each goroutine keeps its own sentinel vs all see the last
```

- Files: `bootstrap.go`, `cmd/demo/main.go`, `bootstrap_test.go`.
- Implement: `BuggyRegisterAll` reusing a pre-declared `s` via `for _, s = range sentinels`; `RegisterAll` declaring `s` with `:=` and passing it as a goroutine parameter.
- Test: register several sentinels and assert `RegisterAll`'s goroutines each ran with their own sentinel while `BuggyRegisterAll`'s all ran with the last one; `-race` clean.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/30-sentinel-bootstrap-value-goroutine-fan-capture/cmd/demo
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/30-sentinel-bootstrap-value-goroutine-fan-capture
go mod edit -go=1.24
```

### Why `for _, s = range` defeats the Go 1.22 fix

Go 1.22 made `for _, s := range sentinels` give each iteration its OWN `s`.
`BuggyRegisterAll` instead declares `var s Sentinel` before the loop and
writes `for _, s = range sentinels` — using `=` to assign into the
pre-existing variable instead of `:=` to declare a fresh one. That is a
single shared variable for the whole function, on any Go version, because
the per-iteration behavior is a property of variables the `for` statement
itself introduces, not of the loop as a whole. Every goroutine's closure
reads that one `s`; a barrier (`ready`) makes the worst case deterministic by
holding every goroutine back until the dispatch loop has finished advancing
`s` to the LAST sentinel, so every goroutine reads that same final value
instead of whichever sentinel it was meant to bootstrap.

`RegisterAll` passes `s` as a goroutine parameter instead, which is
unambiguous on every Go version: parameters are copied at the `go` statement,
so each goroutine gets its own value regardless of how the loop variable
itself behaves.

Create `bootstrap.go`:

```go
package bootstrap

import "sync"

// Sentinel is one bootstrap marker a handler is registered against.
type Sentinel struct {
	Name  string
	Value int
}

// BuggyRegisterAll launches one goroutine per sentinel to run bootstrap
// logic for it, but the range variable `s` is reused via `for _, s = range
// sentinels` after being declared ONCE outside the loop, instead of letting
// each iteration bind its own copy (Go 1.22's per-iteration loop variables
// only apply to a fresh variable declared BY the for statement, such as
// `for _, s := range sentinels`; a variable declared before the loop and
// merely assigned inside it is still one shared variable). A barrier makes
// the worst case deterministic: every goroutine waits on `ready` until the
// dispatch loop has finished advancing s to the LAST sentinel, then they all
// read it -- so every goroutine ends up running bootstrap with the SAME,
// final, sentinel.
func BuggyRegisterAll(sentinels []Sentinel, run func(Sentinel)) []Sentinel {
	var s Sentinel // BUG: declared outside the loop, shared by every goroutine
	var wg sync.WaitGroup
	var mu sync.Mutex
	var seen []Sentinel
	ready := make(chan struct{})
	for _, s = range sentinels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready // wait for dispatch to finish advancing s
			mu.Lock()
			seen = append(seen, s)
			mu.Unlock()
			run(s)
		}()
	}
	close(ready) // only now do goroutines read s; every read sees the final value
	wg.Wait()
	return seen
}

// RegisterAll launches one goroutine per sentinel, each receiving its own
// sentinel as a parameter, so every goroutine runs bootstrap with the
// sentinel it was actually registered for.
func RegisterAll(sentinels []Sentinel, run func(Sentinel)) []Sentinel {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var seen []Sentinel
	for _, s := range sentinels {
		wg.Add(1)
		go func(s Sentinel) {
			defer wg.Done()
			mu.Lock()
			seen = append(seen, s)
			mu.Unlock()
			run(s)
		}(s)
	}
	wg.Wait()
	return seen
}
```

### The runnable demo

The demo registers three sentinels with both variants and prints the sorted
names each variant's goroutines actually ran with.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/bootstrap"
)

func sortedNames(seen []bootstrap.Sentinel) []string {
	names := make([]string, len(seen))
	for i, s := range seen {
		names[i] = s.Name
	}
	sort.Strings(names)
	return names
}

func main() {
	sentinels := []bootstrap.Sentinel{
		{Name: "cache", Value: 1},
		{Name: "queue", Value: 2},
		{Name: "search", Value: 3},
	}

	buggySeen := bootstrap.BuggyRegisterAll(sentinels, func(bootstrap.Sentinel) {})
	fmt.Println("buggy  sentinels seen:", sortedNames(buggySeen))

	fixedSeen := bootstrap.RegisterAll(sentinels, func(bootstrap.Sentinel) {})
	fmt.Println("fixed  sentinels seen:", sortedNames(fixedSeen))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  sentinels seen: [search search search]
fixed  sentinels seen: [cache queue search]
```

### Tests

`TestRegisterAllEachGoroutineKeepsOwnSentinel` sorts and compares the seen
sentinels against the input. `TestBuggyRegisterAllEveryGoroutineSeesLast
Sentinel` asserts every goroutine ran with the last sentinel.
`TestRegisterAllSingleSentinelEdgeCase` covers the boundary where there is
no other sentinel to collapse into.

Create `bootstrap_test.go`:

```go
package bootstrap

import (
	"sort"
	"testing"
)

func testSentinels() []Sentinel {
	return []Sentinel{
		{Name: "cache", Value: 1},
		{Name: "queue", Value: 2},
		{Name: "search", Value: 3},
	}
}

func TestRegisterAllEachGoroutineKeepsOwnSentinel(t *testing.T) {
	sentinels := testSentinels()
	seen := RegisterAll(sentinels, func(Sentinel) {})

	if len(seen) != len(sentinels) {
		t.Fatalf("len(seen) = %d, want %d", len(seen), len(sentinels))
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i].Name < seen[j].Name })
	want := testSentinels()
	sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen = %+v, want %+v", seen, want)
		}
	}
}

func TestBuggyRegisterAllEveryGoroutineSeesLastSentinel(t *testing.T) {
	sentinels := testSentinels()
	seen := BuggyRegisterAll(sentinels, func(Sentinel) {})

	if len(seen) != len(sentinels) {
		t.Fatalf("len(seen) = %d, want %d", len(seen), len(sentinels))
	}
	last := sentinels[len(sentinels)-1]
	for i, s := range seen {
		if s != last {
			t.Fatalf("seen[%d] = %+v, want %+v (every goroutine shares the last sentinel)", i, s, last)
		}
	}
}

func TestRegisterAllSingleSentinelEdgeCase(t *testing.T) {
	sentinels := []Sentinel{{Name: "solo", Value: 1}}

	fixed := RegisterAll(sentinels, func(Sentinel) {})
	buggy := BuggyRegisterAll(sentinels, func(Sentinel) {})

	if len(fixed) != 1 || fixed[0] != sentinels[0] {
		t.Fatalf("fixed = %+v, want %+v", fixed, sentinels)
	}
	if len(buggy) != 1 || buggy[0] != sentinels[0] {
		t.Fatalf("buggy = %+v, want %+v (single sentinel: bug can't manifest)", buggy, sentinels)
	}
}
```

## Review

Bootstrap registration is correct when every goroutine runs with exactly the
sentinel it was registered for, no matter how many run concurrently. The
mechanism to keep straight is that Go 1.22's per-iteration loop variables
only help variables the `for` statement itself declares; reusing a
pre-declared variable with `=` instead of `:=` sidesteps that protection
entirely, on every Go version. The barrier in `BuggyRegisterAll` does not
create the bug, it only pins the outcome so the test is deterministic
instead of a timing-dependent flake — in real code without it, any goroutine
that happens to run late enough hits the same shared, final, sentinel. Run
`go test -race`; passing `s` as a goroutine parameter in the fixed version
sidesteps the whole question by giving each goroutine its own copy.

## Resources

- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — which variables the per-iteration change actually covers.
- [Go spec: Go statements](https://go.dev/ref/spec#Go_statements) — function arguments are evaluated when the `go` statement executes, not when the goroutine runs.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — coordinating completion of a fan-out of goroutines.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-retry-exponential-backoff-timer-callback-index.md](29-retry-exponential-backoff-timer-callback-index.md) | Next: [31-batch-soft-delete-with-error-joined-defer.md](31-batch-soft-delete-with-error-joined-defer.md)
