# Exercise 7: Multiple Assignment in an Address and DSN Parser

Tuple assignment is how Go parses and normalizes structured strings without scratch
variables: `host, port, err := net.SplitHostPort(addr)` destructures in one
statement, and `lo, hi = hi, lo` swaps safely because the whole right-hand side is
evaluated before any assignment. This exercise builds a listen-address normalizer
on those forms.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
netutil/                       independent module: example.com/netutil
  go.mod                       module example.com/netutil
  netutil.go                   NormalizeListenAddr; ClampPort; sortBounds swap
  cmd/
    demo/
      main.go                  normalizes several addresses and prints results
  netutil_test.go              ":8080", "[::1]:80", missing port, swap invariant
```

- Files: `netutil.go`, `cmd/demo/main.go`, `netutil_test.go`.
- Implement: `NormalizeListenAddr(addr, defHost)` using `net.SplitHostPort`/`net.JoinHostPort`, a `ClampPort` using multi-value reassignment, and a bounds swap `lo, hi = hi, lo`.
- Test: `":8080"` defaults the empty host, `"[::1]:80"` handles IPv6 brackets, a missing port errors, and the swap guarantees `lo <= hi`; `JoinHostPort` round-trips the parsed parts.
- Verify: `go test -count=1 -race ./...`

### Tuple assignment parses in one total statement

`net.SplitHostPort` returns three values, and `host, port, err := net.SplitHostPort(addr)`
binds all three at once. This is the idiom the multiple-assignment rule enables:
one statement, one clear failure path. For a listen address like `":8080"`,
`SplitHostPort` returns an empty host and `"8080"`; the normalizer substitutes a
default host (`"0.0.0.0"`) and rejoins with `net.JoinHostPort`, which is the
correct inverse — it adds the IPv6 brackets back when the host contains a colon, so
a parsed `"::1"` rejoins as `"[::1]:80"`. Hand-concatenating `host + ":" + port`
would break IPv6; `JoinHostPort` is the only correct join.

### Evaluation order is what makes the swap safe

`if lo > hi { lo, hi = hi, lo }` yields `lo <= hi` with no temporary because Go
fully evaluates the right-hand side (`hi, lo`) before assigning to the left
(`lo, hi`). The old values are captured first, then written, so there is no
clobbering. This same guarantee is why tuple parsing is total: every right-hand
expression is evaluated before any binding.

### `=` reassignment vs a fresh `:=` in a clamp

`ClampPort` reassigns an existing `p` with `=` when it pulls it back within
bounds, rather than introducing a new short-declared variable each step. Reusing the
one variable keeps the clamp readable and avoids accidentally shadowing a value you
still need. A fresh `:=` would create a new `port` and leave the original untouched
— exactly the kind of subtle bug the concepts file warns about.

Create `netutil.go`:

```go
package netutil

import (
	"fmt"
	"net"
	"strconv"
)

// NormalizeListenAddr splits addr into host and port, substitutes defHost when the
// host is empty, and rejoins with net.JoinHostPort (which handles IPv6 brackets).
func NormalizeListenAddr(addr, defHost string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("normalize %q: %w", addr, err)
	}
	if host == "" {
		host = defHost
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("normalize %q: bad port %q: %w", addr, port, err)
	}
	return net.JoinHostPort(host, port), nil
}

// ClampPort pulls p into [lo, hi]. It first normalizes the bounds with a swap, then
// reassigns p with = (not a fresh :=) so the single variable carries the result.
func ClampPort(p, lo, hi int) int {
	if lo > hi {
		lo, hi = hi, lo // safe swap: RHS fully evaluated before assignment
	}
	if p < lo {
		p = lo
	}
	if p > hi {
		p = hi
	}
	return p
}

// SortBounds returns lo, hi with lo <= hi, demonstrating tuple-return of a swap.
func SortBounds(a, b int) (lo, hi int) {
	lo, hi = a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/netutil"
)

