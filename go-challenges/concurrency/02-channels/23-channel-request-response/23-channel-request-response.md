# Exercise 23: Channel-Based Request-Response (RPC Pattern)

In most Go services shared state hides behind a `sync.Mutex`. That works, but as
operations multiply the lock serializes everything anyway, and it scatters
locking concerns across every caller -- one missed `Lock` is a data race, one
misplaced `defer` is a deadlock. This exercise builds the alternative: a
key-value store that runs as a single goroutine owning all the data, where every
other goroutine talks to it through typed requests and per-request reply
channels. No caller ever touches the map, so there is nothing to lock
incorrectly.

## What you'll build

```text
23-channel-request-response/
  main.go        a single-owner KV server, typed Request/Response structs
                 with embedded reply channels, concurrent clients, and a
                 done channel reporting server stats
```

- Build: a channel-based key-value store where one goroutine owns the map -- a lock-free RPC pattern.
- Implement: typed `Request`/`Response` structs with an embedded reply channel, `kvServer` handling Set/Get/Delete, `clientWorker`, and a `done` channel returning `ServerStats`.
- Verify: `go run -race main.go`.

### Why a single owner beats scattered locks

The channel-based RPC pattern inverts this: a single goroutine owns the state and is the only one that ever reads or writes it. All other goroutines communicate with it by sending typed requests on a channel and waiting for responses on a per-request reply channel. The owner goroutine processes requests sequentially, which serializes access naturally -- no mutex needed.

This is the pattern Rob Pike describes as "don't communicate by sharing memory; share memory by communicating." It is used internally by `net/http`'s server, many database connection pools, and service meshes. The tradeoff is explicit: slightly more ceremony in request/response structs, but the data owner is obvious, testable in isolation, and impossible to access incorrectly.

## Step 1 -- Single Operation: Set and Reply

Start with the simplest possible version: one client sends a Set request, the server processes it, and replies on the embedded channel.

```go
package main

import (
	"fmt"
	"time"
)

const serverShutdownGrace = 100 * time.Millisecond

// OpType identifies the kind of operation requested.
type OpType int

const (
	OpSet OpType = iota
	OpGet
	OpDelete
)

// Request represents a typed operation sent to the KV server.
type Request struct {
	Op    OpType
	Key   string
	Value string
	Reply chan Response
}

// Response carries the result of a single KV operation.
type Response struct {
	Value string
	Found bool
	Error string
}

// NewSetRequest creates a Set request with a buffered reply channel.
func NewSetRequest(key, value string) Request {
	return Request{
		Op:    OpSet,
		Key:   key,
		Value: value,
		Reply: make(chan Response, 1),
	}
}

// kvServer runs in its own goroutine, owning all data.
func kvServer(requests <-chan Request) {
	store := make(map[string]string)
	for req := range requests {
		switch req.Op {
		case OpSet:
			store[req.Key] = req.Value
			req.Reply <- Response{Value: req.Value, Found: true}
		}
	}
}

func main() {
	requests := make(chan Request, 10)
	go kvServer(requests)

	// Client sends a Set request and waits for confirmation.
	setReq := NewSetRequest("user:1", "alice")
	requests <- setReq
	resp := <-setReq.Reply
	fmt.Printf("SET user:1 -> %s (ok: %v)\n", resp.Value, resp.Found)

	close(requests)
	time.Sleep(serverShutdownGrace)
}
```

Key observations:
- The `store` map lives entirely inside `kvServer` -- no goroutine can touch it directly
- Each `Request` carries a `Reply` channel buffered to 1, so the server never blocks on sending
- The server's `for range` loop exits cleanly when `requests` is closed

### Verification
```bash
go run main.go
```
Expected output:
```
SET user:1 -> alice (ok: true)
```

## Step 2 -- Get and Delete with Error Handling

Extend the server to handle all three operations. Get returns the value if found; Delete removes the key and confirms.

