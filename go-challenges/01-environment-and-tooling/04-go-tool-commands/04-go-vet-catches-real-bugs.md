# Exercise 4: go vet — The Static-Analysis Floor

`go vet` flags code that compiles but is almost always wrong. This module builds
the *correct* version of four patterns a backend hits — a network address, a
locked struct, and a concurrent counter — then shows the buggy form of each and
the exact `go vet` diagnostic that would reject it: `printf`, `copylocks`, and the
Go 1.25 `hostport` and `waitgroup` analyzers.

## What you'll build

```text
vet-floor/                     module example.com/vet-floor
  go.mod
  internal/
    netutil/
      netutil.go               Address (JoinHostPort); Counter; SumTo (safe WaitGroup)
      netutil_test.go          table test of Address; SumTo under -race
  cmd/
    demo/
      main.go                  prints IPv4/IPv6 addresses and a concurrent sum
```

- Files: `internal/netutil/netutil.go`, `internal/netutil/netutil_test.go`, `cmd/demo/main.go`.
- Implement: `Address` via `net.JoinHostPort`, a pointer-receiver `Counter`, and `SumTo` that calls `WaitGroup.Add` before launching each goroutine.
- Test: `Address` for IPv4, IPv6, and a hostname; `SumTo(100) == 100` under `-race`.
- Verify: `go vet ./...` is silent and `go test -race ./...` passes on the correct code; each buggy variant makes `go vet` exit non-zero.

Create the module:

```bash
mkdir -p vet-floor/cmd/demo vet-floor/internal/netutil
cd vet-floor
go mod init example.com/vet-floor
```

### Why vet is a separate stage from build

The compiler checks types; it does not check that a `Printf` format verb matches
its argument, because that is a runtime reflection concern. So
`fmt.Printf("Hello, %d\n", name)` with a string `name` *builds cleanly* and prints
garbage. `go vet` performs that check statically and rejects it. That exit-code
gap — `go build` returns 0, `go vet` returns non-zero, on the same source — is the
entire reason vet is its own required CI stage. The correct code below has none of
these defects, so `go vet ./...` is silent; the buggy forms are shown as
illustrations you must not paste in.

The four analyzers:

- `printf` — a format verb that does not match its argument type.
- `copylocks` — copying a value that contains a `sync.Mutex`, which silently
  produces a second, independent lock that guards nothing.
- `hostport` (Go 1.25) — `fmt.Sprintf("%s:%d", host, port)` used as a network
  address, which breaks for IPv6 literals because the address itself contains
  colons; the fix is `net.JoinHostPort`, which brackets an IPv6 host.
- `waitgroup` (Go 1.25) — calling `sync.WaitGroup.Add` inside the goroutine
  instead of before launching it, which races with `Wait` and can let `Wait`
  return before the work is counted.

Create `internal/netutil/netutil.go` — every pattern in its correct form:

```go
package netutil

import (
	"net"
	"strconv"
	"sync"
)

// Address builds a host:port string correct for both IPv4 and IPv6 literals.
// net.JoinHostPort brackets an IPv6 host so its colons are unambiguous.
func Address(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Counter is a concurrency-safe integer. Its methods take a pointer receiver so
// the embedded Mutex is never copied.
type Counter struct {
	mu sync.Mutex
	n  int
}

// Add increments the counter by delta.
func (c *Counter) Add(delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += delta
}

// Value returns the current count.
func (c *Counter) Value() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// SumTo increments a shared counter once per goroutine. WaitGroup.Add is called
// before each goroutine is launched, never inside it.
func SumTo(n int) int {
	var c Counter
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Add(1)
		}()
	}
	wg.Wait()
	return c.Value()
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/vet-floor/internal/netutil"
)

func main() {
	fmt.Println(netutil.Address("10.0.0.1", 8080))
	fmt.Println(netutil.Address("::1", 8080))
	fmt.Println(netutil.Address("api.internal", 443))
	fmt.Println("sum:", netutil.SumTo(100))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
10.0.0.1:8080
[::1]:8080
api.internal:443
sum: 100
```

