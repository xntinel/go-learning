# 6. Building a Line-Based Protocol

TCP delivers bytes in order but has no concept of where one message ends and the next begins. The application layer must define that boundary—message framing. The simplest strategy is newline-delimited text: every line is one complete message. This lesson builds a fully functional key-value store over a line-based text protocol, covering the server, a client library, race-free concurrent access, and a test suite that validates the protocol end-to-end.

```text
linekv/
  go.mod
  store.go
  server.go
  client.go
  linekv_test.go
  cmd/demo/main.go
```

## Concepts

### Message Framing in TCP Streams

TCP is a stream protocol. A single `Write([]byte("SET k v\n"))` on the sender may arrive as two `Read` calls on the receiver: `"SET k "` then `"v\n"`. Conversely, two small writes may coalesce into one read. The receiver cannot know where one logical message ends without an explicit marker in the byte stream.

Newline framing uses the byte `0x0A` (`\n`) as the message terminator. The sender writes a newline at the end of every command; the receiver accumulates bytes until it sees a newline and then processes exactly that line. This strategy underlies SMTP, HTTP/1.x request headers, FTP control connections, and Redis RESP2 inline commands. It is human-readable and instantly debuggable with `nc` or `telnet`.

### bufio.Scanner and bufio.Writer

`bufio.NewScanner(conn)` wraps the connection in a buffered reader and splits the stream at newlines with its default `ScanLines` function. `ScanLines` strips both `\n` and trailing `\r`, so the handler receives clean text:

```go
scanner := bufio.NewScanner(conn)
for scanner.Scan() {
	line := scanner.Text() // no trailing \r or \n
	// process line
}
```

`bufio.NewWriter(conn)` accumulates small writes in a 4096-byte buffer. The critical rule: **call `Flush()` after every response**. Without it, the response bytes remain in the buffer and the client blocks waiting for data that never arrives over the wire. The pattern is:

```go
fmt.Fprintln(w, "+OK")
w.Flush() // send immediately
```

### Wire Format

Every message—command or response—ends with `\n`. The client sends commands; the server sends responses.

| Client sends | Server responds |
|---|---|
| `SET key value\n` | `+OK\n` |
| `GET key\n` | `$value\n` or `-ERR key not found\n` |
| `DEL key\n` | `+DELETED\n` or `-ERR key not found\n` |
| `KEYS\n` | one key per line, then `+END\n` |
| `QUIT\n` | `+BYE\n` then server closes connection |
| anything else | `-ERR unknown command\n` |

Prefix conventions: `+` is a simple status, `$` carries a value payload, `-ERR` signals an error. Parsing a command uses `strings.SplitN(line, " ", 3)` with a limit of 3, which keeps `value` intact when it contains spaces (for example, `SET greeting hello world` gives `["SET", "greeting", "hello world"]`). A plain `strings.Split` would split the value on its spaces and truncate it.

### Concurrent Connections and the Race Detector

Each accepted connection runs in its own goroutine. All goroutines share the same `Store`. Two concurrent reads are safe; a write concurrent with any other read or write is a data race. The fix is `sync.RWMutex`: callers acquire `RLock`/`RUnlock` for reads and `Lock`/`Unlock` for writes. Go's race detector (`go test -race`) catches violations that code review misses—run it in every test invocation.

In tests, bind to `"127.0.0.1:0"`. The kernel picks a free port and returns it via `listener.Addr().String()` after the listen succeeds. Never hardcode a port number in a test: two tests running concurrently would collide.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/linekv/cmd/demo
cd ~/go-exercises/linekv
go mod init example.com/linekv
```

There is no `main` in the library; verification is `go test`, not `go run`.

### Exercise 1: Thread-Safe Store

Create `store.go`:

```go
package linekv

import (
	"sort"
	"sync"
)

// Store is a thread-safe key-value store.
type Store struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewStore returns an empty, ready-to-use Store.
func NewStore() *Store {
	return &Store{data: make(map[string]string)}
}

// Set stores key -> value, overwriting any previous value.
func (s *Store) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

// Get returns the value for key and whether it was found.
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// Del removes key and reports whether it existed.
func (s *Store) Del(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	delete(s.data, key)
	return ok
}

