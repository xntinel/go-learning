# Exercise 7: Whole-File Example — The Package's Headline Usage Doc

When you ship a library, the block other teams read first is the "how do I use
this" sample at the top of the `pkg.go.dev` page. Go promotes exactly one kind of
example to that spot — a *whole-file* example — and this exercise builds a small
rate-limiter package plus a file that qualifies, so the whole file becomes the
package's headline usage sample.

## What you'll build

```text
limiter/                    independent module: example.com/limiter
  go.mod                    go 1.26
  limiter.go                type Bucket; New; Allow; Refill (a burst limiter)
  cmd/
    demo/
      main.go               runnable demo exhausting and refilling a bucket
  example_test.go           whole-file example: one Example + a const, no Test funcs
  limiter_test.go           table-driven Test (kept OUT of the example file)
```

Files: `limiter.go`, `cmd/demo/main.go`, `example_test.go`, `limiter_test.go`.
Implement: a `Bucket` burst limiter with `New(capacity)`, `Allow() bool`, and `Refill()`.
Test: a whole-file `Example` in its own file, plus a table-driven `Test` in a separate file.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/15-testable-examples/07-whole-file-package-example/cmd/demo
cd go-solutions/12-testing-ecosystem/15-testable-examples/07-whole-file-package-example
```

## What makes a file a whole-file example

`pkg.go.dev` renders a file — imports and all — as the package's headline usage
sample when three conditions hold together: the file contains exactly one example
function, it has at least one other top-level declaration (a `const`, `var`,
`type`, or helper func), and it contains no `Test*`, `Benchmark*`, or `Fuzz*`
functions at all. Under those conditions the renderer shows the entire file as the
canonical "how to use this package" block — precisely the getting-started sample a
well-shipped SDK leads with.

The fragile part is the third condition. Drop a single throwaway `TestX` into that
file and it silently loses whole-file promotion — the example still runs, but it
is no longer rendered as the package's headline sample. That is why the
table-driven `Test` here lives in a *separate* file, `limiter_test.go`, and
`example_test.go` holds only the example plus one `const`. `go doc -all` shows
whether the file is being treated as the package example; adding a `TestX` to
`example_test.go` and re-running `go doc` demonstrates the demotion, after which
you remove it.

`example_test.go` uses the external test package `limiter_test` (note the `_test`
suffix on the package name), which is idiomatic for a usage sample because it
exercises the package exactly as a real consumer would — through its exported API
only. The `const burst` is the "at least one other top-level declaration" that,
together with the single `Example`, makes the file qualify.

Create `limiter.go`:

```go
package limiter

// Bucket is a simple burst limiter: it admits up to a fixed number of calls,
// then denies further ones until Refill restores the budget.
type Bucket struct {
	tokens int
	cap    int
}

// New returns a Bucket that admits capacity calls before it starts denying.
func New(capacity int) *Bucket {
	return &Bucket{tokens: capacity, cap: capacity}
}

// Allow reports whether a call is admitted, consuming one token if so.
func (b *Bucket) Allow() bool {
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// Refill restores the bucket to full capacity.
func (b *Bucket) Refill() {
	b.tokens = b.cap
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/limiter"
)

func main() {
	b := limiter.New(2)
	fmt.Println(b.Allow())
	fmt.Println(b.Allow())
	fmt.Println(b.Allow())
	b.Refill()
	fmt.Println(b.Allow())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
true
true
false
true
```

### The whole-file example

Create `example_test.go`. This file has exactly one example, one other top-level
declaration, and no `Test`/`Benchmark`/`Fuzz` functions, so it qualifies as the
package's whole-file example:

```go
package limiter_test

import (
	"fmt"

	"example.com/limiter"
)

// burst is how many calls a fresh bucket admits before it starts denying.
const burst = 2

func Example() {
	b := limiter.New(burst)
	fmt.Println(b.Allow())
	fmt.Println(b.Allow())
	fmt.Println(b.Allow())
	// Output:
	// true
	// true
	// false
}
```

### The table-driven test, kept separate

Create `limiter_test.go`. Keeping the `Test` out of `example_test.go` is what
preserves the whole-file-example promotion:

```go
package limiter

import "testing"

func TestBucket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cap   int
		calls int
		want  []bool
	}{
		{"capacity two", 2, 3, []bool{true, true, false}},
		{"capacity zero denies", 0, 1, []bool{false}},
		{"capacity one", 1, 2, []bool{true, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := New(tt.cap)
			for i := range tt.calls {
				if got := b.Allow(); got != tt.want[i] {
					t.Errorf("call %d: Allow() = %v, want %v", i, got, tt.want[i])
				}
			}
		})
	}

	b := New(1)
	b.Allow()
	b.Refill()
	if !b.Allow() {
		t.Error("Allow after Refill = false, want true")
	}
}
```

## Review

The `Example` is correct when its three printed lines match the `// Output:`
block, and the file is a *whole-file* example only while it stays free of
`Test`/`Benchmark`/`Fuzz` functions — verify with `go doc -all` that the file is
promoted, then note that dropping a `TestX` into `example_test.go` demotes it.
Keeping the table-driven `Test` in `limiter_test.go` is the deliberate structure
that preserves promotion; merging the two files would quietly cost you the
headline sample. Keep `gofmt -l` empty and `go vet ./...` clean; the whole-file
example still runs like any other under `go test -run Example`.

## Resources

- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — the whole-file-example rule (one example, another decl, no test funcs).
- [The Go Blog: Testable Examples in Go](https://go.dev/blog/examples) — how whole-file examples become the package's headline sample.
- [go/doc — Example](https://pkg.go.dev/go/doc#Example) — extraction of whole-file examples.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-error-contract-example.md](06-error-contract-example.md) | Next: [08-no-output-comment-compiled-not-run.md](08-no-output-comment-compiled-not-run.md)
