# Exercise 9: Catch Unexpected Mutation with Watchpoints

Some bugs are not "wrong code runs" but "the right code runs and something else
scribbles on your data". When a field holds an impossible value and you have no
idea who wrote it, a watchpoint is the answer: Delve uses the CPU's debug
registers to stop the program the instant a memory location changes, and the stack
at that stop names the guilty writer. This module plants an unexpected reset and
hunts it with a watchpoint.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
watchdbg/                  independent module: example.com/watchdbg
  go.mod                   go 1.24
  state/
    state.go               type Counter; Inc; Run  (monotonic, correct)
  cmd/
    demo/
      main.go              runs the counter, prints the final value
  state/state_test.go      table-driven monotonic test + Example
```

- Files: `state/state.go`, `cmd/demo/main.go`, `state/state_test.go`.
- Implement: a monotonic `Counter` with `Inc()` and a `Run(c *Counter, n int)` that increments `n` times.
- Test: assert `Run` leaves `Value == n` for several `n`; run under `-race`.
- Verify: `go test -count=1 -race ./...`, then a scripted `dlv` watchpoint session on the buggy variant whose stack names the rogue writer.

Set up the module:

```bash
mkdir -p ~/go-exercises/watchdbg/state ~/go-exercises/watchdbg/cmd/demo
cd ~/go-exercises/watchdbg
go mod init example.com/watchdbg
go mod edit -go=1.24
```

### The rogue writer, and how a watchpoint finds it

Imagine the counter is supposed to be monotonic, but in production `Value` keeps
dropping back to zero and nothing in `Inc` explains it. Here is the buggy variant:
a leftover `reset` call sneaks in mid-loop. It is illustrative; do not save it:

```go
package state

// Counter should only ever increase via Inc.
type Counter struct {
	Value int
}

func (c *Counter) Inc() { c.Value++ }

// reset is the rogue writer: a leftover that zeroes Value. You do not know it
// exists yet — the watchpoint is how you discover it.
func reset(c *Counter) { c.Value = 0 }

// Run increments n times, but a stray reset corrupts the count.
func Run(c *Counter, n int) {
	for i := range n {
		c.Inc()
		if i == 2 {
			reset(c) // the bug
		}
	}
}
```

A watchpoint on `c.Value` stops on every write. Most stops are ordinary `Inc`
calls; the one whose stack is a surprise is the culprit. Set it up by breaking in
`main` once `c` exists, then watching the field:

```bash
dlv debug ./cmd/demo   # built from the buggy variant
```

```text
(dlv) break cmd/demo/main.go:11
Breakpoint 1 set at 0x... for main.main() ./cmd/demo/main.go:11
(dlv) continue
> main.main() ./cmd/demo/main.go:11 (hits goroutine(1):1 total:1)
(dlv) watch -w c.Value
Watchpoint c.Value set at 0x...
(dlv) continue
> example.com/watchdbg/state.(*Counter).Inc() ./state/state.go:8 (hits ...)
    Watchpoint c.Value written
(dlv) continue
...
(dlv) continue
> example.com/watchdbg/state.reset() ./state/state.go:12 (hits ...)
    Watchpoint c.Value written
(dlv) stack
0  example.com/watchdbg/state.reset() ./state/state.go:12
1  example.com/watchdbg/state.Run() ./state/state.go:19
2  main.main() ./cmd/demo/main.go:11
...
(dlv) clearall
```

The stop inside `reset`, with `Run` and `main` above it, is the whole diagnosis:
the guilty write comes from `state.reset`, called by `Run`. `watch -w` watches for
writes (`-r` for reads, `-rw` for both); `clearall` removes every breakpoint and
watchpoint. Because watchpoints use scarce hardware debug registers and are tied to
an addressable, in-scope expression, keep the count small and set them on a
long-lived value like this heap-allocated counter.

### The fix: remove the rogue writer

Create `state/state.go` — the monotonic version with no reset:

```go
package state

// Counter tracks a monotonic count. Value only ever increases via Inc.
type Counter struct {
	Value int
}

// Inc adds one to the count.
func (c *Counter) Inc() { c.Value++ }

// Run increments the counter n times.
func Run(c *Counter, n int) {
	for range n {
		c.Inc()
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/watchdbg/state"
)

func main() {
	c := &state.Counter{}
	state.Run(c, 5)
	fmt.Printf("final count: %d\n", c.Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
final count: 5
```

### The test pins monotonic behavior

Create `state/state_test.go`:

```go
package state

import (
	"fmt"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want int
	}{
		{name: "zero", n: 0, want: 0},
		{name: "one", n: 1, want: 1},
		{name: "five", n: 5, want: 5},
		{name: "hundred", n: 100, want: 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Counter{}
			Run(c, tc.n)
			if c.Value != tc.want {
				t.Fatalf("Run(c, %d) left Value = %d; want %d", tc.n, c.Value, tc.want)
			}
		})
	}
}

func ExampleCounter() {
	c := &Counter{}
	Run(c, 5)
	fmt.Println(c.Value)
	// Output: 5
}
```

Run the gate:

```bash
go vet ./...
go test -count=1 -race ./...
```

### Scripted: name the rogue writer

Against a binary built from the buggy variant, a script sets the watchpoint,
continues to each write, and dumps the stack on the reset hit:

```bash
go build -gcflags='all=-N -l' -o /tmp/watchdbg ./cmd/demo   # buggy variant

cat > /tmp/watch.dlv <<'EOF'
break cmd/demo/main.go:11
continue
watch -w c.Value
continue
continue
continue
continue
stack
quit
EOF

dlv exec /tmp/watchdbg --init /tmp/watch.dlv 2>&1 | tee /tmp/watch.out
grep -q 'state.reset' /tmp/watch.out && echo "rogue writer identified"
```

The captured stack shows `state.reset` at the top when the watchpoint fires on the
zeroing write, which a CI job asserts with the `grep`.

## Review

The counter is correct when `Run` leaves `Value == n` — monotonic, no hidden
writes — which the table pins across several sizes. The watchpoint proof is that
stopping on the mutation lands inside the writer with the full call chain above it,
so `state.reset` (and its caller `Run`) is named without guesswork. The mistakes to
avoid are treating watchpoints as unlimited (they consume a handful of hardware
debug registers) and watching a short-lived local (when its frame returns the
watchpoint is gone); watch a field on a long-lived, addressable value. `watch -w`
is write-only, `-r` read-only, `-rw` both; `clearall` clears everything when you
are done.

## Resources

- [Delve CLI command reference](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) — `watch` (`-r`/`-w`/`-rw`), `clearall`, `stack`.
- [Delve watchpoints documentation](https://github.com/go-delve/delve/tree/master/Documentation) — how hardware watchpoints map to debug registers and their limits.
- [Go data race detector](https://go.dev/doc/articles/race_detector) — the complementary tool for cross-goroutine mutation you run under `-race`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-postmortem-core-dump.md](10-postmortem-core-dump.md)