// Keys returns all keys in sorted order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

`Del` checks existence and deletes under the same write lock. A check-then-delete split across two lock acquisitions would be a TOCTOU race.

### Exercise 2: Server

Create `server.go`:

```go
package linekv

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
)

// Server listens on a TCP address and speaks the linekv protocol.
type Server struct {
	store    *Store
	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

// NewServer returns a Server backed by a new empty Store.
func NewServer() *Server {
	return &Server{
		store: NewStore(),
		conns: make(map[net.Conn]struct{}),
	}
}

// Listen opens a TCP listener on addr.
// Use "127.0.0.1:0" to bind to a kernel-assigned free port,
// then call Addr() to read it back.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("linekv: listen: %w", err)
	}
	s.listener = ln
	return nil
}

// Addr returns the address the listener is bound to.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Serve accepts connections until Close is called.
// Call it in a goroutine: go srv.Serve().
func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener was closed
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

// Close shuts down the listener and all active connections.
func (s *Server) Close() error {
	s.mu.Lock()
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	scanner := bufio.NewScanner(conn)
	w := bufio.NewWriter(conn)

	for scanner.Scan() {
		line := scanner.Text() // ScanLines already strips \r and \n
		if line == "" {
			continue
		}
		// Limit 3 keeps "value with spaces" as a single field for SET.
		parts := strings.SplitN(line, " ", 3)
		cmd := strings.ToUpper(parts[0])

		switch cmd {
		case "SET":
			if len(parts) < 3 {
				fmt.Fprintln(w, "-ERR usage: SET key value")
			} else {
				s.store.Set(parts[1], parts[2])
				fmt.Fprintln(w, "+OK")
			}
		case "GET":
			if len(parts) < 2 {
				fmt.Fprintln(w, "-ERR usage: GET key")
			} else if val, ok := s.store.Get(parts[1]); ok {
				fmt.Fprintln(w, "$"+val)
			} else {
				fmt.Fprintln(w, "-ERR key not found")
			}
		case "DEL":
			if len(parts) < 2 {
				fmt.Fprintln(w, "-ERR usage: DEL key")
			} else if s.store.Del(parts[1]) {
				fmt.Fprintln(w, "+DELETED")
			} else {
				fmt.Fprintln(w, "-ERR key not found")
			}
		case "KEYS":
			for _, k := range s.store.Keys() {
				fmt.Fprintln(w, k)
			}
			fmt.Fprintln(w, "+END")
		case "QUIT":
			fmt.Fprintln(w, "+BYE")
			w.Flush()
			return
		default:
			fmt.Fprintln(w, "-ERR unknown command")
		}
		w.Flush()
	}
}
```

### Exercise 3: Client Library

Create `client.go`:

