# Exercise 1: Compose an Ordered Text-Cleanup Pipeline

Every indexed field in a search ingester goes through the same cleanup chain, so
that chain must be a first-class, testable value rather than a hand-inlined
sequence of calls. This module builds the composition core: a `Transform`
function type, a `Pipeline` that holds an ordered slice of them, and a `Clean`
that folds them left-to-right.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
pipeline/                 independent module: example.com/pipeline
  go.mod                  go 1.26
  pipeline.go             type Transform; type Pipeline; New(...Transform); Clean(string) string
  cmd/
    demo/
      main.go             runnable demo composing two example transforms
  pipeline_test.go        ordering, identity, and purity table tests
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `type Transform func(string) string`, `type Pipeline`, `New(transforms ...Transform) Pipeline`, and `Clean(input string) string` folding each transform in declared order; plus two example transforms `Lowercase` and `CollapseWhitespace`.
- Test: prove transforms run in declared order (append-then-wrap yields `[xa]`, not `x[a]`); the empty pipeline is the identity; `Clean` is pure (same input twice yields the same output and does not mutate the input).
- Verify: `go test -count=1 -race ./...`

### Why the Transform type is deliberately boring

`type Transform func(string) string` carries no error and no state. That is the
whole point: a stage that can neither fail nor remember anything is a pure
function, and pure functions compose without surprises. Composition becomes a
`for` loop over a slice, ordering is explicit in the slice, and every stage is
testable in isolation with a table of `(input, want)` pairs. The stages that
*can* fail — decoding a record, validating UTF-8, checking a length budget — live
in the orchestration layer (later exercises), not in the transform chain. Keeping
fallible work out of `Transform` is what lets `Clean` be a total function that
never returns an error.

`Clean` folds left-to-right: `output = input`, then `output = transform(output)`
for each transform in order. The declared order is the contract. If you build the
pipeline as `New(a, b)`, then `Clean(s)` computes `b(a(s))` — `a` runs first. A
test that appends `"a"` and then wraps in brackets must see `[xa]` (append ran
first, then wrap), never `x[a]`; that single assertion pins the fold direction so
a future refactor cannot silently reverse it.

The empty pipeline is the identity function: `New().Clean(s) == s`. That is not a
degenerate corner case to ignore — it is the algebraic base of the fold, and a
test for it catches an off-by-one that skips the first or last transform.

Create `pipeline.go`:

```go
package pipeline

import "strings"

// Transform is a pure, total cleanup stage: it maps a string to a string with no
// error and no state, so stages compose by simple function application.
type Transform func(string) string

// Pipeline is an ordered chain of transforms. The order of the slice is the
// contract: Clean applies transforms[0] first.
type Pipeline struct {
	transforms []Transform
}

// New builds a Pipeline that applies the given transforms in the order passed.
func New(transforms ...Transform) Pipeline {
	return Pipeline{transforms: transforms}
}

// Clean folds every transform over input, left to right. With New(a, b),
// Clean(s) returns b(a(s)). An empty pipeline returns input unchanged.
func (p Pipeline) Clean(input string) string {
	output := input
	for _, transform := range p.transforms {
		output = transform(output)
	}
	return output
}

// Lowercase is an example transform: Unicode-aware, locale-independent lowering.
func Lowercase(s string) string {
	return strings.ToLower(s)
}

// CollapseWhitespace is an example transform: it splits on runs of Unicode
// whitespace and rejoins with a single space, trimming the ends.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
```

### The runnable demo

The demo composes the two example transforms and cleans a messy title, showing
the fold in action.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipeline"
)

func main() {
	p := pipeline.New(pipeline.Lowercase, pipeline.CollapseWhitespace)

	titles := []string{
		"  The   QUICK  Brown\tFox  ",
		"GoLang\n\nSearch   Ingest",
	}
	for _, t := range titles {
		fmt.Printf("%q -> %q\n", t, p.Clean(t))
	}

	fmt.Printf("empty pipeline is identity: %v\n",
		pipeline.New().Clean("unchanged") == "unchanged")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"  The   QUICK  Brown\tFox  " -> "the quick brown fox"
"GoLang\n\nSearch   Ingest" -> "golang search ingest"
empty pipeline is identity: true
```

### Tests

The tests assert three properties of the composition core, independent of any
concrete transform. `TestRunsInDeclaredOrder` uses two closures whose composition
is order-sensitive, so the result distinguishes `b(a(s))` from `a(b(s))`.
`TestEmptyPipelineIsIdentity` pins the fold's base case. `TestCleanIsPure` runs
the same input twice and also confirms the input string is not mutated (strings
are immutable in Go, so this documents the contract and guards against a future
transform that tries to alias backing storage).

Create `pipeline_test.go`:

```go
package pipeline

import (
	"fmt"
	"testing"
)

func TestRunsInDeclaredOrder(t *testing.T) {
	t.Parallel()

	appendA := func(s string) string { return s + "a" }
	wrap := func(s string) string { return "[" + s + "]" }

	tests := []struct {
		name string
		p    Pipeline
		in   string
		want string
	}{
		{"append then wrap", New(appendA, wrap), "x", "[xa]"},
		{"wrap then append", New(wrap, appendA), "x", "[x]a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.p.Clean(tt.in); got != tt.want {
				t.Fatalf("Clean(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEmptyPipelineIsIdentity(t *testing.T) {
	t.Parallel()

	p := New()
	for _, in := range []string{"", "unchanged", "  spaces  ", "CAPS"} {
		if got := p.Clean(in); got != in {
			t.Fatalf("empty Clean(%q) = %q, want identity", in, got)
		}
	}
}

func TestCleanIsPure(t *testing.T) {
	t.Parallel()

	p := New(Lowercase, CollapseWhitespace)
	in := "  Hello   WORLD  "

	first := p.Clean(in)
	second := p.Clean(in)
	if first != second {
		t.Fatalf("Clean not deterministic: %q vs %q", first, second)
	}
	if in != "  Hello   WORLD  " {
		t.Fatalf("Clean mutated its input: %q", in)
	}
	if want := "hello world"; first != want {
		t.Fatalf("Clean(%q) = %q, want %q", in, first, want)
	}
}

func ExamplePipeline_Clean() {
	p := New(Lowercase, CollapseWhitespace)
	fmt.Printf("%q\n", p.Clean("  Go   SEARCH  "))
	// Output: "go search"
}
```

## Review

The composition core is correct when `Clean` is a left fold over the declared
order and nothing else. The order test is the load-bearing one: `[xa]` proves the
first transform runs first, and if a refactor reverses the loop the test turns
red. The identity test guards the empty-slice base case, which is exactly where an
off-by-one in the fold would hide. Keep fallible work out of `Transform` — a stage
that needs to return an error belongs in the orchestration layer, not in this
chain — so that `Clean` stays a total function and the pipeline value stays a
plain, composable thing. Run `go test -race` even though there is no concurrency
yet; it costs nothing and keeps the module honest as it grows.

## Resources

- [strings.ToLower, Fields, Join](https://pkg.go.dev/strings) — the standard-library string helpers behind the example transforms.
- [Effective Go: function values](https://go.dev/doc/effective_go#functions) — first-class functions and closures, the basis of the `Transform` type.
- [Go blog: errors are values](https://go.dev/blog/errors-are-values) — why keeping fallible work in a separate layer keeps the happy path clean.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pure-cleanup-transforms.md](02-pure-cleanup-transforms.md)
