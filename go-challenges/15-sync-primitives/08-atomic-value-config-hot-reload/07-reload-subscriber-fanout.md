# Exercise 7: Reload Fan-Out: Rebuilding Dependent Components on Config Change

Not every consumer can absorb a config change by reading the next snapshot:
an `http.Client`'s timeout, a logger's level, a listener's TLS material were
baked in at construction and must be *rebuilt*. This exercise builds the
Subscribe/Unsubscribe fan-out that tells them when — with the one iron rule
of publisher design: a slow or stuck subscriber must never block a reload.

This module is fully self-contained: its own `go mod init`, every type it
needs, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cfgfanout/                 independent module: example.com/cfgfanout
  go.mod
  manager.go               Manager: Get/Update + Subscribe/Unsubscribe registry,
                           coalescing non-blocking notify; RunSubscriber loop helper
  client.go                ClientHolder: an http.Client rebuilt from the snapshot
  cmd/
    demo/
      main.go              runnable demo: timeout change rebuilds the client live
  fanout_test.go           rebuild-on-update, stalled-subscriber coalescing, no-leak unsubscribe
```

- Files: `manager.go`, `client.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `Subscribe() (<-chan struct{}, func())` handing out a 1-buffered wakeup channel plus an unsubscribe func that closes it; `Update` storing the snapshot then notifying every subscriber with `select`/`default` (coalescing, never blocking); `RunSubscriber` looping rebuilds until unsubscribed; a `ClientHolder` rebuilding an `http.Client` from the snapshot's timeout.
- Test: the rebuilt client reflects a new `TimeoutMillis` after `Update`; a deliberately-stalled subscriber proves `Update` completes and pending wakeups coalesce to exactly one; unsubscribe terminates the subscriber goroutine, observed via a done channel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cfgfanout/cmd/demo
cd ~/go-exercises/cfgfanout
go mod init example.com/cfgfanout
```

### Events say "changed", snapshots say what changed to

The fan-out's design hinges on separating two channels of information. The
notification carries *no payload* — it is a bare `struct{}{}` meaning "the
config changed, go look". The subscriber then reads the *current* snapshot
with `Get()` and rebuilds from that. This is deliberate: if events carried
config payloads, a subscriber that missed events (and with coalescing, slow
subscribers do miss intermediate ones) would rebuild from a stale payload.
With look-at-current semantics, missing nine of ten notifications is
harmless — the tenth rebuild reads the latest snapshot, which is the only
one that matters. Coalescing is not a compromise; it is the correct
semantics for "reconcile to current state".

The mechanism is a channel-per-subscriber, buffered to exactly 1, notified
with a non-blocking send:

```
select {
case ch <- struct{}{}:
default: // subscriber already has a pending wakeup; coalesce
}
```

Walk the states. Subscriber idle, buffer empty: the send succeeds, the
subscriber wakes. Subscriber busy rebuilding, buffer empty: the send parks
one wakeup it will consume next loop. Subscriber stuck, buffer full: the
`default` arm fires and `Update` moves on instantly. The publisher's latency
is bounded by the number of subscribers, never by their behavior — the
property that keeps one wedged component from freezing config rollout for
the whole process.

Lifecycle is the remaining edge. `Subscribe` returns the channel and an
unsubscribe func; unsubscribe removes the channel from the registry *and
closes it*, which makes the subscriber's `for range ch` loop terminate — a
clean goroutine shutdown with no extra stop channel. Registry mutation and
notification both hold the registry mutex, which serializes any `Update`
against any unsubscribe: a send on a closed channel (a panic) is impossible
because the channel is removed from the map before it is closed, under the
same lock the notifier holds. The mutex here guards only the small registry
map — the config itself stays on the atomic pointer, so readers on the hot
path still take no lock.

Create `manager.go`:

```go
// Package cfgfanout publishes immutable config snapshots and fans out
// coalescing change notifications to subscribers that must rebuild
// components (HTTP clients, loggers) rather than merely re-read fields.
package cfgfanout

import (
	"sync"
	"sync/atomic"
)

// Config is one immutable configuration snapshot.
type Config struct {
	TimeoutMillis int
	LogLevel      string
	Version       int
}

// Manager holds the current snapshot and the subscriber registry. Share
// by pointer; do not copy after first use.
type Manager struct {
	ptr atomic.Pointer[Config]

	mu     sync.Mutex // guards subs and nextID only, never the hot read path
	subs   map[int]chan struct{}
	nextID int
}

// NewManager returns a Manager serving initial with no subscribers.
func NewManager(initial *Config) *Manager {
	m := &Manager{subs: make(map[int]chan struct{})}
	m.ptr.Store(initial)
	return m
}

