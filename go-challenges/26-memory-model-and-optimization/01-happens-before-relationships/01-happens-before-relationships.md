# 1. Happens-Before Relationships

Happens-before is the rule that lets one goroutine rely on another goroutine's writes. This lesson builds a small concurrent key/value package that uses channels, `sync.Mutex`, and `sync.Once` to make visibility explicit instead of relying on timing.

```text
hbstore/
  go.mod
  store.go
  store_test.go
  cmd/demo/main.go
```

The package is a library. The demo is separate and imports only exported API.

## Concepts

### Happens-Before Is A Visibility Contract

The Go memory model defines happens-before as the transitive closure of sequencing inside a goroutine and synchronization between goroutines. If a write happens-before a read, the read is allowed to rely on seeing that write. If two ordinary accesses to the same variable are not ordered and at least one is a write, the program has a data race.

The important practical rule is simple: do not communicate by sleeping or by polling an ordinary variable. Communicate with synchronization. Channel sends, channel closes, mutex unlock/lock pairs, `sync.Once`, `sync.WaitGroup`, and atomic operations document where visibility is created.

### Channels Publish Data

A send on a channel is synchronized before the matching receive completes. Closing a channel is synchronized before a receive that observes the closed channel. That makes a channel useful for publishing a result: write the ordinary data first, then close or send on the channel, then read the ordinary data after receiving.

The lesson's `Publish` function writes a value in one goroutine and closes a channel. The receiving goroutine waits for the close before reading the result, so the write is visible without a race.

### Mutexes Protect Both Atomicity And Visibility

`sync.Mutex` is not just a mutual exclusion tool. An `Unlock` is synchronized before a later successful `Lock` on the same mutex. Code that writes a map while holding the lock publishes those writes to code that later locks the same mutex and reads the map.

The `Store` type keeps its map unexported. All reads and writes go through methods that lock the same mutex, so the map is both protected from concurrent mutation and made visible across goroutines.

### sync.Once Publishes Initialization

The completion of the function passed to `once.Do` is synchronized before any call to `once.Do` returns. That is why `sync.Once` is the usual tool for lazy initialization. A goroutine that receives the initialized value after `Do` returns can rely on the writes performed inside the initializer.

The package uses `Loader.Value` to initialize a value once. All callers either run the initializer or wait for it, and all callers observe the same initialized value.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/hbstore/cmd/demo
cd ~/go-exercises/hbstore
go mod init hbstore
```

### Exercise 1: Build The Synchronized Store

Create `store.go`:

```go
package hbstore

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	ErrEmptyKey  = errors.New("key must not be empty")
	ErrNilLoader = errors.New("loader must not be nil")
)

type Store struct {
	mu     sync.Mutex
	values map[string]string
}

func New(initial map[string]string) (*Store, error) {
	s := &Store{values: make(map[string]string, len(initial))}
	for key, value := range initial {
		if key == "" {
			return nil, fmt.Errorf("new store: %w", ErrEmptyKey)
		}
		s.values[key] = value
	}
	return s, nil
}

func (s *Store) Put(key, value string) error {
	if key == "" {
		return fmt.Errorf("put: %w", ErrEmptyKey)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

func (s *Store) Get(key string) (string, bool, error) {
	if key == "" {
		return "", false, fmt.Errorf("get: %w", ErrEmptyKey)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	return value, ok, nil
}

func (s *Store) Snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.values))
	for key := range s.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
```

`Store` never exposes the map. `Put`, `Get`, and `Snapshot` all use the same mutex, so each unlock publishes the map mutation to later lock holders.

### Exercise 2: Add Channel And Once Publication

Append to `store.go`:

```go
type Loader struct {
	once  sync.Once
	value string
	err   error
	load  func() (string, error)
}

func NewLoader(load func() (string, error)) (*Loader, error) {
	if load == nil {
		return nil, fmt.Errorf("new loader: %w", ErrNilLoader)
	}
	return &Loader{load: load}, nil
}

func (l *Loader) Value() (string, error) {
	l.once.Do(func() {
		l.value, l.err = l.load()
	})
	return l.value, l.err
}

func Publish(value string) <-chan string {
	ready := make(chan string, 1)
	go func() {
		ready <- value
		close(ready)
	}()
	return ready
}
```

`Publish` uses a channel send to publish `value`. `Loader.Value` uses `sync.Once` to publish the initialized fields to every caller.

### Exercise 3: Test The Contract

Create `store_test.go`:

```go
package hbstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNewRejectsEmptyKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		initial map[string]string
		wantErr error
	}{
		{name: "empty key", initial: map[string]string{"": "bad"}, wantErr: ErrEmptyKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.initial)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestStorePublishesThroughMutex(t *testing.T) {
	t.Parallel()

	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := s.Put(fmt.Sprintf("k%02d", id), "seen"); err != nil {
				t.Errorf("Put() error = %v", err)
			}
		}(i)
	}
	wg.Wait()

	keys := s.Snapshot()
	if len(keys) != 20 {
		t.Fatalf("len(keys) = %d, want 20", len(keys))
	}
	value, ok, err := s.Get("k19")
	if err != nil || !ok || value != "seen" {
		t.Fatalf("Get(k19) = %q, %v, %v", value, ok, err)
	}
}

