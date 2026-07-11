# Exercise 8: Precomputed Collation Sort Keys for a Stable DB Index

Running a full collator on every query is expensive, and a database cannot
`ORDER BY` a Go function. The fix is a *sort key*: a byte slice whose plain
`bytes.Compare` order equals the collation order, precomputed once per row and
stored in an indexed column so the database gets locale-correct ordering from a
plain `bytea`. This module builds that indexing helper — and covers the trap that
makes it dangerous: sort keys are tied to the CLDR version.

This module is fully self-contained: its own `go mod init`, its own demo and tests.
It uses `golang.org/x/text/collate` and `golang.org/x/text/language`.

## What you'll build

```text
sortkeys/                   independent module: example.com/sortkeys
  go.mod                    requires golang.org/x/text
  index.go                  Indexer: KeyFromString via a reused collate.Buffer, copied out
  cmd/demo/main.go          build keys, ORDER BY the byte key
  index_test.go             key order == Compare order; Reset reuse; sort-by-key == live sort
```

Files: `index.go`, `cmd/demo/main.go`, `index_test.go`.
Implement: an `Indexer` wrapping a `*collate.Collator` and a reused `collate.Buffer`, whose `Key(string) []byte` copies the sort key out of the buffer so it survives the next call and `Reset`.
Test: `bytes.Compare(Key(a), Key(b))` has the same sign as `Collator.CompareString(a, b)` across a name set; `Buffer.Reset` lets the buffer be reused without corrupting already-copied keys; sorting rows by precomputed key equals sorting them live with `SortStrings`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sortkeys/cmd/demo
cd ~/go-exercises/sortkeys
go mod init example.com/sortkeys
go get golang.org/x/text/collate
```

### Sort keys, the Buffer, and copying out

`Collator.KeyFromString(buf, s)` returns a byte slice whose ordering under
`bytes.Compare` matches the collator's `CompareString` ordering. Store that key in
an indexed column and the database can `ORDER BY sort_key` and reproduce
locale-correct order without ever running the collator at query time — the
collation cost is paid once, at write time.

The subtlety is the `collate.Buffer`. `KeyFromString` appends the key into the
buffer's backing array and returns a slice into it, which is what makes key
generation allocation-light when you reuse one buffer across many rows. But that
also means the returned slice is only stable until `Buffer.Reset` is called (Reset
reclaims the backing array). So an indexer that intends to *keep* a key — to put it
in a struct, a row, a column — must **copy it out** with
`append([]byte(nil), key...)`. The `Key` method here does exactly that, so a caller
can accumulate keys freely and the indexer can `Reset` the buffer to reclaim memory
between batches without corrupting keys already handed out.

Like the collator, a `collate.Buffer` is stateful and not safe for concurrent use;
one `Indexer` belongs to one goroutine or a mutex.

And the operational warning that belongs in the runbook: a sort key encodes the
CLDR/locale data baked into the `x/text` version that produced it. Upgrading Go's
`x/text` can change the key bytes, so a precomputed `sort_key` column must be
**regenerated** on upgrade — otherwise newly written keys sort against old ones and
the order silently drifts.

Create `index.go`:

```go
package sortkeys