```go
package linekv

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrKeyNotFound is returned by Get and Del when the server reports the key
// does not exist. Callers can test for it with errors.Is.
var ErrKeyNotFound = errors.New("key not found")

// Client speaks the linekv protocol over a single TCP connection.
// A Client is not safe for concurrent use from multiple goroutines.
type Client struct {
	conn    net.Conn
	scanner *bufio.Scanner
	writer  *bufio.Writer
}

// Dial connects to the server at addr and returns a ready Client.
func Dial(addr string) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("linekv: dial %s: %w", addr, err)
	}
	return &Client{
		conn:    conn,
		scanner: bufio.NewScanner(conn),
		writer:  bufio.NewWriter(conn),
	}, nil
}

// Set stores key -> value.
func (c *Client) Set(key, value string) error {
	if err := c.send("SET %s %s", key, value); err != nil {
		return err
	}
	resp, err := c.readLine()
	if err != nil {
		return err
	}
	if resp != "+OK" {
		return fmt.Errorf("linekv: set: %s", resp)
	}
	return nil
}

// Get returns the value for key, or ErrKeyNotFound if the key does not exist.
func (c *Client) Get(key string) (string, error) {
	if err := c.send("GET %s", key); err != nil {
		return "", err
	}
	resp, err := c.readLine()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(resp, "$") {
		return resp[1:], nil
	}
	if strings.Contains(resp, "key not found") {
		return "", fmt.Errorf("linekv: get %s: %w", key, ErrKeyNotFound)
	}
	return "", fmt.Errorf("linekv: get: %s", resp)
}

// Del removes key from the store.
// Returns ErrKeyNotFound if the key did not exist.
func (c *Client) Del(key string) error {
	if err := c.send("DEL %s", key); err != nil {
		return err
	}
	resp, err := c.readLine()
	if err != nil {
		return err
	}
	if resp == "+DELETED" {
		return nil
	}
	if strings.Contains(resp, "key not found") {
		return fmt.Errorf("linekv: del %s: %w", key, ErrKeyNotFound)
	}
	return fmt.Errorf("linekv: del: %s", resp)
}

// Keys returns all keys from the store in sorted order.
func (c *Client) Keys() ([]string, error) {
	if err := c.send("KEYS"); err != nil {
		return nil, err
	}
	var keys []string
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		if line == "+END" {
			break
		}
		keys = append(keys, line)
	}
	return keys, nil
}

// Close sends QUIT and closes the underlying connection.
func (c *Client) Close() error {
	fmt.Fprint(c.writer, "QUIT\n")
	c.writer.Flush()
	return c.conn.Close()
}

// send writes a formatted command followed by \n and flushes the buffer.
func (c *Client) send(format string, args ...any) error {
	fmt.Fprintf(c.writer, format+"\n", args...)
	return c.writer.Flush()
}

func (c *Client) readLine() (string, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return "", fmt.Errorf("linekv: read: %w", err)
		}
		return "", fmt.Errorf("linekv: connection closed")
	}
	return c.scanner.Text(), nil
}
```

### Exercise 4: Tests and Example

Create `linekv_test.go`:

```go
package linekv

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// startTestServer starts a Server on a free port and registers cleanup.
// It returns the bound address as "host:port".
func startTestServer(t *testing.T) string {
	t.Helper()
	srv := NewServer()
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return srv.Addr()
}

func TestSetGet(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t)

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Set("color", "blue"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get("color")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "blue" {
		t.Fatalf("Get(color) = %q, want %q", got, "blue")
	}
}

func TestGetMissingKey(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t)

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	_, err = c.Get("missing")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get(missing) err = %v, want ErrKeyNotFound", err)
	}
}

func TestDel(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t)

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Set("x", "1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Del("x"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	_, err = c.Get("x")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get after Del err = %v, want ErrKeyNotFound", err)
	}
}

func TestDelMissingKey(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t)

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Del("nope"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Del(missing) err = %v, want ErrKeyNotFound", err)
	}
}

func TestSetValueWithSpaces(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t)

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Set("msg", "hello world"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get("msg")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("Get(msg) = %q, want %q", got, "hello world")
	}
}

func TestKeys(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t)

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	for _, kv := range [][2]string{{"b", "2"}, {"a", "1"}, {"c", "3"}} {
		if err := c.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	keys, err := c.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(keys) != len(want) {
		t.Fatalf("Keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("Keys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestConcurrentClients(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t)

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(id int) {
			defer wg.Done()
			c, err := Dial(addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			key := fmt.Sprintf("key%d", id)
			if err := c.Set(key, fmt.Sprintf("val%d", id)); err != nil {
				errs <- err
				return
			}
			if _, err := c.Get(key); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent error: %v", err)
	}
}

func ExampleClient_Set() {
	srv := NewServer()
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		panic(err)
	}
	go srv.Serve()
	defer srv.Close()

	c, err := Dial(srv.Addr())
	if err != nil {
		panic(err)
	}
	defer c.Close()

	if err := c.Set("lang", "go"); err != nil {
		panic(err)
	}
	val, err := c.Get("lang")
	if err != nil {
		panic(err)
	}
	fmt.Println(val)
	// Output: go
}
```

Your turn: add `TestSetOverwritesValue` that calls `Set` twice on the same key with different values and asserts `Get` returns the second value.

### cmd/demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/linekv"
)