The IPv6 case is the payoff: `net.JoinHostPort("::1", "8080")` produces
`[::1]:8080`, which `net.Dial` can actually parse. `fmt.Sprintf("%s:%d", "::1",
8080)` would produce `::1:8080`, which it cannot.

### The four bugs and their diagnostics

Each block below is illustrative — do NOT add it to the module. Each compiles;
each is rejected by `go vet`.

`printf` — the classic build-passes, vet-fails case:

```go
name := "Go"
fmt.Printf("Hello, %d\n", name) // string arg, %d verb
```

```text
# example.com/vet-floor
./x.go:7:2: fmt.Printf format %d has arg name of wrong type string
```

`copylocks` — passing a lock-bearing struct by value copies the mutex:

```go
type Config struct {
	mu   sync.Mutex
	name string
}

func byValue(c Config) string { return c.name } // c copies the Mutex
```

```text
# example.com/vet-floor
./x.go:10:16: byValue passes lock by value: example.com/vet-floor.Config contains sync.Mutex
```

`hostport` — an IPv6-unsafe address (Go 1.25 analyzer):

```go
addr := fmt.Sprintf("%s:%d", host, port) // breaks for IPv6 literals
conn, err := net.Dial("tcp", addr)
```

```text
# example.com/vet-floor
./x.go:NN:MM: address format "%s:%d" does not work with IPv6
```

`waitgroup` — Add inside the goroutine (Go 1.25 analyzer):

```go
var wg sync.WaitGroup
go func() {
	wg.Add(1) // races with Wait; Add belongs before the go statement
	defer wg.Done()
	work()
}()
wg.Wait()
```

```text
# example.com/vet-floor
./x.go:NN:MM: WaitGroup.Add called from inside new goroutine
```

The fixes are exactly what the correct `netutil.go` already does:
`net.JoinHostPort` for the address, a pointer receiver so `Config`/`Counter` is
never copied, and `wg.Add(1)` before the `go` statement.

### Tests

Create `internal/netutil/netutil_test.go`:

```go
package netutil

import "testing"

func TestAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, host string
		port       int
		want       string
	}{
		{"ipv4", "10.0.0.1", 8080, "10.0.0.1:8080"},
		{"ipv6", "::1", 8080, "[::1]:8080"},
		{"host", "api.internal", 443, "api.internal:443"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Address(tc.host, tc.port); got != tc.want {
				t.Fatalf("Address(%q,%d) = %q, want %q", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestSumTo(t *testing.T) {
	t.Parallel()
	if got := SumTo(100); got != 100 {
		t.Fatalf("SumTo(100) = %d, want 100", got)
	}
}
```

## Review

The module is correct when `go vet ./...` is silent and `go test -race ./...`
passes — the IPv6 test case (`[::1]:8080`) proves `Address` uses `JoinHostPort`,
and `SumTo(100) == 100` under `-race` proves the counter and `WaitGroup` are
sound. The lesson is the exit-code gap: `go build` accepts all four bugs;
`go vet` rejects them. Treating vet as advisory lets a printf mismatch, a copied
lock, an IPv6-unsafe address, or a racy `WaitGroup.Add` reach production. Wire
`go vet ./...` as a required stage. It is the floor, not the ceiling —
`golangci-lint` layers unused-variable, unchecked-error, and shadowing analyzers
on top.

## Resources

- [Command vet](https://pkg.go.dev/cmd/vet) — the analyzers `go vet` runs, including `printf` and `copylocks`.
- [hostport analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/hostport) — the Go 1.25 IPv6-address check.
- [waitgroup analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/waitgroup) — the Go 1.25 misplaced-`Add` check.
- [net.JoinHostPort](https://pkg.go.dev/net#JoinHostPort) — the IPv6-safe way to build an address.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-testable-examples-as-docs.md](03-testable-examples-as-docs.md) | Next: [05-go-doc-package-and-stdlib.md](05-go-doc-package-and-stdlib.md)
