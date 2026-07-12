# Exercise 9: Parsing host:port By Chaining Multiple-Return Calls

Every server reads a listen address like `127.0.0.1:8080` or `[::1]:443` from
config and has to split, convert, and validate it. This exercise builds
`ParseEndpoint(addr) (host string, port int, err error)` by chaining two fallible
calls — `net.SplitHostPort` then `strconv.Atoi` — early-returning on each error and
distinguishing a syntax failure from an out-of-range port.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
endpoint/                  independent module: example.com/endpoint
  go.mod                   go 1.25
  endpoint.go              ParseEndpoint(addr) (host string, port int, err error); ErrPortRange sentinel
  cmd/
    demo/
      main.go              IPv4, IPv6, missing-port, non-numeric, out-of-range
  endpoint_test.go         table tests; errors.Is(strconv.ErrSyntax); errors.Is(ErrPortRange); -race
```

- Files: `endpoint.go`, `cmd/demo/main.go`, `endpoint_test.go`.
- Implement: `ParseEndpoint` that calls `net.SplitHostPort`, feeds the port string to `strconv.Atoi`, and range-checks `1..65535`, wrapping each failure with `%w` and returning three named values.
- Test: `127.0.0.1:8080` and `[::1]:443` parse; a missing port errors; a non-numeric port gives `errors.Is(err, strconv.ErrSyntax)`; port `0` or `> 65535` gives `errors.Is(err, ErrPortRange)`, distinct from the syntax error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Chaining fallible calls, early-returning on each

`ParseEndpoint` composes two operations that each return `(value, error)`, and the
second consumes the first's success:

1. `net.SplitHostPort(addr)` returns `(host, portStr, err)`. It fails on a missing
   port (`"127.0.0.1"`), too many colons, or unbracketed IPv6. On failure,
   `ParseEndpoint` wraps and returns immediately — there is no port string to
   convert.
2. Only if the split succeeded do we call `strconv.Atoi(portStr)`. It fails on a
   non-numeric port with an error that wraps `strconv.ErrSyntax`.
3. Only if the conversion succeeded do we range-check. A port must be `1..65535`;
   `0` and anything above `65535` are invalid. This is *our* validation, not the
   stdlib's, so we return our own `ErrPortRange` sentinel — distinct from the
   syntax error, so a caller can tell "the port was not a number" from "the number
   was out of range".

Each step early-returns with a wrapped error, so the final message traces the
failure path (`parse endpoint "…": …`). The three-value return `(host, port, err)`
names each purpose: a caller reads the intent straight off the signature and does
not have to guess what the second `int` means.

`net.SplitHostPort` handles the IPv6 bracket rule for you: `[::1]:443` returns host
`::1` (brackets stripped) and port `443`. Rolling your own `strings.Split(addr,
":")` would break on the colons inside an IPv6 literal — always use the stdlib
splitter.

Create `endpoint.go`:

```go
package endpoint

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// ErrPortRange is returned when the port parses as a number but falls outside
// the valid 1..65535 range. It is distinct from strconv.ErrSyntax so callers can
// tell "not a number" from "out of range".
var ErrPortRange = errors.New("port out of range")

// ParseEndpoint splits a listen/dial address into host and numeric port,
// validating the range. It chains net.SplitHostPort and strconv.Atoi, returning
// early on each failure.
func ParseEndpoint(addr string) (host string, port int, err error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("parse endpoint %q: %w", addr, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("parse endpoint %q port: %w", addr, err)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("parse endpoint %q: port %d: %w", addr, port, ErrPortRange)
	}
	return host, port, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/endpoint"
)

func main() {
	for _, addr := range []string{
		"127.0.0.1:8080",
		"[::1]:443",
		"localhost",       // missing port
		"localhost:http",  // non-numeric
		"localhost:70000", // out of range
	} {
		host, port, err := endpoint.ParseEndpoint(addr)
		if err != nil {
			fmt.Printf("%-16s -> error: %v\n", addr, err)
			continue
		}
		fmt.Printf("%-16s -> host=%s port=%d\n", addr, host, port)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
127.0.0.1:8080   -> host=127.0.0.1 port=8080
[::1]:443        -> host=::1 port=443
localhost        -> error: parse endpoint "localhost": address localhost: missing port in address
localhost:http   -> error: parse endpoint "localhost:http" port: strconv.Atoi: parsing "http": invalid syntax
localhost:70000  -> error: parse endpoint "localhost:70000": port 70000: port out of range
```

### Tests

Create `endpoint_test.go`:

```go
package endpoint

import (
	"errors"
	"strconv"
	"testing"
)

func TestParseEndpointValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr     string
		wantHost string
		wantPort int
	}{
		{"127.0.0.1:8080", "127.0.0.1", 8080},
		{"[::1]:443", "::1", 443},
		{"example.com:65535", "example.com", 65535},
		{"host:1", "host", 1},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			host, port, err := ParseEndpoint(tc.addr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Fatalf("ParseEndpoint(%q) = (%q, %d), want (%q, %d)", tc.addr, host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestParseEndpointMissingPort(t *testing.T) {
	t.Parallel()
	if _, _, err := ParseEndpoint("localhost"); err == nil {
		t.Fatal("want error for missing port, got nil")
	}
}

func TestParseEndpointNonNumericPort(t *testing.T) {
	t.Parallel()
	_, _, err := ParseEndpoint("localhost:http")
	if !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("err = %v, want a wrap of strconv.ErrSyntax", err)
	}
	if errors.Is(err, ErrPortRange) {
		t.Fatal("a non-numeric port must not be classified as a range error")
	}
}

func TestParseEndpointOutOfRange(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"host:0", "host:70000", "host:99999"} {
		_, _, err := ParseEndpoint(addr)
		if !errors.Is(err, ErrPortRange) {
			t.Fatalf("ParseEndpoint(%q): err = %v, want ErrPortRange", addr, err)
		}
		if errors.Is(err, strconv.ErrSyntax) {
			t.Fatalf("ParseEndpoint(%q): a range error must not be a syntax error", addr)
		}
	}
}
```

## Review

`ParseEndpoint` is correct when a well-formed address yields the host and numeric
port, and each failure class is distinguishable: a missing port surfaces
`SplitHostPort`'s error, a non-numeric port wraps `strconv.ErrSyntax`, and an
out-of-range port wraps `ErrPortRange`. `TestParseEndpointNonNumericPort` and
`TestParseEndpointOutOfRange` prove those two error chains are separate — a caller
that wants to accept a hostname-only address and default the port branches on the
concrete error, which is only possible because the shapes are kept apart.

The mistakes are chaining order and IPv6. Calling `strconv.Atoi` before checking
`SplitHostPort`'s error would run on an empty string; early-return on each step.
And splitting on `":"` by hand instead of `net.SplitHostPort` mangles `[::1]:443`
into garbage — the stdlib splitter exists precisely because IPv6 literals contain
colons.

## Resources

- [net.SplitHostPort](https://pkg.go.dev/net#SplitHostPort) — the IPv6-aware splitter and its error cases.
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi) — string-to-int conversion that wraps `strconv.ErrSyntax`.
- [errors.Is](https://pkg.go.dev/errors#Is) — distinguishing the syntax chain from the range chain.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-generic-retry-wrapper.md](08-generic-retry-wrapper.md) | Next: [10-handler-returning-error-adapter.md](10-handler-returning-error-adapter.md)