func main() {
	for _, addr := range []string{":8080", "127.0.0.1:9090", "[::1]:80"} {
		out, err := netutil.NormalizeListenAddr(addr, "0.0.0.0")
		if err != nil {
			fmt.Printf("%s -> error: %v\n", addr, err)
			continue
		}
		fmt.Printf("%s -> %s\n", addr, out)
	}

	fmt.Printf("clamp 70000 into [1,65535] = %d\n", netutil.ClampPort(70000, 1, 65535))
	lo, hi := netutil.SortBounds(443, 80)
	fmt.Printf("sorted bounds: %d..%d\n", lo, hi)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
:8080 -> 0.0.0.0:8080
127.0.0.1:9090 -> 127.0.0.1:9090
[::1]:80 -> [::1]:80
clamp 70000 into [1,65535] = 65535
sorted bounds: 80..443
```

The empty host defaults to `0.0.0.0`, the IPv6 address keeps its brackets through
`JoinHostPort`, the clamp caps an out-of-range port, and the swap normalizes
reversed bounds.

### Tests

Create `netutil_test.go`:

```go
package netutil

import (
	"fmt"
	"testing"
)

func TestNormalizeListenAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		addr    string
		defHost string
		want    string
		wantErr bool
	}{
		{"empty host defaults", ":8080", "0.0.0.0", "0.0.0.0:8080", false},
		{"explicit host kept", "127.0.0.1:9090", "0.0.0.0", "127.0.0.1:9090", false},
		{"ipv6 brackets", "[::1]:80", "0.0.0.0", "[::1]:80", false},
		{"missing port", "nohost", "0.0.0.0", "", true},
		{"non-numeric port", "host:http", "0.0.0.0", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeListenAddr(tc.addr, tc.defHost)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeListenAddr(%q) = %q, want error", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeListenAddr(%q) unexpected error: %v", tc.addr, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeListenAddr(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

func TestClampPort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		p, lo, hi, want int
	}{
		{70000, 1, 65535, 65535},
		{0, 1, 65535, 1},
		{8080, 1, 65535, 8080},
		{50, 100, 10, 50}, // reversed bounds: swap yields [10,100]
		{5, 100, 10, 10},  // reversed bounds, below range
	}

	for _, tc := range cases {
		if got := ClampPort(tc.p, tc.lo, tc.hi); got != tc.want {
			t.Fatalf("ClampPort(%d,%d,%d) = %d, want %d", tc.p, tc.lo, tc.hi, got, tc.want)
		}
	}
}

func TestSortBoundsInvariant(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]int{{443, 80}, {80, 443}, {5, 5}} {
		lo, hi := SortBounds(pair[0], pair[1])
		if lo > hi {
			t.Fatalf("SortBounds(%d,%d) = %d,%d violates lo<=hi", pair[0], pair[1], lo, hi)
		}
	}
}

func ExampleNormalizeListenAddr() {
	out, _ := NormalizeListenAddr(":8080", "0.0.0.0")
	fmt.Println(out)
	// Output: 0.0.0.0:8080
}
```

`TestClampPort`'s reversed-bound
rows are the proof that the swap normalizes the interval before clamping.

## Review

The parser is correct when tuple assignment does the destructuring and stdlib does
the join: `net.SplitHostPort` yields `(host, port, err)` in one statement, and
`net.JoinHostPort` is the only correct inverse because it re-brackets IPv6 hosts.
The swap `lo, hi = hi, lo` relies on full right-hand-side evaluation before
assignment, so it needs no temporary. `ClampPort` reassigns the one `p` with `=`
rather than shadowing it with a new `:=`.

The mistakes to avoid: joining with `host + ":" + port` (breaks IPv6), and
introducing a fresh `:=` in a clamp where reassignment with `=` was intended. Run
`go test -race`; the functions are pure, so the value here is the table's edge cases
(empty host, IPv6, reversed bounds).

## Resources

- [net.SplitHostPort](https://pkg.go.dev/net#SplitHostPort)
- [net.JoinHostPort](https://pkg.go.dev/net#JoinHostPort)
- [Go Specification: Assignment statements (evaluation order)](https://go.dev/ref/spec#Assignment_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-blank-identifier-interface-guards.md](06-blank-identifier-interface-guards.md) | Next: [08-if-switch-init-scope.md](08-if-switch-init-scope.md)
