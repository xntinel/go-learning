# Exercise 10: Post-Mortem Analysis of a Core Dump

The hardest crashes are the ones you cannot reproduce: a nil dereference that fires
once, in the production container, under a request you cannot recreate. For those
you debug the corpse. `GOTRACEBACK=crash` makes the runtime leave a core dump on a
fatal panic, and `dlv core` reconstructs the goroutines, stacks, and locals as they
were at the instant of death. This module builds a crashing binary and performs the
post-mortem.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
coredbg/                   independent module: example.com/coredbg
  go.mod                   go 1.24
  crashy/
    crashy.go              Account; Lookup (may return nil); Withdraw (derefs)
  cmd/
    demo/
      main.go              safe by default; `crash` arg triggers the nil deref
  crashy/crashy_test.go    normal-path table test + recover-based panic test
```

- Files: `crashy/crashy.go`, `cmd/demo/main.go`, `crashy/crashy_test.go`.
- Implement: `Lookup` returning `nil` for a missing id and `Withdraw` that dereferences its account (panicking on `nil`).
- Test: table-driven normal withdrawals plus a `recover`-based test asserting the nil-account panic.
- Verify: `go test -count=1 -race ./...`, then (on Linux) a `dlv core` session whose stack names `Withdraw`.

Set up the module:

```bash
mkdir -p ~/go-exercises/coredbg/crashy ~/go-exercises/coredbg/cmd/demo
cd ~/go-exercises/coredbg
go mod init example.com/coredbg
go mod edit -go=1.24
```

### How a Go crash becomes a core file

By default the Go runtime prints a goroutine traceback and exits on an unrecovered
panic — no core file. `GOTRACEBACK=crash` changes the ending: after printing the
traceback the runtime raises `SIGABRT`, and if the OS is configured to write cores
(`ulimit -c unlimited`) a core dump lands on disk. `dlv core <exe> <corefile>`
loads that snapshot. It is not a live process — you cannot continue it — but every
inspection command works: `goroutines` lists the goroutines as they were,
`goroutine <id>` switches to the one that panicked, `stack` shows its frames, and
`frame`/`locals`/`print` read the variables at the moment of death. The binary
paired with the core must be the one that crashed, built with `-N -l`, or the
frames and variables will not line up.

There is a platform caveat. Linux writes Go core dumps cleanly. macOS routes cores
to `/cores` and gates them behind permissions and SIP, so the workflow differs;
the fallback below uses the runtime traceback and `dlv attach` instead. Within a
live session, Delve's `dump` command writes a core of the current process so you
can capture state without a crash at all.

Create `crashy/crashy.go`:

```go
package crashy

// Account holds a balance.
type Account struct {
	Balance int
}

// Lookup returns the account for id, or nil if it does not exist.
func Lookup(accounts map[string]*Account, id string) *Account {
	return accounts[id]
}

// Withdraw subtracts amt from the account's balance and returns the new balance.
// It dereferences a; calling it with a nil account panics with a nil pointer
// dereference — exactly the crash the core dump captures.
func Withdraw(a *Account, amt int) int {
	a.Balance -= amt
	return a.Balance
}
```

Create `cmd/demo/main.go`. By default it withdraws safely and prints; with the
`crash` argument it looks up a missing account and dereferences the `nil`, which is
the crash you analyze post-mortem.

```go
package main

import (
	"fmt"
	"os"

	"example.com/coredbg/crashy"
)

func main() {
	accounts := map[string]*crashy.Account{"alice": {Balance: 100}}

	if len(os.Args) > 1 && os.Args[1] == "crash" {
		bob := crashy.Lookup(accounts, "bob") // not in the map: nil
		crashy.Withdraw(bob, 10)              // panic: nil pointer dereference
		return
	}

	alice := crashy.Lookup(accounts, "alice")
	fmt.Printf("balance after withdraw: %d\n", crashy.Withdraw(alice, 30))
}
```

Run it (the safe path):

```bash
go run ./cmd/demo
```

Expected output:

```text
balance after withdraw: 70
```

### Producing and opening the core (Linux)

Guard the core workflow on the platform, build with debug info, crash the binary
with `GOTRACEBACK=crash`, then open the core:

```bash
if [ "$(uname)" = "Linux" ]; then
	ulimit -c unlimited
	go build -gcflags='all=-N -l' -o /tmp/coredbg ./cmd/demo

	GOTRACEBACK=crash /tmp/coredbg crash   # crashes, writes a core file
	# the core path depends on /proc/sys/kernel/core_pattern; often ./core or core.PID
	CORE=$(ls -t core* 2>/dev/null | head -1)

	dlv core /tmp/coredbg "$CORE"
