# Exercise 11: Parsing "host:port" Config With the Last Colon

**Nivel: Intermedio** — validacion rapida (un test corto).

Every backend service reads a listen or dial address from config at startup —
`db.internal:5432`, `localhost:8080` — and splits it into a host and a
numeric port before connecting. This exercise builds
`ParseAddr(s) (host string, port int, err error)`, the small multi-return
function that sits at the top of nearly every service's config loading path.

This module is fully self-contained: its own `go mod init`, all code inline,
one demo, one quick test file.

## What you'll build

```text
hostport/                  independent module: example.com/hostport-config-parser
  go.mod                   go 1.24
  addr.go                  package addrs; ParseAddr(s) (host, port, err); ErrMissingPort/ErrBadPort
  cmd/
    demo/
      main.go              valid, missing-port, and bad-port inputs
  addr_test.go             one table test; errors.Is against both sentinels
```

- Files: `addr.go`, `cmd/demo/main.go`, `addr_test.go`.
- Implement: `ParseAddr(s string) (host string, port int, err error)` splitting on the *last* colon with `strings.LastIndex`, validating a non-empty host, converting the port with `strconv.Atoi`, and range-checking `1..65535`.
- Test: a table over two valid addresses, a missing colon, an empty host, a non-numeric port, port `0`, and port `70000`, asserting `errors.Is` against `ErrMissingPort` or `ErrBadPort`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hostport/cmd/demo
cd ~/go-exercises/hostport
go mod init example.com/hostport-config-parser
go mod edit -go=1.24
```

### The last colon, not the first

The obvious first attempt is `strings.Split(s, ":")`, but that breaks the
moment a host segment carries its own colon — an IPv6 literal, or any value
with unexpected extra punctuation — because `Split` then returns more than
two pieces with no principled way to recombine them. The fix is to split on
the *last* colon with `strings.LastIndex`: everything before it is the host,
whatever survives after it is the port. A `host:port` address only ever has
one delimiter that matters — the one separating the port from everything
else — and it is always the rightmost one.

Two failure modes matter, and each gets its own sentinel: `ErrMissingPort`
when there is no colon at all, or the host segment is empty; `ErrBadPort`
when a host is present but what follows the colon does not parse as a number
(`strconv.Atoi` fails) or falls outside `1..65535`. Both are wrapped with
`fmt.Errorf("%q: %w", s, ...)` — `%w` keeps the sentinel matchable with
`errors.Is` so a caller can branch on which failure occurred, while `%q`
keeps the offending raw input in the message for whoever reads the log.

Create `addr.go`:

```go
package addrs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMissingPort is returned when the input has no ":port" suffix at all.
var ErrMissingPort = errors.New("missing port")

// ErrBadPort is returned when the port is present but is not a valid
// 1..65535 number.
var ErrBadPort = errors.New("bad port")

// ParseAddr splits a "host:port" service address into its host and numeric
// port. It splits on the LAST colon, not the first, so IPv6-ish or
// otherwise colon-bearing host segments do not break the split.
func ParseAddr(s string) (host string, port int, err error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("%q: %w", s, ErrMissingPort)
	}

	host = s[:i]
	portStr := s[i+1:]
	if host == "" {
		return "", 0, fmt.Errorf("%q: %w", s, ErrMissingPort)
	}

	port, err = strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q: %w", s, ErrBadPort)
	}

	return host, port, nil
}
```

At the call site the three-value return reads left to right as intent — host,
then port, then whatever went wrong: `host, port, err := addrs.ParseAddr(raw)`,
handling `err` first. When only the port is needed, the host is discarded with
the blank identifier instead of a name nobody reads:
`_, port, err := addrs.ParseAddr(raw)`.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	addrs "example.com/hostport-config-parser"
)

func main() {
	for _, s := range []string{
		"db.internal:5432",
		"localhost:8080",
		"localhost",
		"localhost:http",
	} {
		host, port, err := addrs.ParseAddr(s)
		if err != nil {
			fmt.Printf("%-16s -> error: %v\n", s, err)
			continue
		}
		fmt.Printf("%-16s -> host=%s port=%d\n", s, host, port)
	}
}
```

Run it with `go run ./cmd/demo`. Expected output:

```
db.internal:5432 -> host=db.internal port=5432
localhost:8080   -> host=localhost port=8080
localhost        -> error: "localhost": missing port
localhost:http   -> error: "localhost:http": bad port
```

### Test

One table test drives both sentinels through six inputs. Create
`addr_test.go`:

```go
package addrs

import (
	"errors"
	"testing"
)

func TestParseAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantHost string
		wantPort int
		wantErr  error // nil means no error expected
	}{
		{"valid db", "db.internal:5432", "db.internal", 5432, nil},
		{"valid localhost", "localhost:8080", "localhost", 8080, nil},
		{"missing colon", "localhost", "", 0, ErrMissingPort},
		{"empty host", ":8080", "", 0, ErrMissingPort},
		{"non-numeric port", "localhost:http", "", 0, ErrBadPort},
		{"port zero", "localhost:0", "", 0, ErrBadPort},
		{"port too large", "localhost:70000", "", 0, ErrBadPort},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, port, err := ParseAddr(tc.input)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ParseAddr(%q): unexpected error: %v", tc.input, err)
				}
				if host != tc.wantHost || port != tc.wantPort {
					t.Fatalf("ParseAddr(%q) = (%q, %d), want (%q, %d)",
						tc.input, host, port, tc.wantHost, tc.wantPort)
				}
				return
			}

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseAddr(%q): err = %v, want errors.Is match for %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
```

## Review

`ParseAddr` is correct when a well-formed address yields the exact host and
port, and every malformed input lands on the right sentinel: no colon or an
empty host is `ErrMissingPort`; a non-numeric or out-of-range port is
`ErrBadPort`. The table test proves both branches with `errors.Is`, so a
caller's own error handling — say, retrying only on `ErrBadPort` — stays
testable against the same sentinels. The mistake this avoids: `strings.Split`
breaks on any value with more than one colon, while `strings.LastIndex`
anchors the split on the delimiter that actually matters.

## Resources

- [strings.LastIndex](https://pkg.go.dev/strings#LastIndex) — splitting on the rightmost delimiter instead of the first.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a returned error against a sentinel through `fmt.Errorf`'s `%w` wrap.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-handler-returning-error-adapter.md](10-handler-returning-error-adapter.md) | Next: [12-pagination-params-limit-offset.md](12-pagination-params-limit-offset.md)