// Get returns the current snapshot (read-only, non-nil, lock-free).
func (m *Manager) Get() *Config {
	return m.ptr.Load()
}

// Subscribe registers for change notifications. The returned channel
// receives at most one pending wakeup (coalescing); on each receive the
// subscriber must re-read Get() and rebuild from the current snapshot.
// The returned func unsubscribes and closes the channel, terminating a
// RunSubscriber loop ranging over it.
func (m *Manager) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	m.subs[id] = ch
	m.mu.Unlock()

	unsubscribe := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.subs[id]; !ok {
			return // already unsubscribed
		}
		delete(m.subs, id)
		close(ch)
	}
	return ch, unsubscribe
}

// Update atomically replaces the snapshot, then notifies every
// subscriber without blocking: a subscriber with a wakeup already
// pending is skipped (it will see this change when it re-reads Get).
func (m *Manager) Update(next *Config) {
	m.ptr.Store(next)

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default: // coalesce: one pending wakeup is enough
		}
	}
}

// RunSubscriber calls rebuild once per wakeup and returns when the
// subscription is closed by its unsubscribe func. Run it on its own
// goroutine; rebuild must read Get() itself so it always sees the
// current snapshot, not a remembered event payload.
func RunSubscriber(ch <-chan struct{}, rebuild func()) {
	for range ch {
		rebuild()
	}
}
```

Create `client.go`:

```go
package cfgfanout

import (
	"net/http"
	"sync/atomic"
	"time"
)

// ClientHolder owns an http.Client rebuilt from config snapshots. The
// client itself is immutable once published; request paths Load it and
// use it, and a reload swaps in a freshly built one.
type ClientHolder struct {
	ptr atomic.Pointer[http.Client]
}

// NewClientHolder builds the initial client from cfg.
func NewClientHolder(cfg *Config) *ClientHolder {
	h := &ClientHolder{}
	h.Rebuild(cfg)
	return h
}

// Client returns the current client for issuing requests.
func (h *ClientHolder) Client() *http.Client {
	return h.ptr.Load()
}

// Rebuild constructs a new client from cfg and publishes it.
func (h *ClientHolder) Rebuild(cfg *Config) {
	h.ptr.Store(&http.Client{
		Timeout: time.Duration(cfg.TimeoutMillis) * time.Millisecond,
	})
}
```

### The runnable demo

The demo wires a `ClientHolder` to the fan-out, drops the configured timeout
from 5000 ms to 1000 ms, and shows the *rebuilt* client picking it up — then
unsubscribes and proves the subscriber goroutine exits.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/cfgfanout"
)

func main() {
	m := cfgfanout.NewManager(&cfgfanout.Config{TimeoutMillis: 5000, LogLevel: "info", Version: 1})
	holder := cfgfanout.NewClientHolder(m.Get())

	ch, unsubscribe := m.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cfgfanout.RunSubscriber(ch, func() { holder.Rebuild(m.Get()) })
	}()

	fmt.Printf("client timeout: %v\n", holder.Client().Timeout)

	m.Update(&cfgfanout.Config{TimeoutMillis: 1000, LogLevel: "debug", Version: 2})

	deadline := time.Now().Add(5 * time.Second)
	for holder.Client().Timeout != time.Second {
		if time.Now().After(deadline) {
			panic("rebuild never landed")
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Printf("client timeout after reload: %v\n", holder.Client().Timeout)

	unsubscribe()
	<-done
	fmt.Println("subscriber stopped")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
client timeout: 5s
client timeout after reload: 1s
subscriber stopped
```

### Tests

`TestSubscriberRebuildsClient` is the end-to-end path: update lands, wakeup
fires, rebuild reads the snapshot, the client's timeout changes — asserted
by polling with a deadline since the rebuild is asynchronous.
`TestStalledSubscriberNeverBlocksUpdate` registers a subscriber that never
drains and hammers `Update` 100 times: every call must complete, and the
stalled channel must hold exactly one coalesced wakeup.
`TestUnsubscribeStopsSubscriber` closes the loop on lifecycle, and
`TestUnsubscribeIdempotent` pins that calling it twice cannot panic on a
double close.

Create `fanout_test.go`:

```go
package cfgfanout

import (
	"fmt"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSubscriberRebuildsClient(t *testing.T) {
	t.Parallel()

	m := NewManager(&Config{TimeoutMillis: 5000, Version: 1})
	holder := NewClientHolder(m.Get())

	ch, unsubscribe := m.Subscribe()
	defer unsubscribe()
	go RunSubscriber(ch, func() { holder.Rebuild(m.Get()) })

	if got := holder.Client().Timeout; got != 5*time.Second {
		t.Fatalf("initial client timeout = %v", got)
	}

	m.Update(&Config{TimeoutMillis: 1000, Version: 2})
	waitFor(t, "client rebuild", func() bool {
		return holder.Client().Timeout == time.Second
	})
}

func TestStalledSubscriberNeverBlocksUpdate(t *testing.T) {
	t.Parallel()

	m := NewManager(&Config{TimeoutMillis: 1, Version: 1})
	ch, unsubscribe := m.Subscribe()
	defer unsubscribe()
	// No goroutine drains ch: the subscriber is wedged.

	doneUpdating := make(chan struct{})
	go func() {
		defer close(doneUpdating)
		for v := 2; v <= 101; v++ {
			m.Update(&Config{TimeoutMillis: v, Version: v})
		}
	}()

	select {
	case <-doneUpdating:
	case <-time.After(5 * time.Second):
		t.Fatal("Update blocked on a stalled subscriber")
	}

	if got := len(ch); got != 1 {
		t.Fatalf("pending wakeups = %d, want exactly 1 (coalesced)", got)
	}

	// The single coalesced wakeup still leads the subscriber to the
	// latest snapshot: that is the look-at-current contract.
	<-ch
	if got := m.Get().Version; got != 101 {
		t.Fatalf("current Version = %d, want 101", got)
	}
	if got := len(ch); got != 0 {
		t.Fatalf("wakeups left after drain = %d, want 0", got)
	}
}

func TestAllSubscribersNotified(t *testing.T) {
	t.Parallel()

	m := NewManager(&Config{TimeoutMillis: 1, Version: 1})
	ch1, unsub1 := m.Subscribe()
	defer unsub1()
	ch2, unsub2 := m.Subscribe()
	defer unsub2()

	m.Update(&Config{TimeoutMillis: 2, Version: 2})

	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d never notified", i+1)
		}
	}
}

func TestUnsubscribeStopsSubscriber(t *testing.T) {
	t.Parallel()

	m := NewManager(&Config{TimeoutMillis: 1, Version: 1})
	ch, unsubscribe := m.Subscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunSubscriber(ch, func() {})
	}()

	unsubscribe()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber goroutine leaked after unsubscribe")
	}

	// Updates after unsubscribe must not panic (no send on closed chan)
	// and must not notify the departed subscriber.
	m.Update(&Config{TimeoutMillis: 2, Version: 2})
}

func TestUnsubscribeIdempotent(t *testing.T) {
	t.Parallel()

	m := NewManager(&Config{TimeoutMillis: 1, Version: 1})
	_, unsubscribe := m.Subscribe()
	unsubscribe()
	unsubscribe() // second call must be a no-op, not a double close
}

func ExampleManager_Subscribe() {
	m := NewManager(&Config{TimeoutMillis: 5000, Version: 1})
	ch, unsubscribe := m.Subscribe()
	defer unsubscribe()

	m.Update(&Config{TimeoutMillis: 1000, Version: 2})
	<-ch // wakeup: re-read the current snapshot
	fmt.Println(m.Get().TimeoutMillis)
	// Output: 1000
}
```

## Review

The property to verify is asymmetric isolation: subscribers depend on the
publisher, the publisher depends on nobody. `TestStalledSubscriberNeverBlocksUpdate`
is the proof, and its second half is just as important as its first — the one
coalesced wakeup leads to version 101, demonstrating why payload-free
notifications plus re-read-current makes missed events harmless. If you find
yourself wanting to put the new `Config` *in* the notification, that is the
moment you have broken the design: a coalescing channel that drops payloads
drops information; one that drops bare wakeups drops nothing.

The lifecycle discipline deserves a second look too. Closing the wakeup
channel inside unsubscribe is what lets `RunSubscriber` be a plain
`for range` with no select, no stop channel, and no leak — but it only works
because removal-from-map and close happen under the same mutex the notifier
holds, making send-on-closed-channel unrepresentable, and because
unsubscribe is idempotent (the map lookup guards the double close). Verify
with `go test -count=1 -race ./...`.

## Resources

- [The Go Programming Language Specification: select statements](https://go.dev/ref/spec#Select_statements) — the default-arm semantics behind non-blocking sends.
- [net/http: Client](https://pkg.go.dev/net/http#Client) — Timeout and why a client is rebuilt, not patched.
- [sync/atomic: Pointer](https://pkg.go.dev/sync/atomic#Pointer) — both the config snapshot and the rebuilt client are published this way.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-validated-cas-versioned-updates.md](06-validated-cas-versioned-updates.md) | Next: [08-feature-flag-http-middleware.md](08-feature-flag-http-middleware.md)