```go
package main

import (
	"fmt"
	"time"
)

const serverGrace = 100 * time.Millisecond

type OpType int

const (
	OpSet OpType = iota
	OpGet
	OpDelete
)

type Request struct {
	Op    OpType
	Key   string
	Value string
	Reply chan Response
}

type Response struct {
	Value string
	Found bool
	Error string
}

func newRequest(op OpType, key, value string) Request {
	return Request{
		Op:    op,
		Key:   key,
		Value: value,
		Reply: make(chan Response, 1),
	}
}

func NewSetRequest(key, value string) Request { return newRequest(OpSet, key, value) }
func NewGetRequest(key string) Request        { return newRequest(OpGet, key, "") }
func NewDeleteRequest(key string) Request      { return newRequest(OpDelete, key, "") }

// kvServer processes all operations sequentially -- no mutex needed.
func kvServer(requests <-chan Request) {
	store := make(map[string]string)
	for req := range requests {
		switch req.Op {
		case OpSet:
			store[req.Key] = req.Value
			req.Reply <- Response{Value: req.Value, Found: true}
		case OpGet:
			val, ok := store[req.Key]
			if ok {
				req.Reply <- Response{Value: val, Found: true}
			} else {
				req.Reply <- Response{Found: false, Error: fmt.Sprintf("key %q not found", req.Key)}
			}
		case OpDelete:
			if _, ok := store[req.Key]; ok {
				delete(store, req.Key)
				req.Reply <- Response{Found: true}
			} else {
				req.Reply <- Response{Found: false, Error: fmt.Sprintf("key %q not found", req.Key)}
			}
		}
	}
}

// sendAndPrint sends a request and prints the response.
func sendAndPrint(label string, req Request, requests chan<- Request) {
	requests <- req
	resp := <-req.Reply
	if resp.Error != "" {
		fmt.Printf("%-20s error: %s\n", label, resp.Error)
	} else {
		fmt.Printf("%-20s value: %q  found: %v\n", label, resp.Value, resp.Found)
	}
}

func main() {
	requests := make(chan Request, 10)
	go kvServer(requests)

	sendAndPrint("SET user:1", NewSetRequest("user:1", "alice"), requests)
	sendAndPrint("SET user:2", NewSetRequest("user:2", "bob"), requests)
	sendAndPrint("GET user:1", NewGetRequest("user:1"), requests)
	sendAndPrint("GET user:99", NewGetRequest("user:99"), requests)
	sendAndPrint("DELETE user:2", NewDeleteRequest("user:2"), requests)
	sendAndPrint("GET user:2", NewGetRequest("user:2"), requests)

	close(requests)
	time.Sleep(serverGrace)
}
```

The server handles every operation inside a single `switch`. Because requests arrive sequentially on the channel, the map is always consistent -- a Get after a Set always sees the value.

### Verification
```bash
go run main.go
```
Expected output:
```
SET user:1           value: "alice"  found: true
SET user:2           value: "bob"  found: true
GET user:1           value: "alice"  found: true
GET user:99          error: key "user:99" not found
DELETE user:2        value: ""  found: true
GET user:2           error: key "user:2" not found
```

## Step 3 -- Multiple Concurrent Clients

Launch 10 client goroutines that perform Set then Get operations concurrently. The server serializes all access naturally.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const clientCount = 10

type OpType int

const (
	OpSet OpType = iota
	OpGet
	OpDelete
)

type Request struct {
	Op    OpType
	Key   string
	Value string
	Reply chan Response
}

type Response struct {
	Value string
	Found bool
	Error string
}

func newRequest(op OpType, key, value string) Request {
	return Request{
		Op:    op,
		Key:   key,
		Value: value,
		Reply: make(chan Response, 1),
	}
}

func NewSetRequest(key, value string) Request { return newRequest(OpSet, key, value) }
func NewGetRequest(key string) Request        { return newRequest(OpGet, key, "") }
func NewDeleteRequest(key string) Request      { return newRequest(OpDelete, key, "") }

func kvServer(requests <-chan Request) {
	store := make(map[string]string)
	for req := range requests {
		switch req.Op {
		case OpSet:
			store[req.Key] = req.Value
			req.Reply <- Response{Value: req.Value, Found: true}
		case OpGet:
			val, ok := store[req.Key]
			if ok {
				req.Reply <- Response{Value: val, Found: true}
			} else {
				req.Reply <- Response{Found: false, Error: fmt.Sprintf("key %q not found", req.Key)}
			}
		case OpDelete:
			if _, ok := store[req.Key]; ok {
				delete(store, req.Key)
				req.Reply <- Response{Found: true}
			} else {
				req.Reply <- Response{Found: false, Error: fmt.Sprintf("key %q not found", req.Key)}
			}
		}
	}
}

