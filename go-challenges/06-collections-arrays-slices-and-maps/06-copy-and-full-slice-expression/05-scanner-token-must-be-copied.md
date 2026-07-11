# Exercise 5: Copy Scanner Tokens Before Retaining Them

A log aggregator scans lines with `bufio.Scanner` and keeps the ones matching a
predicate. `Scanner.Bytes()` returns a view into the scanner's internal buffer
that is overwritten on the next `Scan`, so a retained token must be copied with
`bytes.Clone` before it is stored. This exercise builds the aggregator correctly
and reproduces the classic bug where every stored entry ends up equal to the last
line once the buffer refills.

Self-contained module: own `go mod init`, own demo, own tests.

## What you'll build

```text
logscan/                   independent module: example.com/logscan
  go.mod                   go 1.26
  logscan.go               Collect (bytes.Clone), collectAliased (buggy), MatchPrefix
  cmd/
    demo/
      main.go              scan a log, keep ERROR lines, print distinct matches
  logscan_test.go          copy correctness under refill, aliasing corruption negative
```

Files: `logscan.go`, `cmd/demo/main.go`, `logscan_test.go`.
Implement: `Collect(sc, match)` storing `bytes.Clone(tok)`; a buggy `collectAliased` storing the raw token.
Test: feed a multi-line reader through a small-buffer scanner, assert each retained line has its correct distinct content; a negative sub-test stores the raw token and asserts the classic "all entries equal the last token" corruption.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logscan/cmd/demo
cd ~/go-exercises/logscan
go mod init example.com/logscan
```

### Why the token is only valid until the next Scan

`bufio.Scanner` reads into a fixed internal buffer and, on each `Scan`, returns a
slice of that buffer via `Bytes()` covering the current token. The documentation
is explicit that the token is only valid until the next call to `Scan`, because
the next read may overwrite the same buffer memory. When the input fits entirely
in the buffer, the tokens happen to live at different offsets and stay valid — so
the bug hides in unit tests with small fixtures. In production the input is a
multi-megabyte log, the buffer refills repeatedly, every token is re-read into the
same offset, and every raw token you stored now points at the *last* line's bytes.
The result is the signature failure: N stored entries that are all identical to
the final matched line.

The fix is one call. To retain a token past the next `Scan`, copy it out of the
scanner's buffer: `bytes.Clone(tok)` (or `slices.Clone(tok)`, or `string(tok)`
which allocates an immutable copy). The copy owns its own backing array, so a
later buffer refill cannot touch it. This exercise forces the refill deterministically
by giving the scanner a tiny buffer with `sc.Buffer(make([]byte, 0, 16), 16)`, so
the correct version's copy is what makes it survive and the buggy version visibly
collapses — exactly what a large real log would do.

Create `logscan.go`:

```go
package logscan

import (
	"bufio"
	"bytes"
)

// Collect returns a copy of every scanned token for which match reports true.
// Each token is cloned out of the scanner's buffer so it survives later Scans.
func Collect(sc *bufio.Scanner, match func([]byte) bool) [][]byte {
	var out [][]byte
	for sc.Scan() {
		tok := sc.Bytes()
		if match(tok) {
			out = append(out, bytes.Clone(tok))
		}
	}
	return out
}

// collectAliased is the buggy variant used only in tests: it retains the raw
// token, which the next Scan may overwrite.
func collectAliased(sc *bufio.Scanner, match func([]byte) bool) [][]byte {
	var out [][]byte
	for sc.Scan() {
		tok := sc.Bytes()
		if match(tok) {
			out = append(out, tok)
		}
	}
	return out
}

// MatchPrefix returns a predicate matching lines beginning with prefix.
func MatchPrefix(prefix string) func([]byte) bool {
	p := []byte(prefix)
	return func(line []byte) bool { return bytes.HasPrefix(line, p) }
}
```

### The runnable demo

The demo scans a small log and keeps the `ERROR ` lines, printing them to show the
copied tokens carry their correct distinct contents.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"fmt"
	"strings"

	"example.com/logscan"
)

func main() {
	log := "INFO  ok\nERROR E001\nINFO  ok\nERROR E002\nERROR E003\n"
	sc := bufio.NewScanner(strings.NewReader(log))

	matches := logscan.Collect(sc, logscan.MatchPrefix("ERROR "))
	for _, m := range matches {
		fmt.Printf("%s\n", m)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ERROR E001
ERROR E002
ERROR E003
```

### Tests

`TestCollectCopiesSurviveRefill` drives a small-buffer scanner (forcing the same
refill a large log would) and asserts every retained line has its correct distinct
content. `TestAliasedTokensCollapseToLast` runs the buggy `collectAliased` through
the same small-buffer scanner and asserts every entry equals the last matched line
— the classic corruption made explicit.

Create `logscan_test.go`:

```go
package logscan

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
)

const sampleLog = "INFO  ok\nERROR E001\nINFO  ok\nERROR E002\nERROR E003\n"

// tinyScanner forces the internal buffer to refill between lines, reproducing
// the refill behavior a large production log would cause.
func tinyScanner(s string) *bufio.Scanner {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 16), 16)
	return sc
}

func TestCollectCopiesSurviveRefill(t *testing.T) {
	t.Parallel()

	got := Collect(tinyScanner(sampleLog), MatchPrefix("ERROR "))
	want := []string{"ERROR E001", "ERROR E002", "ERROR E003"}
	if len(got) != len(want) {
		t.Fatalf("collected %d lines, want %d", len(got), len(want))
	}
	for i, w := range want {
		if string(got[i]) != w {
			t.Fatalf("match[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestAliasedTokensCollapseToLast(t *testing.T) {
	t.Parallel()

	got := collectAliased(tinyScanner(sampleLog), MatchPrefix("ERROR "))
	if len(got) != 3 {
		t.Fatalf("collected %d lines, want 3", len(got))
	}
	// Every retained raw token now aliases the last line's buffer contents.
	for i := range got {
		if string(got[i]) != "ERROR E003" {
			t.Fatalf("aliased match[%d] = %q, want all to collapse to %q", i, got[i], "ERROR E003")
		}
	}
}

func ExampleCollect() {
	sc := bufio.NewScanner(strings.NewReader(sampleLog))
	for _, m := range Collect(sc, MatchPrefix("ERROR ")) {
		fmt.Printf("%s\n", m)
	}
	// Output:
	// ERROR E001
	// ERROR E002
	// ERROR E003
}
```

## Review

The aggregator is correct when every retained line keeps its own content under
buffer refill: `TestCollectCopiesSurviveRefill` proves the `bytes.Clone` survives
the same refill that `TestAliasedTokensCollapseToLast` shows collapsing the raw
tokens to the last line. The insidious part, and the reason this bug reaches
production, is that with a small fixture the tokens sit at distinct offsets and the
buggy code passes; only a refill (large input, or the forced tiny buffer here)
exposes it. The rule is unconditional: any value from a "view into an internal
buffer" API — `Scanner.Bytes`, and similar — must be copied before you retain it
past the next call.

## Resources

- [`bufio.Scanner.Bytes` (token validity)](https://pkg.go.dev/bufio#Scanner.Bytes)
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone)
- [`bufio.Scanner.Buffer`](https://pkg.go.dev/bufio#Scanner.Buffer)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-append-shared-backing-array-corruption.md](04-append-shared-backing-array-corruption.md) | Next: [06-in-place-filter-with-delete-and-zeroing.md](06-in-place-filter-with-delete-and-zeroing.md)
