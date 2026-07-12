# Exercise 9: Make the build fail on suspicious code

`go vet` accepts everything the compiler accepts and then flags the code that is
valid Go but almost certainly wrong: a `%d` handed a string, a lock copied by
value, a cancel func never called, a malformed struct tag. This module starts from
a package riddled with those defects, drives them to zero, and wires the composite
quality gate a senior owns.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
vetgate/                    independent module: example.com/vetgate
  go.mod                    go 1.26
  vetgate.go                Result with correct tags, Format, safe cancel usage
  cmd/demo/main.go          runnable demo of Format + JSON encoding
  vetgate_test.go           asserts Format output and JSON round-trip
```

Files: `vetgate.go`, `cmd/demo/main.go`, `vetgate_test.go`.
Implement: a `Result` struct with correct `json` tags, a `Format` function with a
correct `Printf` verb, and a `WithDeadline` helper that always calls its cancel
func — none of which `go vet` flags.
Test: assert `Format` produces the expected string and that `Result` round-trips
through `encoding/json` with the intended keys.
Verify: the composite gate
`test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...` passes
end to end.

### The defects vet catches, and why the compiler does not

Here is the package as a careless first draft. Every line compiles; every marked
line is a bug `go vet` reports. This block is illustrative — do not build it:

```go
// DEFECTIVE: compiles cleanly, but go vet flags every marked line.
package vetgate

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Result struct {
	URL    string `json"url"`     // structtag: missing colon, tag is ignored
	Status int    `json:"status"`
}

func Format(r Result) string {
	return fmt.Sprintf("%d -> %d", r.URL, r.Status) // printf: %d for a string
}

type Counter struct {
	mu sync.Mutex
	n  int
}

func (c Counter) Inc() { c.n++ } // copylocks: value receiver copies the Mutex

func WithDeadline(d time.Duration) context.Context {
	ctx, _ := context.WithTimeout(context.Background(), d) // lostcancel: cancel dropped
	return ctx
}
```

Four analyzers fire. `structtag`: `json"url"` is missing the colon, so it is not a
valid tag and `encoding/json` silently ignores it — the field marshals as `URL`,
not `url`. `printf`: `%d` given `r.URL` (a string) prints `%!d(string=...)`.
`copylocks`: a value receiver on `Counter` copies the embedded `sync.Mutex` on
every call, so the lock guards nothing. `lostcancel`: discarding the cancel func
from `context.WithTimeout` leaks the timer until the deadline fires. The type
checker is happy with all four; only vet objects.

```bash
go vet ./...
# vetgate.go:12: struct field tag `json"url"` not compatible with reflect.StructTag.Get: bad syntax for struct tag pair
# vetgate.go:17: fmt.Sprintf format %d has arg r.URL of wrong type string
# vetgate.go:26: Inc passes lock by value: example.com/vetgate.Counter contains sync.Mutex
# vetgate.go:31: the cancel function returned by context.WithTimeout should be called, not discarded, to avoid a context leak
# exit status 1
```

### The corrected package

Fixing each defect: the tag gets its colon, `Format` uses `%s` for the string,
`Counter.Inc` takes a pointer receiver so it mutates the real lock and counter,
and `WithDeadline` returns the cancel func so the caller can (and must) call it.

Create `vetgate.go`:

```go
package vetgate

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Result is one health-check outcome, encodable to JSON with lower-case keys.
type Result struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
}

// Format renders a Result for a human. %s for the string field, %d for the int.
func Format(r Result) string {
	return fmt.Sprintf("%s -> %d", r.URL, r.Status)
}

// Counter is a concurrency-safe tally. Methods take a pointer receiver so the
// embedded Mutex is never copied.
type Counter struct {
	mu sync.Mutex
	n  int
}

func (c *Counter) Inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *Counter) N() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// WithDeadline returns a context and its cancel func. Returning cancel (rather
// than discarding it) is what keeps vet's lostcancel analyzer quiet and the
// timer from leaking.
func WithDeadline(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/vetgate"
)

func main() {
	r := vetgate.Result{URL: "https://go.dev", Status: 200}
	fmt.Println(vetgate.Format(r))

	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
https://go.dev -> 200
{"url":"https://go.dev","status":200}
```

### Tests

The tests confirm the fixes are actually correct, not merely silenced: `Format`
produces the intended string, and the struct tags produce the intended JSON keys
via a real `encoding/json` round-trip. A concurrency test drives `Counter` under
`-race` to prove the pointer receiver made the lock effective.

Create `vetgate_test.go`:

```go
package vetgate

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestFormat(t *testing.T) {
	t.Parallel()
	got := Format(Result{URL: "https://go.dev", Status: 200})
	const want = "https://go.dev -> 200"
	if got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

func TestResultJSONKeys(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Result{URL: "https://go.dev", Status: 200})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["url"]; !ok {
		t.Fatalf("missing lower-case url key: %s", b)
	}
	if _, ok := m["status"]; !ok {
		t.Fatalf("missing status key: %s", b)
	}
}

func TestCounterIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	var c Counter
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	if c.N() != 100 {
		t.Fatalf("Counter.N() = %d, want 100", c.N())
	}
}

func ExampleFormat() {
	fmt.Println(Format(Result{URL: "https://go.dev", Status: 200}))
	// Output: https://go.dev -> 200
}
```

The single command a senior wires into CI and a pre-commit hook runs all three
gates and fails on the first:

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...
```

`gofmt -l` printing any filename means unformatted code; `go vet` exiting non-zero
means a suspicious construct; `go test -race` failing means a bug or a data race.
All three must be clean before a change merges.

## Review

The package is correct when vet is silent for the right reason: the struct tag is
well-formed so `encoding/json` honors it, the `Printf` verb matches its argument's
type, the lock is never copied because every method has a pointer receiver, and
the cancel func is returned rather than dropped. The tests prove the fixes are
real — the JSON round-trip would fail if the tag were still malformed, and the
`-race` counter test would fail if `Inc` still copied the lock. Silencing a vet
finding without a test to confirm the intended behavior is only half a fix.

The trap is treating vet as optional because the code compiles. Vet catches
valid-but-wrong Go that the type checker allows, and every one of these four
defects is a real production bug: a JSON field with the wrong key, a garbled log
line, a lock that guards nothing, a leaked timer. Wire `gofmt`, `go vet`, and
`go test -race` into one non-negotiable command and make passing it the
precondition to merge.

## Resources

- [go vet](https://pkg.go.dev/cmd/vet) — the analyzers and what each flags.
- [golang.org/x/tools/go/analysis](https://pkg.go.dev/golang.org/x/tools/go/analysis) — how the analyzers (and `staticcheck`, via `-vettool`) are built.
- [reflect.StructTag](https://pkg.go.dev/reflect#StructTag) — the tag syntax the `structtag` analyzer checks.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — why the cancel func must always be called.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-build-constraints-cross-compile.md](08-build-constraints-cross-compile.md) | Next: [10-structured-output-and-subcommands.md](10-structured-output-and-subcommands.md)