// clientWorker performs a Set followed by a Get for verification.
func clientWorker(id int, requests chan<- Request, wg *sync.WaitGroup) {
	defer wg.Done()

	key := fmt.Sprintf("session:%d", id)
	value := fmt.Sprintf("data-for-client-%d", id)

	// Set the value.
	setReq := NewSetRequest(key, value)
	requests <- setReq
	setResp := <-setReq.Reply

	// Get it back to verify.
	getReq := NewGetRequest(key)
	requests <- getReq
	getResp := <-getReq.Reply

	if getResp.Found && getResp.Value == value {
		fmt.Printf("client %2d: SET %s -> confirmed (set: %v, get: %q)\n",
			id, key, setResp.Found, getResp.Value)
	} else {
		fmt.Printf("client %2d: MISMATCH expected %q got %q\n",
			id, value, getResp.Value)
	}
}

func main() {
	requests := make(chan Request, 50)
	go kvServer(requests)

	var wg sync.WaitGroup
	epoch := time.Now()

	for i := 1; i <= clientCount; i++ {
		wg.Add(1)
		go clientWorker(i, requests, &wg)
	}

	wg.Wait()
	close(requests)

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("\n%d clients completed in %v (no mutex, no races)\n", clientCount, elapsed)
}
```

Even though 10 goroutines send requests concurrently, every operation is serialized through the server's channel. The `store` map is never accessed by more than one goroutine.

### Verification
```bash
go run -race main.go
```
Expected output (order varies):
```
client  3: SET session:3 -> confirmed (set: true, get: "data-for-client-3")
client  1: SET session:1 -> confirmed (set: true, get: "data-for-client-1")
...
10 clients completed in Xms (no mutex, no races)
```
The `-race` flag confirms no data races.

## Step 4 -- Clean Shutdown with Done Channel

Add a `done` channel so the server reports how many operations it processed before shutting down. Clients perform Set, Get, and Delete operations in sequence.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const finalClientCount = 10

type OpType int

const (
	OpSet OpType = iota
	OpGet
	OpDelete
)

type Request struct {
	Op    OpType
	Key   string
	Value string
	Reply chan Response
}

type Response struct {
	Value string
	Found bool
	Error string
}

// ServerStats reports what the server processed.
type ServerStats struct {
	Sets    int
	Gets    int
	Deletes int
}

func newRequest(op OpType, key, value string) Request {
	return Request{
		Op:    op,
		Key:   key,
		Value: value,
		Reply: make(chan Response, 1),
	}
}

func NewSetRequest(key, value string) Request { return newRequest(OpSet, key, value) }
func NewGetRequest(key string) Request        { return newRequest(OpGet, key, "") }
func NewDeleteRequest(key string) Request      { return newRequest(OpDelete, key, "") }

// kvServer runs until requests is closed, then sends stats on done.
func kvServer(requests <-chan Request, done chan<- ServerStats) {
	store := make(map[string]string)
	var stats ServerStats

	for req := range requests {
		switch req.Op {
		case OpSet:
			store[req.Key] = req.Value
			req.Reply <- Response{Value: req.Value, Found: true}
			stats.Sets++
		case OpGet:
			val, ok := store[req.Key]
			if ok {
				req.Reply <- Response{Value: val, Found: true}
			} else {
				req.Reply <- Response{Found: false, Error: fmt.Sprintf("key %q not found", req.Key)}
			}
			stats.Gets++
		case OpDelete:
			if _, ok := store[req.Key]; ok {
				delete(store, req.Key)
				req.Reply <- Response{Found: true}
			} else {
				req.Reply <- Response{Found: false, Error: fmt.Sprintf("key %q not found", req.Key)}
			}
			stats.Deletes++
		}
	}

	done <- stats
}

func clientWorker(id int, requests chan<- Request, wg *sync.WaitGroup) {
	defer wg.Done()

	key := fmt.Sprintf("session:%d", id)
	value := fmt.Sprintf("token-%d", id)

	// Set.
	setReq := NewSetRequest(key, value)
	requests <- setReq
	<-setReq.Reply

	// Get and verify.
	getReq := NewGetRequest(key)
	requests <- getReq
	getResp := <-getReq.Reply

	// Delete.
	delReq := NewDeleteRequest(key)
	requests <- delReq
	<-delReq.Reply

	// Verify deletion.
	getReq2 := NewGetRequest(key)
	requests <- getReq2
	getResp2 := <-getReq2.Reply

	fmt.Printf("client %2d: set=%q -> get=%q -> delete -> exists=%v\n",
		id, value, getResp.Value, getResp2.Found)
}

func main() {
	requests := make(chan Request, 100)
	done := make(chan ServerStats, 1)

	go kvServer(requests, done)

	var wg sync.WaitGroup
	epoch := time.Now()

	for i := 1; i <= finalClientCount; i++ {
		wg.Add(1)
		go clientWorker(i, requests, &wg)
	}

	wg.Wait()
	close(requests)

	stats := <-done
	elapsed := time.Since(epoch).Round(time.Millisecond)

	fmt.Printf("\n=== Server Stats ===\n")
	fmt.Printf("SETs:    %d\n", stats.Sets)
	fmt.Printf("GETs:    %d\n", stats.Gets)
	fmt.Printf("DELETEs: %d\n", stats.Deletes)
	fmt.Printf("Total:   %d ops in %v\n", stats.Sets+stats.Gets+stats.Deletes, elapsed)
}
```

