# Exercise 4: Differential Testing a Hand-Rolled host:port Parser Against a Reference

On a request-per-microsecond path, `net.SplitHostPort` can be too much: it
allocates, validates IPv6 brackets, and handles zone identifiers you may not
need. Teams write a stripped-down splitter for the common `host:port` case and
pay for it in risk — a hand-rolled parser is exactly where the accept/reject
decision quietly diverges from the standard library. The oracle (differential)
property is the defense: the fast implementation must agree with the trusted
reference on *both* the parsed value *and* the accept/reject decision, for every
input in the subset the fast path claims to handle. This exercise builds the fast
splitter and differential-tests it against `net.SplitHostPort` with `pgregory.net/rapid`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
hostport/                   independent module: example.com/hostport
  go.mod                    go 1.26, requires pgregory.net/rapid
  hostport.go               FastSplit(string) (host, port string, ok bool)
  cmd/
    demo/
      main.go               runnable demo: split a few addresses
  hostport_test.go          rapid differential property against net.SplitHostPort
```

Files: `hostport.go`, `cmd/demo/main.go`, `hostport_test.go`.
Implement: `FastSplit`, an allocation-light splitter for unbracketed `host:port` with exactly one colon.
Test: a `rapid.Check` differential property that draws both valid-looking and fully random strings, gates on the exact subset the fast path targets, and asserts agreement with `net.SplitHostPort` on value and decision.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/24-property-based-testing/04-differential-oracle-parser/cmd/demo
cd go-solutions/12-testing-ecosystem/24-property-based-testing/04-differential-oracle-parser
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### Defining the fast path's contract, then gating the oracle on it

The fast splitter targets one shape: an unbracketed address with exactly one
colon, `host:port`, where either part may be empty. It rejects everything else —
no colon (missing port), more than one colon (ambiguous or IPv6), or any square
bracket (IPv6 literal form it deliberately does not handle). Rejecting outside its
contract is not a weakness; it is what lets the differential property be airtight.

`FastSplit` finds the last colon, takes everything before it as the host and
everything after as the port, and rejects if the host still contains a colon (that
means there were two or more) or if the input contains a bracket. That is the
whole parser — no allocation beyond the returned substrings, no validation of what
the host or port *contain*.

The oracle is `net.SplitHostPort`, which handles far more: it strips IPv6 brackets
(`[::1]:80` -> host `::1`), rejects multi-colon unbracketed input with "too many
colons", and rejects a missing port. The naive differential — "`FastSplit` must
equal `net.SplitHostPort` for all inputs" — is wrong, because on `[a]:9` the oracle
strips the brackets and returns host `a` while `FastSplit` (correctly, per its
contract) rejects the bracket. The fix is to gate the comparison on exactly the
subset the fast path claims: the input is in the supported domain when the oracle
accepts it, the oracle's host contains no colon, and the input has no brackets.
Inside that subset, the two must agree on host, port, and acceptance; outside it,
`FastSplit` must reject. This gating predicate — the precise boundary the fast path
targets — is the crux of a correct differential test. Get it wrong and you either
get false divergences (comparing outside the contract) or you miss real ones
(gating too loosely).

Create `hostport.go`:

```go
package hostport

import "strings"

// FastSplit splits an unbracketed "host:port" with exactly one colon into its
// host and port parts. It rejects (ok=false) any input with no colon, more than
// one colon, or a square bracket — the IPv6 and error cases it does not handle.
// Either part may be empty.
func FastSplit(s string) (host, port string, ok bool) {
	if strings.ContainsAny(s, "[]") {
		return "", "", false
	}
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	host, port = s[:i], s[i+1:]
	if strings.ContainsRune(host, ':') {
		return "", "", false // a second colon: ambiguous / IPv6
	}
	return host, port, true
}
```

### The runnable demo

The demo splits a plain address, a host with no port, and an IPv6 literal to show
the fast path accepting the first and rejecting the others.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hostport"
)

func main() {
	for _, s := range []string{"api.internal:8443", "localhost", "[::1]:80"} {
		h, p, ok := hostport.FastSplit(s)
		fmt.Printf("%-20s host=%q port=%q ok=%v\n", s, h, p, ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api.internal:8443    host="api.internal" port="8443" ok=true
localhost            host="" port="" ok=false
[::1]:80             host="" port="" ok=false
```

