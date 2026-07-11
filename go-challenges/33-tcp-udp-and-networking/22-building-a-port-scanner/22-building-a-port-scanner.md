# 22. Building a Port Scanner

TCP port scanning is deceptively simple in concept — dial a port, observe whether the connection succeeds — but every production concern falls out of that one step: concurrency limits to avoid file-descriptor exhaustion, configurable timeouts to distinguish closed from filtered, banner grabbing with a separate read deadline, and a clean result type that downstream code can filter and sort. This lesson builds a real `portscanner` package that handles all of these and is verified with a `*_test.go` that starts live TCP listeners.

```text
portscanner/
  go.mod
  portscanner.go
  portscanner_test.go
  cmd/demo/main.go
```

## Concepts

### The three port states

A TCP connect scan (the only scan type available without raw sockets or root privileges) produces exactly three observable outcomes:

- **Open** — the kernel on the remote host completes the three-way handshake. `net.DialTimeout` returns a live `net.Conn`.
- **Closed** — no process is listening on that port; the kernel sends RST. `net.DialTimeout` returns an error whose `Timeout()` method returns `false` and whose inner error is "connection refused".
- **Filtered** — a packet-filtering firewall (or a very slow host) drops the SYN without responding. `net.DialTimeout` returns a `net.Error` with `Timeout() == true` after the deadline expires.

The two error cases look different at the `net.Error` level. The classification function uses `errors.As` to retrieve the `net.Error` interface, then inspects `Timeout()`:

```go
var netErr net.Error
if errors.As(err, &netErr) && netErr.Timeout() {
	return StateFiltered
}
return StateClosed
```

### Concurrency control with a semaphore channel

Scanning 1000 ports at 500 ms timeout would take 500 s sequentially. Launching 1000 goroutines at once risks running out of file descriptors (the default `ulimit -n` on most systems is 1024). The idiomatic Go semaphore is a buffered channel:

```go
sem := make(chan struct{}, concurrency)
// acquire
sem <- struct{}{}
// release
<-sem
```

Each goroutine sends into the channel before dialing and receives from it when done. When the channel is full (all N slots are occupied), new goroutines block on the send until an existing one finishes and frees a slot. No mutex, no sync primitive beyond `sync.WaitGroup` for completion.

### Banner grabbing and the read deadline

After the handshake succeeds, many services send an identification line immediately (SSH, SMTP, FTP, many custom protocols). Reading that banner requires a separate deadline because the per-port dial timeout applies only to the handshake:

```go
conn.SetReadDeadline(time.Now().Add(time.Second))
buf := make([]byte, 1024)
n, _ := conn.Read(buf)
```

If the service does not send a banner within one second, `Read` returns an `io.EOF` or a deadline error and `n == 0`, so the banner string is empty. Ignore the read error: an empty banner is not a scan failure.

### Functional options for scanner configuration

The scanner accepts options at construction time via the functional-options pattern. Each option is a `func(*Scanner) error`. The constructor applies them in order; the first error aborts construction. Sentinel errors (`ErrInvalidHost`, etc.) allow callers to assert with `errors.Is` rather than matching strings.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/portscanner/cmd/demo
cd ~/go-exercises/portscanner
go mod init example.com/portscanner
```

This is a library with a thin demo; verification is via `go test`.

### Exercise 1: State, Result, and Sentinel Errors

Create `portscanner.go`:

```go
package portscanner

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// State is the observed state of a TCP port.
type State int

const (
	// StateOpen means the port accepted a connection.
	StateOpen State = iota
	// StateClosed means the port replied with RST (connection refused).
	StateClosed
	// StateFiltered means no response arrived within the timeout.
	StateFiltered
)

// String implements fmt.Stringer.
func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateClosed:
		return "closed"
	case StateFiltered:
		return "filtered"
	default:
		return "unknown"
	}
}

// Result is the outcome of probing one port.
type Result struct {
	Host    string
	Port    int
	State   State
	Banner  string
	Elapsed time.Duration
}

