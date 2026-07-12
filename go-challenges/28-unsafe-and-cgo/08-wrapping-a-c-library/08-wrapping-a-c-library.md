# 8. Wrapping a C Library

A production cgo wrapper is not a thin translation layer. It is a full Go abstraction that hides every unsafe operation, translates C error codes into Go error types, adds goroutine safety where the C library has none, and prevents use-after-free when callers forget to close the resource. The public API must expose only Go types; no `unsafe.Pointer` or `C.*` types leak through.

This lesson builds a complete wrapper around a custom C key-value store. You write the C library (an array-backed hash table), then build the Go `kvstore` package around it layer by layer: opaque handle, mutex, sentinel errors, finalizer, closed guard, and a comprehensive test suite. The resulting pattern — handle + mutex + finalizer + sentinel errors + closed guard — is identical to what go-sqlite3, go-libsass, and other serious cgo wrappers use.

```text
kvstore/
  go.mod
  kv.h       (C header)
  kv.c       (C implementation, placed in package dir for cgo)
  kvstore.go (Go wrapper)
  kvstore_test.go
  cmd/demo/main.go
```

## Concepts

### The Five Problems Every cgo Wrapper Must Solve

**1. Resource lifecycle.** C libraries do not have finalizers. A `kv_store*` from `kv_open()` must be freed by `kv_close()`. Your Go wrapper must ensure that happens even if the caller forgets. The solution is `runtime.SetFinalizer` for garbage-collected cleanup plus an explicit `Close()` for deterministic cleanup. Both paths must be idempotent.

**2. Error translation.** C returns integer error codes. Go callers expect `error` values. Translate each error code to a sentinel error (`var ErrNotFound = errors.New(...)`) so callers can use `errors.Is` rather than comparing integers.

**3. Thread safety.** C libraries are usually not goroutine-safe. Go programs are inherently concurrent. The wrapper must add a `sync.Mutex` that protects every call that touches the C handle.

**4. String handling.** `C.CString(s)` allocates a null-terminated copy of a Go string in C memory. The caller is responsible for calling `C.free` on it. Every method that passes a string to C must allocate and free in a matched pair, typically via `defer C.free(unsafe.Pointer(cKey))`.

**5. Use-after-free protection.** After `Close()`, the C handle is invalid. Calling `C.kv_get` on it is a segfault. The wrapper must check a closed flag before every C call and return `ErrClosed` instead.

### Finalizer Semantics

`runtime.SetFinalizer(obj, fn)` arranges for the runtime to call `fn(obj)` when `obj` becomes unreachable. Two caveats matter for cgo wrappers:

- The finalizer runs after an unpredictable GC cycle, not when the store goes out of scope. For resources with real cost (file descriptors, memory, database connections), always provide an explicit `Close()` and document that callers should use it.
- Finalizers prevent an object from being collected in the cycle where it first becomes unreachable. An object with a finalizer is collected one GC cycle later. For most wrappers, this is fine; for latency-sensitive high-frequency allocation patterns, it is not.

The closed guard (`atomic.Bool` or `sync.Once`) must prevent the finalizer from calling `kv_close` a second time if `Close()` was already called.

### The Opaque Handle Pattern

The C library's `kv_store*` must never be visible to callers. Embed it in an unexported field:

```go
type Store struct {
	mu     sync.Mutex
	handle *C.kv_store  // unexported; callers see only *Store
	closed atomic.Bool
}
```

This is the same approach SQLite's `*sql.DB` uses internally: the public type is opaque; the C handle is unreachable from outside the package.

## Exercises

Set up the module. This exercise requires `gcc` or `clang`:

```bash
mkdir -p go-solutions/28-unsafe-and-cgo/08-wrapping-a-c-library/08-wrapping-a-c-library/cmd/demo
cd go-solutions/28-unsafe-and-cgo/08-wrapping-a-c-library/08-wrapping-a-c-library
go env CGO_ENABLED   # must print 1
```

### Exercise 1: The C Library

Place the following files in `~/go-exercises/kvstore/`. cgo picks up any `.c` files in the same directory as the Go file that contains the `import "C"` comment.

Create `kv.h`:

```c
#ifndef KV_H
#define KV_H

#define KV_OK        0
#define KV_NOT_FOUND -1
#define KV_FULL      -2
#define KV_NULL_PTR  -3

typedef struct kv_store kv_store;

kv_store *kv_open(int capacity);
int       kv_put(kv_store *s, const char *key, const char *value);
int       kv_get(kv_store *s, const char *key, char *buf, int buflen);
int       kv_delete(kv_store *s, const char *key);
int       kv_count(kv_store *s);
void      kv_close(kv_store *s);
const char *kv_error_string(int code);

#endif
```

Create `kv.c`:

