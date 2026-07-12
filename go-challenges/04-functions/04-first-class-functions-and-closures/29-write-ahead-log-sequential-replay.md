# Exercise 29: Write-Ahead Log Sequential Replay After Crash Recovery

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye el borde de un crash a mitad de escritura).

A server that keeps state only in memory loses everything the instant it
crashes. A write-ahead log fixes that by making every mutation durable
*before* it takes effect: `apply` appends the operation to a log file,
fsyncs it, and only then updates the in-memory map. On restart, `replay`
re-reads the log from the beginning and reapplies every entry in order,
deterministically rebuilding the exact state the process had before it
died — including tolerating a log whose very last line is truncated,
because that is exactly what a crash mid-write leaves behind.

## What you'll build

```text
write-ahead-log/             independent module: example.com/write-ahead-log
  go.mod                      go 1.24
  wal.go                      Entry, NewStore returns apply/get/replay/closeStore
  cmd/
    demo/
      main.go                  write ops, "restart", replay, verify recovered state
  wal_test.go                  table test: apply/get, replay after restart, corrupted tail
```

- Files: `wal.go`, `cmd/demo/main.go`, `wal_test.go`.
- Implement: `NewStore(path string) (apply func(op, key, value string) error, get func(key string) (string, bool), replay func() error, closeStore func() error, err error)`, closing over a mutex-guarded map and an open file handle.
- Test: `apply` then `get` round-trips set and delete; closing a store and opening a fresh one over the same file, then calling `replay`, recovers the exact prior state; a hand-written log file with a truncated final line still replays every entry before the truncation and stops cleanly at it.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Log first, mutate second

`NewStore` opens `path` once and returns four closures sharing that file
handle, a mutex, and a captured `state map[string]string`. `apply` does
three things in strict order: marshal the operation to JSON, append it to
the file and `Sync()` it to disk, and only after the fsync succeeds does it
apply the operation to the in-memory map. If the process crashes between
the fsync and the map mutation, the log already contains the operation, so
replaying it after restart reproduces the exact same map mutation — nothing
is lost, and nothing is applied to memory that was not first made durable.

`replay` seeks to the start of the file, discards the current in-memory
map, and reapplies every entry it can parse, in file order, using the same
`applyToState` helper `apply` uses — one code path decides what an
operation means, whether it is being applied live or being replayed from
disk. The interesting edge case is the trailing line: if the process died
in the middle of writing the last entry's JSON, that line will not parse.
Failing the whole recovery over one truncated tail would be worse than the
crash itself, so `replay` simply stops at the first line it cannot
unmarshal — everything before it, which by definition finished its fsync
before the crash, is still applied.

Create `wal.go`:

```go
// Package wal implements a minimal write-ahead log: every mutation is
// appended (and fsync'd) to a log file before it is applied to an in-memory
// map, so a crash between the two never applies an unlogged write, and
// replaying the log from the start deterministically rebuilds the map.
package wal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Entry is one logged operation.
type Entry struct {
	Op    string `json:"op"` // "set" or "delete"
	Key   string `json:"key"`
	Value string `json:"value"`
}

// NewStore opens (creating if needed) the WAL file at path and returns four
// closures sharing a private mutex-guarded map and file handle:
//
//   - apply appends the operation to the WAL, fsyncs it, and only then
//     mutates the in-memory map -- the log is durable before the state
//     change is visible.
//   - get reads the in-memory map.
//   - replay re-reads the WAL from the beginning and reapplies every
//     syntactically valid entry in order, rebuilding the map from scratch.
//     A truncated or corrupted final line -- the signature of a crash that
//     happened mid-write -- stops replay at that point instead of failing
//     the whole recovery; every entry before it is still applied.
//   - closeStore closes the underlying file.
func NewStore(path string) (
	apply func(op, key, value string) error,
	get func(key string) (string, bool),
	replay func() error,
	closeStore func() error,
	err error,
) {
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if openErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("wal: open %s: %w", path, openErr)
	}

	var mu sync.Mutex
	state := make(map[string]string)

	applyToState := func(e Entry) {
		switch e.Op {
		case "set":
			state[e.Key] = e.Value
		case "delete":
			delete(state, e.Key)
		}
	}

	apply = func(op, key, value string) error {
		mu.Lock()
		defer mu.Unlock()

		e := Entry{Op: op, Key: key, Value: value}
		line, marshalErr := json.Marshal(e)
		if marshalErr != nil {
			return fmt.Errorf("wal: marshal entry: %w", marshalErr)
		}
		if _, writeErr := f.Write(append(line, '\n')); writeErr != nil {
			return fmt.Errorf("wal: write entry: %w", writeErr)
		}
		if syncErr := f.Sync(); syncErr != nil {
			return fmt.Errorf("wal: fsync: %w", syncErr)
		}
		applyToState(e)
		return nil
	}

	get = func(key string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := state[key]
		return v, ok
	}

	replay = func() error {
		mu.Lock()
		defer mu.Unlock()

		if _, seekErr := f.Seek(0, 0); seekErr != nil {
			return fmt.Errorf("wal: seek to start: %w", seekErr)
		}
		state = make(map[string]string)

		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var e Entry
			if unmarshalErr := json.Unmarshal(line, &e); unmarshalErr != nil {
				// Truncated or corrupted trailing line: the process crashed
				// mid-write. Everything logged before this line is still
				// durable and already applied; stop here rather than fail
				// the whole recovery.
				break
			}
			applyToState(e)
		}
		return sc.Err()
	}

	closeStore = func() error {
		mu.Lock()
		defer mu.Unlock()
		return f.Close()
	}

	return apply, get, replay, closeStore, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/write-ahead-log"
)

func main() {
	dir, err := os.MkdirTemp("", "wal-demo")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "wal.log")

	apply, _, _, closeStore, err := wal.NewStore(path)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	apply("set", "counter", "1")
	apply("set", "counter", "2")
	apply("set", "temp", "scratch")
	apply("delete", "temp", "")
	closeStore()
	fmt.Println("wrote 4 operations, process 'crashes' here")

	// Simulate a restart: open a brand new store over the same file and
	// replay the WAL to rebuild state before serving any request.
	_, get, replay, closeStore2, err := wal.NewStore(path)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	if err := replay(); err != nil {
		fmt.Println("replay error:", err)
		return
	}
	counter, ok := get("counter")
	fmt.Printf("after recovery, counter = %q (found=%v)\n", counter, ok)
	_, ok = get("temp")
	fmt.Printf("after recovery, temp found=%v\n", ok)
	closeStore2()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wrote 4 operations, process 'crashes' here
after recovery, counter = "2" (found=true)
after recovery, temp found=false
```