// Sentinel errors.
var (
	ErrInvalidHost        = errors.New("host must not be empty")
	ErrInvalidConcurrency = errors.New("concurrency must be at least 1")
	ErrInvalidTimeout     = errors.New("timeout must be positive")
	ErrInvalidPortRange   = errors.New("port must be between 1 and 65535")
)

// Scanner probes TCP ports on a single target host.
type Scanner struct {
	host        string
	timeout     time.Duration
	concurrency int
	grabBanner  bool
}

// Option configures a Scanner.
type Option func(*Scanner) error

// New creates a Scanner with defaults: host 127.0.0.1, timeout 500 ms,
// concurrency 100, banner grabbing off.
func New(opts ...Option) (*Scanner, error) {
	s := &Scanner{
		host:        "127.0.0.1",
		timeout:     500 * time.Millisecond,
		concurrency: 100,
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, fmt.Errorf("portscanner: %w", err)
		}
	}
	return s, nil
}

// WithHost sets the target host or IP address.
func WithHost(host string) Option {
	return func(s *Scanner) error {
		if host == "" {
			return ErrInvalidHost
		}
		s.host = host
		return nil
	}
}

// WithTimeout sets the per-port dial timeout.
func WithTimeout(d time.Duration) Option {
	return func(s *Scanner) error {
		if d <= 0 {
			return ErrInvalidTimeout
		}
		s.timeout = d
		return nil
	}
}

// WithConcurrency limits simultaneous dial attempts.
func WithConcurrency(n int) Option {
	return func(s *Scanner) error {
		if n < 1 {
			return ErrInvalidConcurrency
		}
		s.concurrency = n
		return nil
	}
}

// WithBannerGrab enables reading up to 1 KiB from each open connection.
func WithBannerGrab(on bool) Option {
	return func(s *Scanner) error {
		s.grabBanner = on
		return nil
	}
}

// Host returns the configured target host.
func (sc *Scanner) Host() string { return sc.host }

// Timeout returns the configured dial timeout.
func (sc *Scanner) Timeout() time.Duration { return sc.timeout }

// Concurrency returns the maximum simultaneous dial attempts.
func (sc *Scanner) Concurrency() int { return sc.concurrency }

// Scan probes each port and returns one Result per port, in input order.
// It caps goroutine count at the configured concurrency using a semaphore
// channel, so at most Concurrency() sockets are open at any moment.
func (sc *Scanner) Scan(ports []int) []Result {
	results := make([]Result, len(ports))
	sem := make(chan struct{}, sc.concurrency)
	var wg sync.WaitGroup
	for i, port := range ports {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = sc.probePort(port)
		}()
	}
	wg.Wait()
	return results
}

func (sc *Scanner) probePort(port int) Result {
	addr := net.JoinHostPort(sc.host, strconv.Itoa(port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, sc.timeout)
	elapsed := time.Since(start)
	if err != nil {
		return Result{
			Host:    sc.host,
			Port:    port,
			State:   classifyErr(err),
			Elapsed: elapsed,
		}
	}
	defer conn.Close()
	banner := ""
	if sc.grabBanner {
		banner = grabBanner(conn)
	}
	return Result{
		Host:    sc.host,
		Port:    port,
		State:   StateOpen,
		Banner:  banner,
		Elapsed: elapsed,
	}
}

// classifyErr maps a dial error to StateClosed or StateFiltered.
// A timeout (filtered) returns a net.Error with Timeout() == true.
// Connection refused (closed) does not.
func classifyErr(err error) State {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return StateFiltered
	}
	return StateClosed
}

// grabBanner reads up to 1 KiB from an open connection with a 1-second
// read deadline. A service that sends no banner returns an empty string.
func grabBanner(conn net.Conn) string {
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1024)
	n, _ := conn.Read(buf)
	return string(buf[:n])
}

// OpenPorts returns only the results with StateOpen.
func OpenPorts(results []Result) []Result {
	out := results[:0:0]
	for _, r := range results {
		if r.State == StateOpen {
			out = append(out, r)
		}
	}
	return out
}