import (
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Indexer precomputes collation sort keys for storage in an indexed column. It
// reuses one collate.Buffer across rows and copies each key out so the key stays
// valid after the next call or a Reset. Not safe for concurrent use.
type Indexer struct {
	c   *collate.Collator
	buf collate.Buffer
}

// NewIndexer builds an Indexer for the given locale.
func NewIndexer(tag language.Tag, opts ...collate.Option) *Indexer {
	return &Indexer{c: collate.New(tag, opts...)}
}

// Key returns the collation sort key for s, copied out of the shared buffer so it
// remains valid independently of later calls and Reset.
func (ix *Indexer) Key(s string) []byte {
	k := ix.c.KeyFromString(&ix.buf, s)
	out := make([]byte, len(k))
	copy(out, k)
	return out
}

// Compare reports the live collation order of a and b (-1, 0, +1), for tests and
// callers that want to compare without materializing keys.
func (ix *Indexer) Compare(a, b string) int {
	return ix.c.CompareString(a, b)
}

// Reset reclaims the shared buffer's memory between batches. Keys already returned
// by Key are copies and are unaffected.
func (ix *Indexer) Reset() {
	ix.buf.Reset()
}
```

### The runnable demo

The demo precomputes a sort key per name, then sorts the rows purely by
`bytes.Compare` on the key — the database's `ORDER BY sort_key` — and prints the
resulting locale order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"sort"

	"example.com/sortkeys"
	"golang.org/x/text/language"
)

type row struct {
	name string
	key  []byte
}

func main() {
	names := []string{"Zorn", "Ström", "Ahlgren", "Öberg", "aker", "Åberg"}

	ix := sortkeys.NewIndexer(language.Make("sv"))
	rows := make([]row, len(names))
	for i, n := range names {
		rows[i] = row{name: n, key: ix.Key(n)}
	}

	// ORDER BY sort_key: pure byte comparison, no collator at query time.
	sort.Slice(rows, func(i, j int) bool {
		return bytes.Compare(rows[i].key, rows[j].key) < 0
	})

	ordered := make([]string, len(rows))
	for i, r := range rows {
		ordered[i] = r.name
	}
	fmt.Println("order by key:", ordered)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order by key: [Ahlgren aker Ström Zorn Åberg Öberg]
```

### Tests

Three properties. `TestKeyOrderMatchesCompare` is the core guarantee: the sign of
`bytes.Compare` on the keys equals the sign of the live `CompareString` for every
pair. `TestResetKeepsCopiedKeys` proves the copy-out contract: keys survive a
`Reset` and further key generation. `TestSortByKeyEqualsLiveSort` proves the whole
point — ordering rows by their precomputed keys gives the same result as sorting
live.

Create `index_test.go`:

```go
package sortkeys

import (
	"bytes"
	"reflect"
	"sort"
	"testing"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func TestKeyOrderMatchesCompare(t *testing.T) {
	t.Parallel()

	names := []string{"Zorn", "Ström", "Ahlgren", "Öberg", "aker", "Åberg"}
	ix := NewIndexer(language.Make("sv"))

	keys := make(map[string][]byte, len(names))
	for _, n := range names {
		keys[n] = ix.Key(n)
	}
	for _, a := range names {
		for _, b := range names {
			wantSign := sign(ix.Compare(a, b))
			gotSign := sign(bytes.Compare(keys[a], keys[b]))
			if gotSign != wantSign {
				t.Fatalf("sign mismatch for %q vs %q: key %d, compare %d", a, b, gotSign, wantSign)
			}
		}
	}
}

func TestResetKeepsCopiedKeys(t *testing.T) {
	t.Parallel()

	ix := NewIndexer(language.Make("sv"))
	aberg := ix.Key("Åberg")

	ix.Reset()               // reclaim the shared buffer
	_ = ix.Key("Zorn")       // generate a new key into the reused buffer
	again := ix.Key("Åberg") // fresh key for the same input

	// The pre-Reset copy must be byte-identical to a freshly computed one.
	if !bytes.Equal(aberg, again) {
		t.Fatalf("copied key changed across Reset: %v vs %v", aberg, again)
	}
}

func TestSortByKeyEqualsLiveSort(t *testing.T) {
	t.Parallel()

	names := []string{"Zorn", "Ström", "Ahlgren", "Öberg", "aker", "Åberg"}
	ix := NewIndexer(language.Make("sv"))

	type row struct {
		name string
		key  []byte
	}
	rows := make([]row, len(names))
	for i, n := range names {
		rows[i] = row{name: n, key: ix.Key(n)}
	}
	sort.Slice(rows, func(i, j int) bool {
		return bytes.Compare(rows[i].key, rows[j].key) < 0
	})
	byKey := make([]string, len(rows))
	for i, r := range rows {
		byKey[i] = r.name
	}

	live := append([]string(nil), names...)
	collate.New(language.Make("sv")).SortStrings(live)

	if !reflect.DeepEqual(byKey, live) {
		t.Fatalf("sort by key = %v, want live sort %v", byKey, live)
	}
}
```

## Review

The indexer is correct when byte order over its keys equals collation order:
`Key` returns a copy of `KeyFromString`, and `TestKeyOrderMatchesCompare` checks
that `bytes.Compare` agrees in sign with `CompareString` for every pair, while
`TestSortByKeyEqualsLiveSort` confirms an `ORDER BY sort_key` reproduces a live
sort. The two mistakes this module is built to prevent: holding the raw
`KeyFromString` slice past the next call or `Reset` (it aliases the shared buffer —
always copy out, which `Key` does), and treating the key column as permanent
(it encodes the CLDR/locale version, so it must be regenerated when `x/text` is
upgraded or the stored order drifts). As with the plain collator, one `Indexer` is
single-goroutine.

## Resources

- [`golang.org/x/text/collate`](https://pkg.go.dev/golang.org/x/text/collate) — `Collator.KeyFromString`/`Key`, `Buffer`, `Buffer.Reset`.
- [`bytes.Compare`](https://pkg.go.dev/bytes#Compare) — the byte ordering the sort key encodes.
- [UTS #10: Unicode Collation Algorithm, sort keys](https://www.unicode.org/reports/tr10/#Step_3) — how a sort key encodes multi-level order into bytes.

---

Back to [07-locale-aware-collation-for-listings.md](07-locale-aware-collation-for-listings.md) | Next: [09-streaming-normalization-for-ingestion.md](09-streaming-normalization-for-ingestion.md)