```c
#include "kv.h"
#include <stdlib.h>
#include <string.h>

typedef struct {
    char *key;
    char *value;
    int   occupied;
} kv_entry;

struct kv_store {
    kv_entry *entries;
    int       capacity;
    int       count;
};

kv_store *kv_open(int capacity) {
    if (capacity <= 0) return NULL;
    kv_store *s = calloc(1, sizeof(kv_store));
    if (!s) return NULL;
    s->entries = calloc(capacity, sizeof(kv_entry));
    if (!s->entries) { free(s); return NULL; }
    s->capacity = capacity;
    return s;
}

int kv_put(kv_store *s, const char *key, const char *value) {
    if (!s || !key || !value) return KV_NULL_PTR;
    /* Update existing entry. */
    for (int i = 0; i < s->capacity; i++) {
        if (s->entries[i].occupied && strcmp(s->entries[i].key, key) == 0) {
            char *v = strdup(value);
            if (!v) return KV_NULL_PTR;
            free(s->entries[i].value);
            s->entries[i].value = v;
            return KV_OK;
        }
    }
    /* Insert into first empty slot. */
    for (int i = 0; i < s->capacity; i++) {
        if (!s->entries[i].occupied) {
            s->entries[i].key = strdup(key);
            s->entries[i].value = strdup(value);
            if (!s->entries[i].key || !s->entries[i].value) {
                free(s->entries[i].key);
                free(s->entries[i].value);
                s->entries[i].key = s->entries[i].value = NULL;
                return KV_NULL_PTR;
            }
            s->entries[i].occupied = 1;
            s->count++;
            return KV_OK;
        }
    }
    return KV_FULL;
}

int kv_get(kv_store *s, const char *key, char *buf, int buflen) {
    if (!s || !key || !buf) return KV_NULL_PTR;
    for (int i = 0; i < s->capacity; i++) {
        if (s->entries[i].occupied && strcmp(s->entries[i].key, key) == 0) {
            int n = (int)strlen(s->entries[i].value);
            if (n >= buflen) return KV_NULL_PTR;
            memcpy(buf, s->entries[i].value, n + 1);
            return n;
        }
    }
    return KV_NOT_FOUND;
}

int kv_delete(kv_store *s, const char *key) {
    if (!s || !key) return KV_NULL_PTR;
    for (int i = 0; i < s->capacity; i++) {
        if (s->entries[i].occupied && strcmp(s->entries[i].key, key) == 0) {
            free(s->entries[i].key);
            free(s->entries[i].value);
            s->entries[i].key = s->entries[i].value = NULL;
            s->entries[i].occupied = 0;
            s->count--;
            return KV_OK;
        }
    }
    return KV_NOT_FOUND;
}

int kv_count(kv_store *s) {
    if (!s) return KV_NULL_PTR;
    return s->count;
}

void kv_close(kv_store *s) {
    if (!s) return;
    for (int i = 0; i < s->capacity; i++) {
        if (s->entries[i].occupied) {
            free(s->entries[i].key);
            free(s->entries[i].value);
        }
    }
    free(s->entries);
    free(s);
}

const char *kv_error_string(int code) {
    switch (code) {
    case KV_OK:        return "ok";
    case KV_NOT_FOUND: return "key not found";
    case KV_FULL:      return "store is full";
    case KV_NULL_PTR:  return "null pointer";
    default:           return "unknown error";
    }
}
```

### Exercise 2: The Go Wrapper

Create `kvstore.go`:

```go
package kvstore

/*
#cgo CFLAGS: -I.
#include "kv.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Sentinel errors map to the C library's error codes.
var (
	ErrNotFound  = errors.New("kvstore: key not found")
	ErrStoreFull = errors.New("kvstore: store is full")
	ErrClosed    = errors.New("kvstore: store is closed")
	ErrNullPtr   = errors.New("kvstore: null pointer from C library")
)

// Store is a goroutine-safe key-value store backed by a C library.
// Call Close when done; the finalizer is a safety net, not a substitute.
type Store struct {
	mu     sync.Mutex
	handle *C.kv_store
	closed atomic.Bool
}

// Open creates a new Store with the given capacity.
func Open(capacity int) (*Store, error) {
	h := C.kv_open(C.int(capacity))
	if h == nil {
		return nil, fmt.Errorf("kvstore: kv_open returned nil (capacity=%d)", capacity)
	}
	s := &Store{handle: h}
	runtime.SetFinalizer(s, (*Store).finalize)
	return s, nil
}

func (s *Store) finalize() {
	// Close is idempotent; safe to call from the finalizer.
	_ = s.Close()
}

func (s *Store) checkClosed() error {
	if s.closed.Load() {
		return ErrClosed
	}
	return nil
}

func codeToError(code C.int) error {
	switch int(code) {
	case 0:
		return nil
	case -1:
		return ErrNotFound
	case -2:
		return ErrStoreFull
	case -3:
		return ErrNullPtr
	default:
		return fmt.Errorf("kvstore: unknown C error code %d", int(code))
	}
}

// Put inserts or updates key with the given value.
func (s *Store) Put(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}

	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cVal := C.CString(value)
	defer C.free(unsafe.Pointer(cVal))

	code := C.kv_put(s.handle, cKey, cVal)
	return codeToError(code)
}

// Get returns the value for key. Returns ErrNotFound if the key does not exist.
func (s *Store) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return "", err
	}

	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))

	const bufSize = 4096
	buf := make([]byte, bufSize)
	n := C.kv_get(s.handle, cKey, (*C.char)(unsafe.Pointer(&buf[0])), C.int(bufSize))
	if n < 0 {
		return "", codeToError(n)
	}
	return string(buf[:int(n)]), nil
}

// Delete removes key from the store. Returns ErrNotFound if the key does not exist.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkClosed(); err != nil {
		return err
	}

	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))

	code := C.kv_delete(s.handle, cKey)
	return codeToError(code)
}

// Count returns the number of entries in the store.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return 0
	}
	return int(C.kv_count(s.handle))
}

// Close frees the underlying C store. It is idempotent and safe to call multiple times.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil // already closed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	C.kv_close(s.handle)
	s.handle = nil
	runtime.SetFinalizer(s, nil)
	return nil
}
```

### Exercise 3: Tests

Create `kvstore_test.go`:

```go
package kvstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestPutGet(t *testing.T) {
	t.Parallel()
	s, err := Open(16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Put("hello", "world"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "world" {
		t.Fatalf("Get = %q, want world", got)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	s, _ := Open(16)
	defer s.Close()

	_ = s.Put("k", "v1")
	_ = s.Put("k", "v2")
	got, _ := s.Get("k")
	if got != "v2" {
		t.Fatalf("Get after update = %q, want v2", got)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	s, _ := Open(16)
	defer s.Close()

	_, err := s.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s, _ := Open(16)
	defer s.Close()

	_ = s.Put("del", "gone")
	if err := s.Delete("del"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("del"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
}

func TestStoreFull(t *testing.T) {
	t.Parallel()
	s, _ := Open(2) // capacity 2
	defer s.Close()

	_ = s.Put("k1", "v1")
	_ = s.Put("k2", "v2")
	err := s.Put("k3", "v3") // must fail
	if !errors.Is(err, ErrStoreFull) {
		t.Fatalf("err = %v, want ErrStoreFull", err)
	}
}

func TestClosePreventsFurtherUse(t *testing.T) {
	t.Parallel()
	s, _ := Open(16)
	_ = s.Close()

	if _, err := s.Get("k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get on closed: err = %v, want ErrClosed", err)
	}
	if err := s.Put("k", "v"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put on closed: err = %v, want ErrClosed", err)
	}
	if err := s.Delete("k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete on closed: err = %v, want ErrClosed", err)
	}
}

func TestDoubleCloseNoPanic(t *testing.T) {
	t.Parallel()
	s, _ := Open(16)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Second Close must not panic.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCount(t *testing.T) {
	t.Parallel()
	s, _ := Open(16)
	defer s.Close()

	for i := 0; i < 5; i++ {
		_ = s.Put(fmt.Sprintf("k%d", i), "v")
	}
	if got := s.Count(); got != 5 {
		t.Fatalf("Count = %d, want 5", got)
	}
	_ = s.Delete("k2")
	if got := s.Count(); got != 4 {
		t.Fatalf("Count after delete = %d, want 4", got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	s, _ := Open(256)
	defer s.Close()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("key%d", id%16)
			_ = s.Put(key, fmt.Sprintf("val%d", id))
			_, _ = s.Get(key)
			if id%3 == 0 {
				_ = s.Delete(key)
			}
		}(i)
	}
	wg.Wait()
}

func BenchmarkPut(b *testing.B) {
	s, _ := Open(b.N + 1)
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Put(fmt.Sprintf("k%d", i), "value")
	}
}

func BenchmarkGet(b *testing.B) {
	s, _ := Open(1024)
	defer s.Close()
	_ = s.Put("bench", "value")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Get("bench")
	}
}
```

Your turn: add `TestDeleteMissingKey` that calls `s.Delete("nonexistent")` on an empty store and asserts `errors.Is(err, ErrNotFound)`.

### Exercise 4: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/kvstore"
)