// PortRange returns [low, high] inclusive as a slice of ints.
// Both endpoints must be in [1, 65535] and low must not exceed high.
func PortRange(low, high int) ([]int, error) {
	if low < 1 || high > 65535 || low > high {
		return nil, fmt.Errorf("%w: [%d, %d]", ErrInvalidPortRange, low, high)
	}
	ports := make([]int, high-low+1)
	for i := range ports {
		ports[i] = low + i
	}
	return ports, nil
}
```

`New` sets safe defaults before applying options; each option validates its argument and returns an `ErrInvalid*` sentinel so the caller can use `errors.Is`.

### Exercise 2: Test the Contract with Live Listeners

Create `portscanner_test.go`:

```go
package portscanner

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	sc, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if sc.host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", sc.host)
	}
	if sc.timeout != 500*time.Millisecond {
		t.Errorf("timeout = %v, want 500ms", sc.timeout)
	}
	if sc.concurrency != 100 {
		t.Errorf("concurrency = %d, want 100", sc.concurrency)
	}
}

func TestWithHostRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := New(WithHost(""))
	if !errors.Is(err, ErrInvalidHost) {
		t.Fatalf("err = %v, want ErrInvalidHost", err)
	}
}

func TestWithTimeoutRejectsNonPositive(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{0, -time.Second} {
		_, err := New(WithTimeout(d))
		if !errors.Is(err, ErrInvalidTimeout) {
			t.Errorf("WithTimeout(%v): err = %v, want ErrInvalidTimeout", d, err)
		}
	}
}

func TestWithConcurrencyRejectsZero(t *testing.T) {
	t.Parallel()

	_, err := New(WithConcurrency(0))
	if !errors.Is(err, ErrInvalidConcurrency) {
		t.Fatalf("err = %v, want ErrInvalidConcurrency", err)
	}
}

func TestScanDetectsOpenPort(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	sc, err := New(
		WithHost("127.0.0.1"),
		WithTimeout(2*time.Second),
		WithConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	results := sc.Scan([]int{port})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].State != StateOpen {
		t.Fatalf("port %d: want open, got %s", port, results[0].State)
	}
}

func TestScanDetectsClosedPort(t *testing.T) {
	t.Parallel()

	// Reserve a port then close it immediately. The kernel will reply with
	// RST on any subsequent connect attempt (no TIME_WAIT for a never-connected
	// listening socket).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	sc, err := New(
		WithHost("127.0.0.1"),
		WithTimeout(2*time.Second),
		WithConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	results := sc.Scan([]int{port})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].State != StateClosed {
		t.Fatalf("port %d: want closed, got %s", port, results[0].State)
	}
}