else
	echo "macOS: cores go to /cores behind SIP; use the traceback + dlv attach fallback below"
fi
```

Inside the `dlv core` session, find the goroutine that panicked and read its stack:

```text
(dlv) goroutines
* Goroutine 1 - User: ./cmd/demo/main.go:15 main.main (0x...) [running]
...
(dlv) stack
0  runtime.raise ...
...
K  example.com/coredbg/crashy.Withdraw() ./crashy/crashy.go:17
K+1 main.main() ./cmd/demo/main.go:15
...
(dlv) frame K
> example.com/coredbg/crashy.Withdraw() ./crashy/crashy.go:17
(dlv) print a
*example.com/coredbg/crashy.Account nil
(dlv) print amt
10
```

The stack names `crashy.Withdraw` at line 17 — the `a.Balance -= amt` line — and
`print a` shows the account pointer is `nil`, which is the root cause: `Lookup`
returned `nil` for the missing "bob" and `Withdraw` dereferenced it. You debugged a
crash you never had to reproduce interactively.

### The fallback when cores are unavailable

On macOS (or any host without core support), skip the core file. Run the binary
with `GOTRACEBACK=crash` and read the traceback it prints to stderr — it names the
same `Withdraw` frame and the nil dereference. To inspect live state, set a
breakpoint just before the deref with `dlv debug ./cmd/demo -- crash`, break on
`crashy.Withdraw`, and `print a` to see the `nil` before it crashes. The
post-mortem view is richer, but the traceback plus a pre-crash breakpoint recovers
the same root cause.

### The test: normal path plus a recovered panic

A test binary must not actually crash, so the panic path is exercised with
`recover`, asserting the nil dereference happens rather than letting it kill the
process.

Create `crashy/crashy_test.go`:

```go
package crashy

import (
	"fmt"
	"strings"
	"testing"
)

func TestWithdraw(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		balance int
		amt     int
		want    int
	}{
		{name: "partial", balance: 100, amt: 30, want: 70},
		{name: "to_zero", balance: 50, amt: 50, want: 0},
		{name: "overdraw", balance: 10, amt: 25, want: -15},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &Account{Balance: tc.balance}
			if got := Withdraw(a, tc.amt); got != tc.want {
				t.Fatalf("Withdraw(%d, %d) = %d; want %d", tc.balance, tc.amt, got, tc.want)
			}
		})
	}
}

func TestWithdrawNilPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Withdraw(nil, ...) did not panic")
		}
		err, ok := r.(error)
		if !ok || !strings.Contains(err.Error(), "nil pointer") {
			t.Fatalf("unexpected panic value: %v", r)
		}
	}()

	Withdraw(nil, 10)
}

func TestLookupMissing(t *testing.T) {
	t.Parallel()

	accounts := map[string]*Account{"alice": {Balance: 100}}
	if got := Lookup(accounts, "bob"); got != nil {
		t.Fatalf("Lookup(bob) = %v; want nil", got)
	}
}

func ExampleWithdraw() {
	a := &Account{Balance: 100}
	fmt.Println(Withdraw(a, 30))
	// Output: 70
}
```

Run the gate:

```bash
go vet ./...
go test -count=1 -race ./...
```

## Review

The code is correct in the sense the crash needs: `Lookup` returns `nil` for a
missing id and `Withdraw` dereferences its argument, so the recovered-panic test
proves the nil dereference fires and the normal table pins the arithmetic. The
post-mortem proof is that `dlv core` reconstructs the `Withdraw` frame with `a ==
nil` from a dead process — the same root cause you would find live, but from a
crash you cannot reproduce. The mistakes to avoid are expecting a core without
`GOTRACEBACK=crash` and `ulimit -c unlimited`, pairing the core with a different
build (frames drift), and assuming macOS behaves like Linux (it does not; use the
traceback plus `dlv attach` fallback). The `dump` command captures a core from a
live session when you want state without a crash.

## Resources

- [`runtime` package](https://pkg.go.dev/runtime) — `GOTRACEBACK` values and the crash behavior that writes a core.
- [Delve core dump documentation](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_core.md) — `dlv core` and what it reconstructs.
- [Debugging Go programs with Delve (blog)](https://go.dev/blog/delve) — the Go team's overview, including post-mortem debugging.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../08-cross-compilation-and-build-tags/00-concepts.md](../08-cross-compilation-and-build-tags/00-concepts.md)
