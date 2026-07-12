# Exercise 6: Canonicalize JSON so map ordering never flakes the snapshot

When output derives from a Go map, key ordering is randomized and a naive snapshot
flakes. This module canonicalizes by round-tripping through `json.Marshal` — which
sorts map keys — and re-indenting with `json.Indent`, producing a stable byte
stream you can pin. It contrasts that with the trap of snapshotting a map's
`fmt`-rendered form.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
canonsnap/                 independent module: example.com/canonsnap
  go.mod                   go 1.26
  canon.go                 Canonicalize(any) ([]byte, error)
  cmd/
    demo/
      main.go              canonicalizes a nested map and prints it
  canon_test.go            stability-under-repetition + sorted-key + Example
```

Files: `canon.go`, `cmd/demo/main.go`, `canon_test.go`.
Implement: `Canonicalize(v any) ([]byte, error)` that marshals (sorting map keys), re-indents, and ends with one trailing newline.
Test: canonicalize a fresh `map[string]any` many times and assert every run yields the same bytes; assert keys come out sorted.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/06-canonical-json-snapshot/cmd/demo
cd go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/06-canonical-json-snapshot
```

### Why a Marshal round-trip is the canonical form

Go randomizes map iteration order on purpose, so any output that renders a map by
ranging over it is non-deterministic. `fmt.Sprintf("%v", m)` is the classic trap:
it looks stable in a quick manual check and then flakes in CI because the runtime
shuffled the keys. The fix is not to sort by hand — it is to route the output
through `encoding/json.Marshal`, whose documented behavior is that *map keys are
sorted*, recursively, at every level of nesting. That single guarantee turns a map
into a deterministic byte stream.

`Canonicalize` marshals `v` to compact JSON (sorted keys) and then re-indents it
with `json.Indent` into a `bytes.Buffer`, so the result is pretty-printed and
stable. Splitting marshal from indent is deliberate: `Marshal` gives the sort
guarantee, `Indent` gives the human-readable layout, and doing them separately
means you could feed `Indent` bytes that came from somewhere other than your own
`Marshal` call — for instance, canonicalizing a JSON payload you received over the
wire. The trailing newline keeps the output consistent with a golden file's
one-newline convention if you later store it under `testdata/`.

The proof is `TestStableUnderRepetition`: it builds the map fresh and canonicalizes
it a hundred times, asserting every result is byte-identical. Because the map is
rebuilt each iteration, its internal iteration order genuinely varies, yet the
canonical output does not — that is the sort guarantee doing its job.

Create `canon.go`:

```go
package canonsnap

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Canonicalize serializes v to indented JSON with a deterministic key order.
// json.Marshal sorts map keys (recursively), so output derived from a map is
// stable across runs. The result ends in one trailing newline.
func Canonicalize(v any) ([]byte, error) {
	compact, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonicalize: marshal: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return nil, fmt.Errorf("canonicalize: indent: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"os"

	"example.com/canonsnap"
)

func main() {
	// Keys are inserted out of order and one value is itself a map; the output
	// is sorted at every level regardless.
	payload := map[string]any{
		"zeta":  3,
		"alpha": map[string]any{"y": 2, "x": 1},
		"mike":  2,
	}
	out, err := canonsnap.Canonicalize(payload)
	if err != nil {
		log.Fatal(err)
	}
	os.Stdout.Write(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "alpha": {
    "x": 1,
    "y": 2
  },
  "mike": 2,
  "zeta": 3
}
```

### Tests

`TestStableUnderRepetition` canonicalizes a freshly-built map a hundred times and
asserts each result equals the first — the anti-flake proof. `TestSortedKeys`
pins the canonical form against an inline snapshot with keys in sorted order.
`TestKeyOrder` asserts the byte positions of the keys are ascending, an
independent check that the sort really happened.

Create `canon_test.go`:

```go
package canonsnap

import (
	"bytes"
	"testing"
)

func sample() map[string]any {
	return map[string]any{
		"zeta":  3,
		"alpha": map[string]any{"y": 2, "x": 1},
		"mike":  2,
	}
}

const canonical = `{
  "alpha": {
    "x": 1,
    "y": 2
  },
  "mike": 2,
  "zeta": 3
}
`

func TestSortedKeys(t *testing.T) {
	t.Parallel()

	got, err := Canonicalize(sample())
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if string(got) != canonical {
		t.Fatalf("canonical mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, canonical)
	}
}

func TestStableUnderRepetition(t *testing.T) {
	t.Parallel()

	first, err := Canonicalize(sample())
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	for i := range 100 {
		got, err := Canonicalize(sample())
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("iteration %d differs from first; canonicalization is not stable:\n%s", i, got)
		}
	}
}

func TestKeyOrder(t *testing.T) {
	t.Parallel()

	got, _ := Canonicalize(sample())
	ia := bytes.Index(got, []byte(`"alpha"`))
	im := bytes.Index(got, []byte(`"mike"`))
	iz := bytes.Index(got, []byte(`"zeta"`))
	if !(ia < im && im < iz) {
		t.Fatalf("keys not in sorted byte order: alpha=%d mike=%d zeta=%d", ia, im, iz)
	}
}
```

## Review

Canonicalization is correct when the same logical value always produces the same
bytes regardless of the map's internal iteration order — which is exactly what the
hundred-iteration test asserts. The mechanism is `json.Marshal`'s documented,
recursive key-sorting; do not reimplement it by hand-sorting keys, and above all
do not snapshot `fmt.Sprintf("%v", m)`, which renders keys in randomized order and
will flake. Keeping `Marshal` and `Indent` as separate steps is what lets you
canonicalize JSON from any source, not just your own structs. The output ends in a
single newline so it drops cleanly into a `testdata/` golden if you later scale
past an inline literal. Run `go test -race` to confirm stability and the sorted-key
order.

## Resources

- [encoding/json: Marshal](https://pkg.go.dev/encoding/json#Marshal) — "map keys are sorted", the guarantee canonicalization relies on.
- [encoding/json: Indent](https://pkg.go.dev/encoding/json#Indent) — re-indenting compact JSON into a stable pretty-printed form.
- [bytes: Buffer](https://pkg.go.dev/bytes#Buffer) — the growable byte sink `Indent` writes into.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-redact-volatile-fields.md](05-redact-volatile-fields.md) | Next: [07-http-handler-approval.md](07-http-handler-approval.md)
