# Exercise 5: Config Snapshot — Sort Keys for a Stable Ordered Output

`// Unordered output:` is not the only answer to map randomness. When you want the
documentation to read top-to-bottom in a meaningful order and the review diff to
stay stable, you sort the keys yourself and pin a plain `// Output:`. This
exercise builds a config-snapshot printer and pins its output that way, teaching
the senior trade-off between the two approaches.

## What you'll build

```text
confsnap/                   independent module: example.com/confsnap
  go.mod                    go 1.26
  confsnap.go               Snapshot(map[string]string) — sorted "k=v" per line
  cmd/
    demo/
      main.go               runnable demo printing a sorted config snapshot
  confsnap_test.go          table-driven Test + ExampleSnapshot with plain // Output:
```

Files: `confsnap.go`, `cmd/demo/main.go`, `confsnap_test.go`.
Implement: `Snapshot(map[string]string)` that prints entries as `key=value`, one per line, in sorted key order.
Test: a table-driven `Test`, plus `ExampleSnapshot` asserting an exact ordered block.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/15-testable-examples/05-sorted-keys-stable-output/cmd/demo
cd go-solutions/12-testing-ecosystem/15-testable-examples/05-sorted-keys-stable-output
```

## Sort-then-print versus // Unordered output:

The previous exercise used `// Unordered output:` to absorb map randomness. This
one takes the other road: `Snapshot` collects the keys, sorts them, and prints in
order, so a plain `// Output:` is both deterministic and *readable*. The two are
genuine alternatives with a real trade-off. `// Unordered output:` says "this is a
set; order is not part of the contract," and costs nothing at runtime. Sorting
says "the natural order is arbitrary, but I am pinning a canonical, alphabetical
one," and costs a single sort — in exchange for godoc that reads in order and a
diff a reviewer can scan when the config surface changes. For a config or
environment snapshot, where a human will read the block, the sorted form is
usually the better choice; for a pure set dump, `// Unordered output:` is
lighter.

The collect-and-sort idiom uses the modern iterator API: `maps.Keys(m)` returns
an `iter.Seq[string]` (a function-shaped iterator, Go 1.23+), and
`slices.Sorted` consumes that sequence and returns a sorted slice in one step. If
you needed the slice for more than sorting you would use `slices.Collect` first,
then `slices.Sort`; here `slices.Sorted(maps.Keys(m))` is the concise form.

Create `confsnap.go`:

```go
package confsnap

import (
	"fmt"
	"maps"
	"slices"
)

// Snapshot prints each config entry as "key=value", one per line, in sorted key
// order. Sorting makes the output deterministic (so a plain // Output: is safe)
// and readable (so a reviewer can scan the diff).
func Snapshot(cfg map[string]string) {
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		fmt.Printf("%s=%s\n", k, cfg[k])
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import "example.com/confsnap"

func main() {
	confsnap.Snapshot(map[string]string{
		"LOG_LEVEL": "info",
		"DB_HOST":   "db.internal",
		"PORT":      "8080",
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
DB_HOST=db.internal
LOG_LEVEL=info
PORT=8080
```

### Tests and examples

The `Test` captures the printed lines and asserts they are already in sorted
order (no re-sorting in the assertion — the whole point is that `Snapshot` sorts).
`ExampleSnapshot` pins the exact ordered block with a plain `// Output:`.

Create `confsnap_test.go`:

```go
package confsnap

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

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

func TestSnapshotSorted(t *testing.T) {
	cfg := map[string]string{"LOG_LEVEL": "info", "DB_HOST": "db.internal", "PORT": "8080"}
	out := captureStdout(t, func() { Snapshot(cfg) })

	got := strings.Split(strings.TrimSpace(out), "\n")
	want := []string{"DB_HOST=db.internal", "LOG_LEVEL=info", "PORT=8080"}
	if !slices.Equal(got, want) {
		t.Errorf("Snapshot output = %q, want sorted %q", got, want)
	}
	if !slices.IsSorted(got) {
		t.Errorf("Snapshot output is not sorted: %q", got)
	}
}

func ExampleSnapshot() {
	Snapshot(map[string]string{
		"LOG_LEVEL": "info",
		"DB_HOST":   "db.internal",
		"PORT":      "8080",
	})
	// Output:
	// DB_HOST=db.internal
	// LOG_LEVEL=info
	// PORT=8080
}
```

## Review

`ExampleSnapshot` is correct because `Snapshot` sorts, so a plain `// Output:`
block is deterministic despite the map input — and the block reads in a
meaningful order, which is the payoff over `// Unordered output:`. The judgment to
carry away: choose `// Unordered output:` when the value is genuinely a set and
order means nothing, and choose sort-then-`// Output:` when a human reads the
godoc or the review diff and a stable, canonical order is worth one sort. Confirm
`maps.Keys` yields an `iter.Seq` consumed by `slices.Sorted` (Go 1.23+ signature).
Keep `gofmt -l` empty and `go vet ./...` clean.

## Resources

- [slices.Sorted](https://pkg.go.dev/slices#Sorted) and [maps.Keys](https://pkg.go.dev/maps#Keys) — the iterator-to-sorted-slice idiom.
- [The Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — `iter.Seq` and how `maps.Keys` returns one.
- [testing package — Examples](https://pkg.go.dev/testing#hdr-Examples) — `// Output:` versus `// Unordered output:`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-unordered-output-header-set.md](04-unordered-output-header-set.md) | Next: [06-error-contract-example.md](06-error-contract-example.md)