func main() {
	s, err := kvstore.Open(16)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	_ = s.Put("language", "Go")
	_ = s.Put("topic", "cgo")
	_ = s.Put("chapter", "28")

	fmt.Printf("Count: %d\n", s.Count())

	for _, key := range []string{"language", "topic", "chapter", "missing"} {
		val, err := s.Get(key)
		switch {
		case err == nil:
			fmt.Printf("  %s = %q\n", key, val)
		case errors.Is(err, kvstore.ErrNotFound):
			fmt.Printf("  %s: not found\n", key)
		default:
			fmt.Fprintf(os.Stderr, "  %s: %v\n", key, err)
		}
	}

	_ = s.Delete("topic")
	fmt.Printf("Count after delete: %d\n", s.Count())
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Calling C After kv_close (Use-After-Free)

Wrong:

```go
s.Close()
s.Get("key") // s.handle is nil after Close; kv_get dereferences it → SIGSEGV
```

What happens: `kv_get(NULL, ...)` in C returns `KV_NULL_PTR`, but if the caller bypasses `checkClosed` or if the handle was already freed, accessing it is undefined behavior.

Fix: the `closed atomic.Bool` flag is checked at the top of every method. `CompareAndSwap(false, true)` in `Close` ensures exactly one path calls `C.kv_close`.

### C.CString Without C.free (Memory Leak)

Wrong:

```go
cKey := C.CString(key)
// forgot: defer C.free(unsafe.Pointer(cKey))
C.kv_get(s.handle, cKey, ...)
```

What happens: every call to `Put` or `Get` leaks one or two C heap allocations. Over millions of calls, this exhausts C memory.

Fix: always pair `C.CString` with `defer C.free(unsafe.Pointer(ptr))` immediately on the next line.

### Holding the Mutex Over a Blocking cgo Call

Wrong: not a mistake in this example (the mutex is held only over the cgo call, which is fast). But if the C function blocks (e.g., network I/O), holding the mutex blocks all other goroutines accessing the store.

Fix: for blocking C functions, release the mutex before the cgo call and reacquire it afterward, re-checking the closed flag.

### Registering a Finalizer on a Non-Pointer Type

Wrong:

```go
s := Store{...}              // value, not pointer
runtime.SetFinalizer(s, ...) // panics: SetFinalizer requires a pointer
```

Fix: `Open` returns `*Store`, and `runtime.SetFinalizer(s, (*Store).finalize)` where `s` is `*Store`. Finalizers require pointer receivers.

### Passing a Go Pointer Inside a C Struct to Another cgo Call

Wrong:

```go
type GoStruct struct { Ptr *SomeThing }
gs := &GoStruct{Ptr: &SomeThing{}}
C.some_func((*C.GoStruct)(unsafe.Pointer(gs))) // C stores gs.Ptr, then calls back into Go
```

What happens: the cgo pointer rules prohibit C code from holding a pointer to Go memory across cgo calls. The GC may move `SomeThing` while C holds the pointer.

Fix: never let C hold a pointer to Go heap memory across cgo call boundaries. Use C memory (via `C.malloc`) for data that C needs to retain.

## Verification

From `~/go-exercises/kvstore`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=Benchmark -benchmem ./...
```

All five must pass. The race detector must report zero races for `TestConcurrentAccess`. The benchmarks show the cgo overhead per call: `BenchmarkGet` will be in the hundreds of nanoseconds, demonstrating that the wrapper's overhead is dominated by the cgo boundary, not the mutex or the Go code.

Note: this module requires `CGO_ENABLED=1` and a working C compiler (`gcc` or `clang`). It will not compile offline with `CGO_ENABLED=0`. Validated by §15 prose/code consistency + gofmt + go vet on the extractable Go portions.

## Summary

- Every cgo wrapper needs five things: an opaque handle, a mutex for goroutine safety, sentinel errors for C error codes, a finalizer backed by a closed flag, and use-after-close protection.
- `C.CString(s)` allocates C memory; always pair it with `defer C.free(unsafe.Pointer(ptr))`.
- `runtime.SetFinalizer` is a safety net for GC cleanup. It does not replace explicit `Close()`.
- `atomic.Bool` with `CompareAndSwap` in `Close` ensures exactly one call to the C cleanup function, even under concurrent use.
- The public API must not expose `C.*` types, `unsafe.Pointer`, or any cgo detail. Callers should not need to import `"C"` or `"unsafe"` to use the package.

## What's Next

Next: [Zero-Copy Deserialization](../09-zero-copy-deserialization/09-zero-copy-deserialization.md).

## Resources

- [cmd/cgo: Passing pointers](https://pkg.go.dev/cmd/cgo#hdr-Passing_pointers) — rules for what can cross the boundary
- [runtime.SetFinalizer](https://pkg.go.dev/runtime#SetFinalizer) — semantics, caveats, and GC interaction
- [Go wiki: cgo](https://go.dev/wiki/cgo) — comprehensive best-practices guide
- [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3) — production example of wrapping a large C library (SQLite3)
- [Go blog: C? Go? Cgo!](https://go.dev/blog/cgo) — introductory reference from the Go team