func TestPutAndGetRejectEmptyKey(t *testing.T) {
	t.Parallel()

	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		call func() error
	}{
		{name: "put", call: func() error { return s.Put("", "value") }},
		{name: "get", call: func() error { _, _, err := s.Get(""); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(); !errors.Is(err, ErrEmptyKey) {
				t.Fatalf("err = %v, want ErrEmptyKey", err)
			}
		})
	}
}

func TestPublishUsesChannelOrdering(t *testing.T) {
	t.Parallel()

	got := <-Publish("ready")
	if got != "ready" {
		t.Fatalf("Publish() = %q, want ready", got)
	}
}

func TestLoaderRunsOnce(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := 0
	loader, err := NewLoader(func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return "production", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := loader.Value()
			if err != nil || value != "production" {
				t.Errorf("Value() = %q, %v", value, err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestNewLoaderRejectsNil(t *testing.T) {
	t.Parallel()

	if _, err := NewLoader(nil); !errors.Is(err, ErrNilLoader) {
		t.Fatalf("err = %v, want ErrNilLoader", err)
	}
}

func ExamplePublish() {
	fmt.Println(<-Publish("visible"))
	// Output: visible
}
```

### Exercise 4: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"hbstore"
)

func main() {
	store, err := hbstore.New(map[string]string{"mode": "initial"})
	if err != nil {
		log.Fatal(err)
	}
	if err := store.Put("mode", <-hbstore.Publish("production")); err != nil {
		log.Fatal(err)
	}
	value, ok, err := store.Get("mode")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("mode=%s ok=%v keys=%v\n", value, ok, store.Snapshot())
}
```

## Common Mistakes

### Using Sleep As Synchronization

Wrong: write a variable in a goroutine, call `time.Sleep`, then read the variable in another goroutine.

Fix: use a channel, mutex, `sync.Once`, `sync.WaitGroup`, or `sync/atomic`. `Publish` uses a channel so the receive is ordered after the send.

### Protecting Writes But Not Reads

Wrong: lock around `Put`, but read the map directly in `Get` because reads look harmless.

Fix: guard every access to shared mutable state with the same synchronization. `Store.Get` locks before reading the map.

### Assuming Once Only Prevents Duplicate Work

Wrong: treat `sync.Once` as only an execution counter and ignore visibility.

Fix: use it for lazy initialization when later callers must observe initialized fields. `Loader.Value` relies on the memory model guarantee that the initializer happens-before `Do` returns.

## Verification

Run this from `~/go-exercises/hbstore`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test: call `Put("region", "eu")`, then `Get("region")`, and assert that the value is visible. The test should fail if either method stops using the mutex.

## Summary

- Happens-before is the visibility relation that makes cross-goroutine reads reliable.
- Timing does not synchronize memory; channels, locks, `sync.Once`, wait groups, and atomics do.
- A mutex protects map mutation and publishes prior writes to later lock holders.
- `sync.Once` publishes initialized state to every caller after `Do` returns.
- The race detector is a verification tool, not a substitute for designing synchronization deliberately.

## What's Next

Next: [CPU Profiling with pprof](../02-cpu-profiling-with-pprof/02-cpu-profiling-with-pprof.md).

## Resources

- [The Go Memory Model](https://go.dev/ref/mem)
- [Data Race Detector](https://go.dev/doc/articles/race_detector)
- [sync package](https://pkg.go.dev/sync)
- [sync/atomic package](https://pkg.go.dev/sync/atomic)