func main() {
	srv := linekv.NewServer()
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		log.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	c, err := linekv.Dial(srv.Addr())
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	pairs := [][2]string{
		{"name", "Alice"},
		{"lang", "Go"},
		{"proto", "line-based"},
	}
	for _, p := range pairs {
		if err := c.Set(p[0], p[1]); err != nil {
			log.Fatalf("Set: %v", err)
		}
	}

	keys, err := c.Keys()
	if err != nil {
		log.Fatalf("Keys: %v", err)
	}
	for _, k := range keys {
		val, err := c.Get(k)
		if err != nil {
			log.Fatalf("Get: %v", err)
		}
		fmt.Printf("%s = %s\n", k, val)
	}

	if err := c.Del("lang"); err != nil {
		log.Fatalf("Del: %v", err)
	}
	fmt.Println("lang deleted")

	_, err = c.Get("lang")
	fmt.Printf("Get after Del: %v\n", err)
}
```

Run with: `go run ./cmd/demo`

## Common Mistakes

### Not Flushing the Writer After Every Response

Wrong: write the response with `fmt.Fprintln(w, "+OK")` and omit `w.Flush()`. The bytes sit in the 4096-byte buffer. The client's `scanner.Scan()` blocks waiting for the newline that never arrives over the wire.

Fix: call `w.Flush()` after every response, or after a batch of responses within one command (KEYS writes multiple lines and flushes once at the end).

### Using strings.Split Instead of strings.SplitN for SET

Wrong: `parts := strings.Split(line, " ")` with the command `SET greeting hello world`. This yields `["SET", "greeting", "hello", "world"]`. The server sees `parts[2]` as `"hello"` and discards `"world"`.

Fix: `strings.SplitN(line, " ", 3)` yields `["SET", "greeting", "hello world"]`. The third element is always the entire remainder of the line, preserving spaces in values.

### Reading from the Connection Directly Instead of bufio.Scanner

Wrong: `buf := make([]byte, 1024); n, _ := conn.Read(buf)`. A single `Read` may return a partial line, half of two lines, or nothing at all depending on network buffering. The code appears to work in unit tests and breaks under load.

Fix: wrap the connection in `bufio.NewScanner(conn)` and call `scanner.Scan()` in a loop. The scanner accumulates bytes internally until a full line is available, regardless of how many `Read` calls that requires.

### Hardcoding a Port in Tests

Wrong: `srv.Listen("127.0.0.1:9000")`. Two tests running concurrently bind to the same port; one fails with "address already in use".

Fix: bind to `"127.0.0.1:0"`. The kernel selects a free ephemeral port. Read it back with `srv.Addr()` after `Listen` returns.

## Verification

From `~/go-exercises/linekv`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector exercises the `sync.RWMutex` paths under `TestConcurrentClients`.

## Summary

- TCP is a byte stream; line-based protocols use `\n` as the message terminator.
- `bufio.Scanner` with the default `ScanLines` reads one complete line per `Scan()` call and strips trailing `\r\n`.
- `bufio.Writer` buffers writes; `Flush()` after every response is mandatory or the client hangs.
- `strings.SplitN(line, " ", 3)` keeps values with spaces intact for SET-style commands.
- `sync.RWMutex` protects the shared store: `RLock` for reads, `Lock` for writes.
- Bind to `"127.0.0.1:0"` in tests so the kernel picks a free port; read it back with `listener.Addr().String()`.
- Sentinel errors wrapped with `%w` let callers use `errors.Is` without string matching.

## What's Next

Next: [Connection Pooling Implementation](../07-connection-pooling-implementation/07-connection-pooling-implementation.md).

## Resources

- [bufio package](https://pkg.go.dev/bufio) — Scanner, Writer, ScanLines
- [net package](https://pkg.go.dev/net) — Listener, Conn, Dial, Listen
- [Redis Inline Commands (RESP2)](https://redis.io/docs/latest/develop/reference/protocol-spec/#inline-commands) — real-world example of line-based protocol over TCP
- [Go Blog: Concurrency is not parallelism](https://go.dev/blog/waza-talk) — background on goroutine-per-connection model
- [sync package](https://pkg.go.dev/sync#RWMutex) — RWMutex semantics
