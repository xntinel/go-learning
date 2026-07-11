# Exercise 4: Header/Flag Set Dump — // Unordered output:

Some output is a set, not a sequence: the response headers on a request, the
feature flags enabled for a tenant. Printing a map one entry per line and pinning
it with a plain `// Output:` is the classic flaky example, because Go randomizes
map iteration. This exercise builds the set-dumping function the right way, with
`// Unordered output:`.

## What you'll build

```text
headerset/                  independent module: example.com/headerset
  go.mod                    go 1.26
  headerset.go              Dump(map[string]string) — prints "k: v" per entry
  cmd/
    demo/
      main.go               runnable demo dumping a small header set
  headerset_test.go         table-driven Test + ExampleDump with // Unordered output:
```

Files: `headerset.go`, `cmd/demo/main.go`, `headerset_test.go`.
Implement: `Dump(map[string]string)` that prints each entry as `key: value` on its own line.
Test: a table-driven `Test` that captures and sorts the lines, plus `ExampleDump` using `// Unordered output:`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/headerset/cmd/demo
cd ~/go-exercises/headerset
go mod init example.com/headerset
```

## Why a plain // Output: flakes, and what fixes it

Go deliberately randomizes the iteration order of a map. That is a feature — it
stops code from accidentally depending on an order the language never promised —
but it is fatal to a plain `// Output:` example that ranges a map: the lines are
correct but their order changes run to run, so the exact-match comparison passes
on your laptop and fails intermittently in CI. Running the example under
`go test -count=20` is enough to reproduce the flake.

`// Unordered output:` is the primitive built for exactly this. It compares the
example's stdout to the comment as a *multiset of lines*, ignoring order, so any
permutation of the same lines passes. It is the correct tool whenever the output
is set-shaped and its order carries no meaning — response headers and enabled
feature flags being the canonical backend cases. With `// Unordered output:`,
`go test -count=10` passes reliably; swap it back to `// Output:` and the same
loop reproduces the intermittent failure, which is the demonstration that the two
comment forms are genuinely different primitives.

Because the demo's `main` must be deterministic (it has no unordered-output
comment to protect it), the demo sorts the lines itself before printing; the
example, by contrast, prints in raw map order and relies on `// Unordered
output:` to absorb the randomness.

Create `headerset.go`:

```go
package headerset

import "fmt"

// Dump prints each header as "key: value" on its own line, in map-iteration
// order. Because Go randomizes that order, callers who need a stable order must
// sort; an example that pins Dump's output must use // Unordered output:.
func Dump(headers map[string]string) {
	for k, v := range headers {
		fmt.Printf("%s: %s\n", k, v)
	}
}
```

### The runnable demo

The demo sorts the keys before printing so its output is stable and its
Expected-output block is exact — a reminder that `Dump` itself does not sort.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"slices"

	"example.com/headerset"
)

func main() {
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Cache-Control": "no-store",
		"X-Request-Id":  "req-42",
	}

	// Dump prints in random order; for a stable demo we print sorted ourselves.
	_ = headerset.Dump // referenced so the package is exercised in tests, not here
	for _, k := range slices.Sorted(maps.Keys(headers)) {
		fmt.Printf("%s: %s\n", k, headers[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Cache-Control: no-store
Content-Type: application/json
X-Request-Id: req-42
```

### Tests and examples

The `Test` captures the lines `Dump` writes, sorts them, and compares to a sorted
expectation — the deterministic way to test order-independent output inside a
`Test`. `ExampleDump` uses `// Unordered output:` so it passes regardless of map
order.

Create `headerset_test.go`:

```go
package headerset

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// captureStdout runs f and returns everything it wrote to os.Stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return buf.String()
}

func TestDumpLines(t *testing.T) {
	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Request-Id": "req-42",
	}
	out := captureStdout(t, func() { Dump(headers) })

	got := strings.Split(strings.TrimSpace(out), "\n")
	slices.Sort(got)
	want := []string{"Content-Type: application/json", "X-Request-Id: req-42"}
	if !slices.Equal(got, want) {
		t.Errorf("Dump lines = %q, want %q", got, want)
	}
}

func ExampleDump() {
	Dump(map[string]string{
		"Content-Type": "application/json",
		"X-Request-Id": "req-42",
	})
	// Unordered output:
	// Content-Type: application/json
	// X-Request-Id: req-42
}
```

## Review

`ExampleDump` is correct precisely because it does not assume an order: with
`// Unordered output:` it passes under `go test -count=10 -run ExampleDump` every
time, whereas rewriting it with a plain `// Output:` reproduces the flake under
`-count=20`. That is the whole lesson — `// Unordered output:` is the right
primitive for set semantics, and reaching for a plain `// Output:` on map-derived
lines is the mistake. Note the `Test` handles the same non-determinism a
different way, by capturing and sorting the lines, which is the pattern to use
when you need order-independence inside a `Test` rather than an example. Keep
`gofmt -l` empty and `go vet ./...` clean.

## Resources

- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — documents `// Unordered output:` and when to use it.
- [The Go Blog: Testable Examples in Go](https://go.dev/blog/examples) — examples, output comments, and unordered output.
- [Go spec — For statements with range](https://go.dev/ref/spec#For_range) — the iteration order of a map is unspecified.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-json-response-deterministic-output.md](03-json-response-deterministic-output.md) | Next: [05-sorted-keys-stable-output.md](05-sorted-keys-stable-output.md)
