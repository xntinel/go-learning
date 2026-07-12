# Exercise 32: DNS Resolution Cache: Goroutine Capturing and Mutating Shared Cache Entry

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A DNS cache warms itself by resolving a batch of hosts concurrently, one
goroutine per host, each writing its result into a per-host `*Entry`. The
trap: declaring ONE `*Entry` before the loop and having every goroutine
mutate that SAME entry instead of getting its own. Each individual write is
correctly mutex-protected — there is no data race on memory — but every key
in the cache map ends up pointing at the exact same object, so the cache
holds only whichever host's resolution happened to write last, and looking
up any OTHER host silently returns that one shared host's data.

## What you'll build

```text
dnscache/                    independent module: example.com/dnscache
  go.mod                     go 1.24
  dnscache.go                 Entry, ResolveFunc, BuggyResolveAll, ResolveAll
  cmd/
    demo/
      main.go                runnable demo: resolve 3 hosts, print aliasing + per-host lookups
  dnscache_test.go            table test: distinct entries vs one aliased entry; edge case
```

- Files: `dnscache.go`, `cmd/demo/main.go`, `dnscache_test.go`.
- Implement: `Entry` with its own mutex guarding `host`/`addrs`; `BuggyResolveAll` sharing one `*Entry` across every host; `ResolveAll` allocating a fresh `*Entry` per host.
- Test: resolve several hosts concurrently and assert `ResolveAll` gives each host a distinct pointer with its own data while `BuggyResolveAll` makes every key alias the identical pointer; `-race` clean over multiple runs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why per-entry locking does not save you from sharing the wrong entry

`Entry.set` is properly guarded by its own `sync.Mutex`, so concurrent
writes to the SAME entry can never corrupt its fields — that much is
correct in both variants of this exercise. The actual bug in
`BuggyResolveAll` is one level up: it allocates a single `*Entry` before the
loop and stores that SAME pointer under every host's key in the result map,
then spawns one goroutine per host that all call `.set` on that one shared
object. There is no data race — `.set`'s mutex serializes every writer
correctly — but there is only ever ONE entry behind N keys, so the cache
converges on whichever host's resolution happened to run last, and every
other host's lookup silently returns that one survivor's data instead of
its own. Because which host wins that race is genuinely scheduler-dependent,
the test asserts the part that IS deterministic regardless of scheduling:
every key maps to the identical pointer, and therefore every lookup returns
the identical result.

`ResolveAll` fixes this by allocating a fresh `*Entry` inside the loop, one
per host, before spawning that host's goroutine — distinct pointers can
never alias, so this needs no barrier or scheduling trick to test: `results[i]`
being that goroutine's own memory is true regardless of execution order.

Create `dnscache.go`:

```go
package dnscache

import "sync"

// Entry is one cached DNS resolution result, safe for concurrent access via
// its own mutex.
type Entry struct {
	mu    sync.Mutex
	host  string
	addrs []string
}

func (e *Entry) set(host string, addrs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.host = host
	e.addrs = addrs
}

// Snapshot returns the host and addresses this Entry currently holds.
func (e *Entry) Snapshot() (host string, addrs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.host, append([]string(nil), e.addrs...)
}

// ResolveFunc resolves one host to its addresses.
type ResolveFunc func(host string) []string

// BuggyResolveAll resolves every host concurrently, but every goroutine
// mutates the SAME *Entry declared once before the loop instead of getting
// its own. Each write is individually mutex-protected inside Entry.set, so
// there is no data race on memory -- but because every host aliases one
// shared Entry, the cache map ends up with every key pointing at the exact
// same object, and it ends up holding only whichever host's resolution
// happened to run last. Looking up any OTHER host's entry silently returns
// that one shared host's data instead of its own.
func BuggyResolveAll(hosts []string, resolve ResolveFunc) map[string]*Entry {
	out := make(map[string]*Entry, len(hosts))
	shared := &Entry{} // BUG: one Entry for every host
	var wg sync.WaitGroup
	for _, host := range hosts {
		out[host] = shared
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			shared.set(h, resolve(h))
		}(host)
	}
	wg.Wait()
	return out
}

// ResolveAll resolves every host concurrently, each into its OWN *Entry, so
// concurrent resolutions for different hosts never alias the same memory.
func ResolveAll(hosts []string, resolve ResolveFunc) map[string]*Entry {
	out := make(map[string]*Entry, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, host := range hosts {
		e := &Entry{}
		mu.Lock()
		out[host] = e
		mu.Unlock()
		wg.Add(1)
		go func(h string, e *Entry) {
			defer wg.Done()
			e.set(h, resolve(h))
		}(host, e)
	}
	wg.Wait()
	return out
}
```

### The runnable demo

Which host's resolution wins the race for the buggy version's one shared
entry is scheduler-dependent, so the demo reports the deterministic part of
the bug: every key aliases the same object, and every lookup agrees.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dnscache"
)

func resolve(host string) []string {
	return []string{host + "-ip"}
}