### Verification
```bash
go run -race main.go
```
Expected output (order varies):
```
client  2: set="token-2" -> get="token-2" -> delete -> exists=false
client  5: set="token-5" -> get="token-5" -> delete -> exists=false
...

=== Server Stats ===
SETs:    10
GETs:    20
DELETEs: 10
Total:   40 ops in Xms
```
Each client does 4 operations (set, get, delete, get), so 10 clients produce 40 total.

## Common Mistakes

### Unbuffered Reply Channel
**What happens:** The server blocks on `req.Reply <- resp` until the client reads. If the client has not started reading yet (or if multiple requests queue up), the server stalls and stops processing all other requests.
**Fix:** Always buffer the reply channel with capacity 1: `Reply: make(chan Response, 1)`. The server sends and immediately moves to the next request.

### Accessing the Map Outside the Server Goroutine
**What happens:** A developer adds a "quick read" that accesses the store map directly from a client goroutine, bypassing the channel. This creates a data race that `go run -race` detects -- or worse, causes silent corruption.
**Fix:** The map must never be referenced outside `kvServer`. Every access goes through a Request. If you need bulk reads, add a new OpType (e.g., `OpList`) that the server handles internally.

### Forgetting to Close the Request Channel
**What happens:** The server's `for range` loop blocks forever waiting for more requests. The program hangs on shutdown -- the `done` channel never receives stats.
**Fix:** Close the `requests` channel after all clients complete. Use `sync.WaitGroup` to coordinate: `wg.Wait(); close(requests)`.

## Review

The pattern reduces to one rule: a single goroutine owns the map, and every
mutation and read arrives as a typed request on a shared channel. Because the
owner processes requests one at a time, access is serialized for free -- no
mutex, and `go run -race` stays quiet no matter how many clients push
concurrently. Each `Request` carries its own reply channel buffered to one, so
the server sends the response and immediately moves to the next request instead
of blocking on a client that has not read yet. A `done` channel closes the loop
on shutdown, letting the server hand back its `ServerStats` once `requests` is
closed and the range loop drains.

To check that the ownership boundary is real, add an `OpList` operation that
returns every key as a comma-separated string: the server iterates the map
internally and replies on the request's channel, exactly like the other
operations, and no client ever ranges over the map itself. Confirm that five
clients can list keys while others set and delete concurrently and the race
detector still finds nothing -- if it does, some code path reached the map
without going through a request.

## Resources
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the single-owner philosophy this KV server implements.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- send, receive, buffering, and closing: the mechanics the server loop depends on.
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the talk that popularized request-response over a channel.
- [Go Wiki: MutexOrChannel](https://go.dev/wiki/MutexOrChannel) -- when this pattern earns its ceremony versus reaching for a mutex.

---

Back to [Concurrency](../../concurrency.md) | Next: [24-channel-retry-backoff](../24-channel-retry-backoff/24-channel-retry-backoff.md)