### Tests

Create `wal_test.go`:

```go
package wal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyThenGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	apply, get, _, closeStore, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer closeStore()

	if err := apply("set", "k", "v1"); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if got, ok := get("k"); !ok || got != "v1" {
		t.Fatalf("get(k) = (%q, %v), want (v1, true)", got, ok)
	}

	if err := apply("set", "k", "v2"); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if got, ok := get("k"); !ok || got != "v2" {
		t.Fatalf("get(k) = (%q, %v), want (v2, true)", got, ok)
	}

	if err := apply("delete", "k", ""); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if _, ok := get("k"); ok {
		t.Fatal("get(k) found after delete, want not found")
	}
}

func TestReplayRecoversStateAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")

	apply, _, _, closeStore, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	apply("set", "counter", "1")
	apply("set", "counter", "2")
	apply("set", "temp", "scratch")
	apply("delete", "temp", "")
	if err := closeStore(); err != nil {
		t.Fatalf("closeStore() error = %v", err)
	}

	// Simulate a crash + restart: a fresh store over the same file.
	_, get, replay, closeStore2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() (reopen) error = %v", err)
	}
	defer closeStore2()

	if err := replay(); err != nil {
		t.Fatalf("replay() error = %v", err)
	}
	if got, ok := get("counter"); !ok || got != "2" {
		t.Fatalf("get(counter) after replay = (%q, %v), want (2, true)", got, ok)
	}
	if _, ok := get("temp"); ok {
		t.Fatal("get(temp) found after replay, want not found (deleted before crash)")
	}
}

func TestReplayIgnoresCorruptedTrailingEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")

	// Write a WAL file by hand: two valid entries followed by a truncated
	// line, simulating a crash mid-write of the third entry.
	content := `{"op":"set","key":"a","value":"1"}` + "\n" +
		`{"op":"set","key":"b","value":"2"}` + "\n" +
		`{"op":"set","key":"c","valu` // truncated, no closing quote/brace/newline

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, get, replay, closeStore, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer closeStore()

	if err := replay(); err != nil {
		t.Fatalf("replay() error = %v, want nil (corrupted tail is tolerated)", err)
	}
	if got, ok := get("a"); !ok || got != "1" {
		t.Fatalf("get(a) = (%q, %v), want (1, true)", got, ok)
	}
	if got, ok := get("b"); !ok || got != "2" {
		t.Fatalf("get(b) = (%q, %v), want (2, true)", got, ok)
	}
	if _, ok := get("c"); ok {
		t.Fatal("get(c) found, want not found (its entry was truncated)")
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`TestApplyThenGet` proves the live path — set, overwrite, and delete all
show up immediately in `get`. `TestReplayRecoversStateAfterRestart` is the
core contract: closing a store, opening a brand-new one over the same file
(exactly what a restarted process does), and calling `replay` reproduces the
identical map an in-process reader would have seen, including a key that
was deleted before the "crash." `TestReplayIgnoresCorruptedTrailingEntry` is
the edge case that separates a real WAL from a naive one: a hand-corrupted
final line must not abort recovery of everything that came before it — only
the truncated entry itself is lost, which is the correct outcome, since that
entry never finished its fsync.

## Resources

- [pkg.go.dev: os.File.Sync](https://pkg.go.dev/os#File.Sync) — the fsync call that makes each WAL entry durable before it is applied.
- [PostgreSQL docs: Write-Ahead Logging (WAL)](https://www.postgresql.org/docs/current/wal-intro.html) — the log-before-mutate discipline this exercise mirrors.
- [pkg.go.dev: bufio.Scanner](https://pkg.go.dev/bufio#Scanner) — line-by-line replay, and why a truncated final line simply fails `Scan`/`Unmarshal` instead of corrupting earlier lines.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-gradual-rollout-feature-variant-router.md](28-gradual-rollout-feature-variant-router.md) | Next: [30-gossip-broadcast-with-exponential-backoff.md](30-gossip-broadcast-with-exponential-backoff.md)