func main() {
	hosts := []string{"a.example.com", "b.example.com", "c.example.com"}

	// Which host's resolution happens to win the race for the one shared
	// Entry is scheduler-dependent, so the demo reports the deterministic
	// part of the bug: every key in the map aliases the exact same object,
	// and every lookup therefore returns the exact same (host, addrs) pair.
	buggy := dnscache.BuggyResolveAll(hosts, resolve)
	allSameEntry := true
	for _, h := range hosts {
		if buggy[h] != buggy[hosts[0]] {
			allSameEntry = false
		}
	}
	fmt.Println("buggy  every host key aliases the same *Entry:", allSameEntry)

	wantHost, _ := buggy[hosts[0]].Snapshot()
	allSameResult := true
	for _, h := range hosts {
		host, _ := buggy[h].Snapshot()
		if host != wantHost {
			allSameResult = false
		}
	}
	fmt.Println("buggy  every lookup returns the identical result:", allSameResult)

	fixed := dnscache.ResolveAll(hosts, resolve)
	for _, h := range hosts {
		host, addrs := fixed[h].Snapshot()
		fmt.Printf("fixed  lookup(%s) -> host=%s addrs=%v\n", h, host, addrs)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy  every host key aliases the same *Entry: true
buggy  every lookup returns the identical result: true
fixed  lookup(a.example.com) -> host=a.example.com addrs=[a.example.com-ip]
fixed  lookup(b.example.com) -> host=b.example.com addrs=[b.example.com-ip]
fixed  lookup(c.example.com) -> host=c.example.com addrs=[c.example.com-ip]
```

### Tests

`TestResolveAll` is a table test: the fixed variant asserts every host maps
to a distinct pointer holding exactly its own resolution; the buggy variant
asserts every host maps to the SAME pointer and every lookup returns the
same, valid, surviving result. `TestResolveAllSingleHostEdgeCase` covers the
boundary where there is no other host to alias into.

Create `dnscache_test.go`:

```go
package dnscache

import "testing"

func testResolve(host string) []string {
	return []string{host + "-ip"}
}

func TestResolveAll(t *testing.T) {
	hosts := []string{"a.com", "b.com", "c.com", "d.com"}

	tests := []struct {
		name        string
		resolveAll  func([]string, ResolveFunc) map[string]*Entry
		wantAliased bool // true: every host maps to the SAME *Entry
	}{
		{
			name:        "fixed: each host gets its own Entry",
			resolveAll:  ResolveAll,
			wantAliased: false,
		},
		{
			name:        "buggy: every host aliases the same shared Entry",
			resolveAll:  BuggyResolveAll,
			wantAliased: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.resolveAll(hosts, testResolve)
			if len(got) != len(hosts) {
				t.Fatalf("len(cache) = %d, want %d", len(got), len(hosts))
			}

			first := got[hosts[0]]
			for _, h := range hosts[1:] {
				aliased := got[h] == first
				if aliased != tt.wantAliased {
					t.Fatalf("entry[%q] aliases entry[%q] = %v, want %v", h, hosts[0], aliased, tt.wantAliased)
				}
			}

			if !tt.wantAliased {
				for _, h := range hosts {
					host, addrs := got[h].Snapshot()
					if host != h {
						t.Fatalf("Snapshot host = %q, want %q", host, h)
					}
					want := h + "-ip"
					if len(addrs) != 1 || addrs[0] != want {
						t.Fatalf("Snapshot addrs = %v, want [%s]", addrs, want)
					}
				}
				return
			}

			// Aliased case: every lookup must return the exact same result,
			// whichever host happened to write last (scheduler-dependent).
			wantHost, _ := first.Snapshot()
			found := false
			for _, h := range hosts {
				if h == wantHost {
					found = true
				}
			}
			if !found {
				t.Fatalf("surviving host %q does not match any resolved host", wantHost)
			}
			for _, h := range hosts {
				host, _ := got[h].Snapshot()
				if host != wantHost {
					t.Fatalf("lookup(%q) = %q, want %q (every lookup shares one Entry)", h, host, wantHost)
				}
			}
		})
	}
}

func TestResolveAllSingleHostEdgeCase(t *testing.T) {
	t.Parallel()

	hosts := []string{"solo.com"}
	fixed := ResolveAll(hosts, testResolve)
	buggy := BuggyResolveAll(hosts, testResolve)

	fHost, fAddrs := fixed["solo.com"].Snapshot()
	if fHost != "solo.com" || len(fAddrs) != 1 || fAddrs[0] != "solo.com-ip" {
		t.Fatalf("fixed snapshot = (%q, %v), want (%q, %v)", fHost, fAddrs, "solo.com", []string{"solo.com-ip"})
	}

	bHost, bAddrs := buggy["solo.com"].Snapshot()
	if bHost != "solo.com" || len(bAddrs) != 1 || bAddrs[0] != "solo.com-ip" {
		t.Fatalf("buggy snapshot = (%q, %v), want (%q, %v) (single host: bug can't manifest)", bHost, bAddrs, "solo.com", []string{"solo.com-ip"})
	}
}
```

## Review

A resolution cache is correct when every host's key maps to storage that
actually belongs to that host, no matter how many resolutions run
concurrently. The mutex inside `Entry` proves one thing and one thing only:
that writes to a GIVEN entry are serialized. It says nothing about whether
the loop that builds the cache handed out one entry per host or reused a
single one — that is a decision made before any goroutine even starts, in
how `out[host]` gets populated. `TestResolveAll`'s pointer-identity check is
the guard: it catches aliasing even when the content-level bug (which host's
data survives) is itself nondeterministic. Run `go test -race` repeatedly;
both variants stay clean, because the bug was never a race, only a decision
about how much memory to allocate.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — protecting one Entry's fields; does not protect against sharing the wrong Entry.
- [Go spec: Go statements](https://go.dev/ref/spec#Go_statements) — function arguments are evaluated when the `go` statement executes, not when the goroutine runs.
- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why per-goroutine allocation, not just per-iteration variables, is what matters here.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-batch-soft-delete-with-error-joined-defer.md](31-batch-soft-delete-with-error-joined-defer.md) | Next: [33-fan-in-multi-channel-shared-result-write-index.md](33-fan-in-multi-channel-shared-result-write-index.md)