func TestScanBannerGrab(t *testing.T) {
	t.Parallel()

	const greeting = "HELLO\r\n"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte(greeting))
		conn.Close()
	}()

	sc, err := New(
		WithHost("127.0.0.1"),
		WithTimeout(2*time.Second),
		WithConcurrency(1),
		WithBannerGrab(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	results := sc.Scan([]int{port})
	if len(results) != 1 || results[0].State != StateOpen {
		t.Fatalf("want open, got %+v", results)
	}
	if results[0].Banner != greeting {
		t.Fatalf("banner = %q, want %q", results[0].Banner, greeting)
	}
}

func TestScanMultiplePorts(t *testing.T) {
	t.Parallel()

	// Open two listeners; scanning both plus a closed port returns the right
	// states and keeps input order.
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()

	p1 := ln1.Addr().(*net.TCPAddr).Port
	p2 := ln2.Addr().(*net.TCPAddr).Port

	// A third listener we immediately close to get a closed port.
	ln3, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p3 := ln3.Addr().(*net.TCPAddr).Port
	ln3.Close()

	sc, err := New(
		WithHost("127.0.0.1"),
		WithTimeout(2*time.Second),
		WithConcurrency(3),
	)
	if err != nil {
		t.Fatal(err)
	}

	ports := []int{p1, p2, p3}
	results := sc.Scan(ports)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	// Order is preserved: results[i] corresponds to ports[i].
	if results[0].Port != p1 || results[0].State != StateOpen {
		t.Errorf("results[0]: want %d open, got %+v", p1, results[0])
	}
	if results[1].Port != p2 || results[1].State != StateOpen {
		t.Errorf("results[1]: want %d open, got %+v", p2, results[1])
	}
	if results[2].Port != p3 || results[2].State != StateClosed {
		t.Errorf("results[2]: want %d closed, got %+v", p3, results[2])
	}
}

func TestOpenPortsFilter(t *testing.T) {
	t.Parallel()

	results := []Result{
		{Port: 80, State: StateOpen},
		{Port: 81, State: StateClosed},
		{Port: 443, State: StateOpen},
		{Port: 8080, State: StateFiltered},
	}
	open := OpenPorts(results)
	if len(open) != 2 {
		t.Fatalf("want 2 open results, got %d", len(open))
	}
	if open[0].Port != 80 || open[1].Port != 443 {
		t.Fatalf("open ports wrong: %v", open)
	}
}

func TestPortRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		low, high int
		wantLen   int
		wantErr   error
	}{
		{low: 80, high: 83, wantLen: 4},
		{low: 1, high: 1, wantLen: 1},
		{low: 22, high: 25, wantLen: 4},
		{low: 0, high: 80, wantErr: ErrInvalidPortRange},
		{low: 80, high: 65536, wantErr: ErrInvalidPortRange},
		{low: 443, high: 80, wantErr: ErrInvalidPortRange},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d-%d", tc.low, tc.high), func(t *testing.T) {
			t.Parallel()
			ports, err := PortRange(tc.low, tc.high)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(ports) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(ports), tc.wantLen)
			}
			if ports[0] != tc.low || ports[len(ports)-1] != tc.high {
				t.Fatalf("range endpoints wrong: got [%d..%d], want [%d..%d]",
					ports[0], ports[len(ports)-1], tc.low, tc.high)
			}
		})
	}
}

// ExampleNew shows how to create a scanner with non-default options and
// read back its configuration via the exported accessors.
func ExampleNew() {
	sc, _ := New(WithHost("127.0.0.1"), WithConcurrency(10))
	fmt.Printf("host=%s concurrency=%d\n", sc.Host(), sc.Concurrency())
	// Output:
	// host=127.0.0.1 concurrency=10
}

// ExamplePortRange shows that PortRange produces a contiguous int slice.
func ExamplePortRange() {
	ports, _ := PortRange(80, 83)
	fmt.Println(ports)
	// Output:
	// [80 81 82 83]
}

// Your turn: add TestScanResultsPreserveOrder. Start N listeners, scan their
// ports in a known order, and assert that results[i].Port == ports[i] for
// every i. This pins the contract that Scan preserves input order even under
// high concurrency.
```

### Exercise 3: The Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"example.com/portscanner"
)

func main() {
	// Start two listeners so the demo has guaranteed open ports.
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ln1.Close()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ln2.Close()

	p1 := ln1.Addr().(*net.TCPAddr).Port
	p2 := ln2.Addr().(*net.TCPAddr).Port

	// Scan the two open ports plus one closed neighbour.
	ports := []int{p1 - 1, p1, p2}

	sc, err := portscanner.New(
		portscanner.WithHost("127.0.0.1"),
		portscanner.WithTimeout(500*time.Millisecond),
		portscanner.WithConcurrency(10),
		portscanner.WithBannerGrab(true),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	results := sc.Scan(ports)
	for _, r := range results {
		fmt.Printf("%-6d %-10s (%s)\n", r.Port, r.State, r.Elapsed.Round(time.Millisecond))
	}

	open := portscanner.OpenPorts(results)
	fmt.Printf("\n%d open port(s) on %s\n", len(open), sc.Host())
}
```

Run the demo with:

```bash
go run ./cmd/demo
```

Expected output (port numbers are dynamic; the two open results correspond to `p1` and `p2`):

```text
NNNNN  closed     (0ms)
NNNNN  open       (0ms)
NNNNN  open       (0ms)

2 open port(s) on 127.0.0.1
```

## Common Mistakes

### Not closing connections when the port is open