### The differential property

The generator draws from `rapid.OneOf`: a `StringMatching` producing valid-looking
`host:port` strings so the accept path is exercised, plus a fully random
`rapid.String()` so the reject path and adversarial inputs (multiple colons,
brackets, empty parts) are exercised too, plus a handful of hand-picked corner
cases via `Just`. Without the random arm the test would only ever see well-formed
input and never probe the boundary; without the structured arm it would rarely
generate a valid address by chance. The property computes both parsers, builds the
gating predicate, and asserts agreement inside the supported subset and rejection
outside it.

Create `hostport_test.go`:

```go
package hostport

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func genAddr() *rapid.Generator[string] {
	return rapid.OneOf(
		rapid.StringMatching(`[a-z0-9.]{0,6}:[0-9]{0,5}`),
		rapid.String(),
		rapid.Just(":80"),
		rapid.Just("a:b:c"),
		rapid.Just("[::1]:80"),
		rapid.Just("host"),
	)
}

func TestFastSplitDifferential(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		s := genAddr().Draw(t, "addr")

		fh, fp, fok := FastSplit(s)
		oh, op, oerr := net.SplitHostPort(s)

		// The supported domain: exactly what FastSplit claims to handle.
		supported := oerr == nil &&
			!strings.Contains(oh, ":") &&
			!strings.ContainsAny(s, "[]")

		if supported {
			if !fok {
				t.Fatalf("FastSplit(%q) rejected a supported address", s)
			}
			if fh != oh || fp != op {
				t.Fatalf("FastSplit(%q)=(%q,%q), oracle=(%q,%q)", s, fh, fp, oh, op)
			}
			return
		}
		if fok {
			t.Fatalf("FastSplit(%q)=(%q,%q) accepted an out-of-contract address", s, fh, fp)
		}
	})
}

func ExampleFastSplit() {
	h, p, ok := FastSplit("db.internal:5432")
	fmt.Println(h, p, ok)
	// Output: db.internal 5432 true
}
```

## Review

The parser is correct when, inside the subset it targets — unbracketed, exactly one
colon — it agrees with `net.SplitHostPort` on host, port, and acceptance, and
outside that subset it rejects. The differential property proves this over both
structured and random inputs; the gating predicate is what makes it airtight, and
building that predicate from the oracle's result plus the input shape (not from the
fast parser's internals) is what keeps the test independent.

The mistakes to avoid are the classic differential-testing traps. First, do not
compare the two implementations over the full input space: the oracle handles cases
(bracketed IPv6, bracket-stripping) the fast path deliberately does not, so an
ungated comparison reports false divergences on inputs the fast path never promised
to handle. Second, do not gate too loosely — forgetting the bracket exclusion or
the "oracle host has no colon" clause lets a real divergence slip through, which is
worse than a false one. Third, do not compare un-normalized outputs; here both
sides return raw substrings so no normalization is needed, but the moment either
side canonicalized (lowercasing a host, trimming), you would have to normalize both
before comparing or the property would fail on a non-bug. Run `go test -race`; the
parser is pure.

## Resources

- [`net.SplitHostPort`](https://pkg.go.dev/net#SplitHostPort) — the trusted oracle, including its bracket and multi-colon rules.
- [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid) — `OneOf`, `StringMatching`, `Just`, and `Check`.
- [`strings`](https://pkg.go.dev/strings#LastIndexByte) — `LastIndexByte`, `ContainsAny`, and `ContainsRune`, the substring primitives the fast path uses.

---

Back to [03-idempotent-canonicalization.md](03-idempotent-canonicalization.md) | Next: [05-metamorphic-query-pipeline.md](05-metamorphic-query-pipeline.md)
