# Exercise 2: The Multi-Line Shape Of A Joined Error Message

When a joined error lands in a log, its `Error()` string renders one wrapped error
per line. That shape is convenient for humans and grep, and it is exactly the thing
you must not turn into a parse contract. This exercise pins the observable
formatting: line count equals the number of non-nil inputs, and an aggregate built
by a collector contains every failing source name.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
joinshape/                 independent module: example.com/joinshape
  go.mod                   go 1.26
  joinshape.go             Collect() aggregating named probes; Lines() helper
  cmd/
    demo/
      main.go              prints a three-failure aggregate and its line count
  joinshape_test.go        asserts line count and per-name Contains
```

- Files: `joinshape.go`, `cmd/demo/main.go`, `joinshape_test.go`.
- Implement: a `Collect(probes ...Probe) error` that wraps each failing probe with its name and joins them; a `Lines(err error) []string` that splits `Error()` on newlines.
- Test: split `Error()` on `\n` and assert the count equals the number of non-nil inputs; assert each source name is `strings.Contains`-present in the aggregated message.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/07-multiple-error-returns/02-joined-error-message-shape/cmd/demo
cd go-solutions/10-error-handling/07-multiple-error-returns/02-joined-error-message-shape
```

### What the joined `Error()` guarantees, and what it does not

`errors.Join` documents its `Error()` as "the concatenation of the strings obtained
by calling the Error method of each of the elements, one per line". So for K non-nil
members you get K lines separated by `\n`. That is enough to assert two useful
things in a test: the number of lines equals the number of failures, and each
failure's identifying text (here, the source name stamped by `fmt.Errorf`) appears
somewhere in the message.

What you must *not* do is depend on this as a machine contract. The line order
tracks input order, and the exact per-line text is whatever the member's `Error()`
returns — both are implementation choices upstream of you, not a stable API. When a
caller needs to *act* on individual failures (map each to an HTTP status, retry a
subset), you expose typed errors through `errors.As` (Exercise 10) rather than
splitting this string. This exercise deliberately asserts only the loose, honest
properties: count and containment, not exact layout.

Create `joinshape.go`:

```go
package joinshape

import (
	"errors"
	"fmt"
	"strings"
)

// Probe is a named check; a nil Err means it passed.
type Probe struct {
	Name string
	Err  error
}

// Collect wraps each failing probe with its name and joins the failures. It
// returns nil when every probe passed.
func Collect(probes ...Probe) error {
	var errs []error
	for _, p := range probes {
		if p.Err != nil {
			errs = append(errs, fmt.Errorf("probe %q: %w", p.Name, p.Err))
		}
	}
	return errors.Join(errs...)
}

// Lines splits a joined error's message into its per-member lines. A nil error
// yields no lines.
func Lines(err error) []string {
	if err == nil {
		return nil
	}
	return strings.Split(err.Error(), "\n")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/joinshape"
)

func main() {
	err := joinshape.Collect(
		joinshape.Probe{Name: "billing", Err: errors.New("timeout")},
		joinshape.Probe{Name: "inventory", Err: errors.New("connection refused")},
		joinshape.Probe{Name: "shipping", Err: errors.New("503")},
	)

	lines := joinshape.Lines(err)
	fmt.Printf("failures: %d\n", len(lines))
	for _, l := range lines {
		fmt.Println(l)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
failures: 3
probe "billing": timeout
probe "inventory": connection refused
probe "shipping": 503
```

### Tests

`TestLineCountEqualsFailures` builds an aggregate of three failures interleaved
with a passing probe and asserts `Lines` returns exactly three — proving nil inputs
do not produce blank lines. `TestMessageContainsEverySourceName` asserts every
failing name is `strings.Contains`-present in the message, which is the property a
log reader relies on. `TestNilIsZeroLines` pins that an all-passing collect returns
nil and zero lines. The tests assert count and containment, never exact ordering
beyond what input order trivially gives.

Create `joinshape_test.go`:

```go
package joinshape

import (
	"errors"
	"strings"
	"testing"
)

func TestLineCountEqualsFailures(t *testing.T) {
	t.Parallel()

	err := Collect(
		Probe{Name: "a", Err: errors.New("x")},
		Probe{Name: "ok", Err: nil},
		Probe{Name: "b", Err: errors.New("y")},
		Probe{Name: "c", Err: errors.New("z")},
	)
	if got := len(Lines(err)); got != 3 {
		t.Fatalf("Lines count = %d, want 3 (message=%q)", got, err.Error())
	}
}

func TestMessageContainsEverySourceName(t *testing.T) {
	t.Parallel()

	names := []string{"billing", "inventory", "shipping"}
	probes := make([]Probe, len(names))
	for i, n := range names {
		probes[i] = Probe{Name: n, Err: errors.New("down")}
	}
	msg := Collect(probes...).Error()
	for _, n := range names {
		if !strings.Contains(msg, n) {
			t.Errorf("message %q missing source name %q", msg, n)
		}
	}
}

func TestNilIsZeroLines(t *testing.T) {
	t.Parallel()

	err := Collect(Probe{Name: "a", Err: nil}, Probe{Name: "b", Err: nil})
	if err != nil {
		t.Fatalf("Collect(all-ok) = %v, want nil", err)
	}
	if got := len(Lines(err)); got != 0 {
		t.Fatalf("Lines(nil) count = %d, want 0", got)
	}
}
```

## Review

The joined message is correct when its line count tracks the number of non-nil
members and each member's identifying text is present — the two properties these
tests assert. Note what they avoid: no assertion on exact line ordering beyond
input order, and no parsing of the line text to extract structure. That restraint
is the lesson. The message is a log surface; if a caller needs to branch on a
specific failure, surface a typed error and use `errors.As` (Exercise 10), because
the string layout is not a contract you control. Run with `-race` for habit; the
code is synchronous.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — documents the "one per line" `Error()` formatting.
- [strings.Split](https://pkg.go.dev/strings#Split) and [strings.Contains](https://pkg.go.dev/strings#Contains) — the assertions here.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-source-collector-join.md](01-source-collector-join.md) | Next: [03-join-nil-semantics.md](03-join-nil-semantics.md)