Wrong: dialing to detect open ports but never calling `conn.Close()`. Each open connection holds a file descriptor. On a scan of 10 000 ports with 100 concurrent workers, leaving connections open exhausts the process's file-descriptor limit (default `ulimit -n` is 1024 on many systems), causing dials to fail with "too many open files".

Fix: always `defer conn.Close()` immediately after a successful `net.DialTimeout`. The `probePort` function in this lesson does that; any banner grabbing happens before the deferred close.

### Using a single dial timeout for both connect and banner read

Wrong: `net.DialTimeout("tcp", addr, 5*time.Second)` and then `conn.Read(buf)` with no separate deadline. The read blocks for the full dial timeout on every open port that sends no banner — the scanner runs 10x slower than it should.

Fix: the dial timeout covers only the TCP handshake. Set a fresh, short read deadline (`conn.SetReadDeadline`) immediately after connecting, as `grabBanner` does. Keep them separate and small (500 ms dial, 1 s read is a reasonable default).

### Assuming `net.Error.Timeout()` is the only dial-error discriminant

Wrong: treating any non-nil dial error as "filtered" because you checked `err != nil` but forgot to inspect `Timeout()`. Connection-refused errors would be silently misclassified as filtered — the scan would report every closed port as filtered, making it useless.

Fix: use `errors.As(err, &netErr)` and check `netErr.Timeout()`. Anything that is not a timeout is a hard rejection (StateClosed). The `classifyErr` function in this lesson separates the two cases correctly.

### Relying on port order without pinning it in tests

Wrong: launching goroutines for each port and appending results to a shared slice with `append` — the order of results is undefined under race conditions.

Fix: pre-allocate the results slice to `len(ports)` and write to index `i` inside the goroutine. Each goroutine owns its slot; no locking is needed. `Scan` in this lesson uses `results[i] = sc.probePort(port)` with the captured loop variable `i`. This is what `TestScanMultiplePorts` pins.

## Verification

From `~/go-exercises/portscanner`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass with no output except the test runner's result lines. The `-race` flag is critical: the semaphore channel and the pre-allocated results slice must be free of data races under concurrent access.

Run the demo to observe the scanner against live ports:

```bash
go run ./cmd/demo
```

Add one more test of your own: `TestScanResultsPreserveOrder`. Start N listeners, collect their ports into a known-order slice, scan with concurrency N, and assert that `results[i].Port == ports[i]` for every `i`. This pins the order guarantee regardless of goroutine scheduling.

## Summary

- A TCP connect scan produces three states: open (handshake succeeds), closed (RST), or filtered (timeout). Classify the dial error by calling `Timeout()` on `net.Error` via `errors.As`.
- A buffered channel of size N is the idiomatic Go semaphore: send to acquire a slot, receive to release. This caps goroutine count at N without mutexes.
- Set a short, separate read deadline immediately after connecting for banner grabbing. Never let `conn.Read` inherit the dial timeout.
- Pre-allocate the results slice and write to index `i` inside each goroutine. Appending to a shared slice under concurrency is a data race.
- Sentinel errors wrapped with `%w` let callers use `errors.Is`; string matching is fragile and breaks on refactoring.

## What's Next

Next: [DNS Recursive Resolver](../23-dns-recursive-resolver/23-dns-recursive-resolver.md).

## Resources

- [pkg.go.dev/net — DialTimeout](https://pkg.go.dev/net#DialTimeout) — signature, error semantics, and the `net.Error` interface with `Timeout()`.
- [pkg.go.dev/net — Conn.SetReadDeadline](https://pkg.go.dev/net#Conn) — per-operation deadline independent of the dial timeout.
- [go.dev/blog/pipelines](https://go.dev/blog/pipelines) — semaphore channels and the done-channel pattern; explains the buffered-channel semaphore used in `Scan`.
- [go.dev/doc/articles/race_detector](https://go.dev/doc/articles/race_detector) — why `-race` is mandatory and what it detects; directly relevant to the concurrent-write-to-shared-slice mistake.
- [IANA Service Name and Port Registry](https://www.iana.org/assignments/service-names-port-numbers/service-names-port-numbers.xhtml) — authoritative port-to-service mapping for banner correlation and well-known port identification.
