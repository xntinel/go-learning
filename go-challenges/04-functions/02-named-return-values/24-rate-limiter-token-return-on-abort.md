# Exercise 24: Rate Limiter Token Repayment on Operation Abort

Borrowing tokens from a rate limiter before an operation and forgetting to
give them back when that operation fails is a slow leak: the pool shrinks a
little every time something goes wrong, until it is permanently starved. This
exercise builds a `Do` helper that borrows tokens up front and repays them
through a deferred closure keyed on a named `committed bool` — repayment is
the default, and only a genuinely successful operation opts out of it.

**Nivel: Intermedio** — validacion rapida (cuatro pruebas cortas).

## What you'll build

```text
tokenlimiter/                    independent module: example.com/tokenlimiter
  go.mod
  tokenlimiter.go                 Limiter; borrow; Do (named committed, deferred repay)
  cmd/demo/
    main.go                       runnable demo: success, abort, over-budget request
  tokenlimiter_test.go             commits on success, repays on abort, rejects insufficient tokens
```

- Files: `tokenlimiter.go`, `cmd/demo/main.go`, `tokenlimiter_test.go`.
- Implement: `(*Limiter) Do(n int, op func() error) (committed bool, err error)` that borrows `n` tokens, runs `op`, and repays them via a deferred closure unless `op` succeeded.
- Test: a successful `op` permanently spends the tokens; a failing `op` gets them all back; a request for more tokens than are available is rejected without running `op` at all; repeated aborted borrows leave the pool exactly where it started.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tokenlimiter/cmd/demo
cd ~/go-exercises/tokenlimiter
go mod init example.com/tokenlimiter
go mod edit -go=1.24
```

### Repay by default, keep only on confirmed success

```go
release, berr := l.borrow(n)
if berr != nil {
    err = berr
    return
}

defer func() {
    if !committed {
        release()
    }
}()

if err = op(); err != nil {
    return
}
committed = true
return
```

`committed` starts false. The only line in the whole function that sets it to
true is the very last one, reached only after `op()` has returned a nil
error. That means the deferred closure's default behavior — repay the
tokens — is what happens on every other exit: `op` returning an error, or (if
`op` panics) the panic unwinding through `Do` before `committed` is ever set.
Repayment is the fallback, not something bolted onto each failure branch
individually; only the one true success path has to actively opt out of it
by setting the named result.

Create `tokenlimiter.go`:

```go
package tokenlimiter

import (
	"errors"
	"sync"
)

// ErrInsufficientTokens is returned when a borrow request cannot be
// satisfied from the pool.
var ErrInsufficientTokens = errors.New("tokenlimiter: insufficient tokens")

// Limiter is a simple token-bucket pool with no refill: tokens are only
// returned by an explicit repayment, which is what this exercise is about.
type Limiter struct {
	mu        sync.Mutex
	Available int
}

// NewLimiter returns a limiter starting with n available tokens.
func NewLimiter(n int) *Limiter {
	return &Limiter{Available: n}
}

// borrow removes n tokens from the pool, returning a release func that puts
// them back. It fails if fewer than n tokens are available.
func (l *Limiter) borrow(n int) (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Available < n {
		err = ErrInsufficientTokens
		return
	}
	l.Available -= n
	release = func() {
		l.mu.Lock()
		l.Available += n
		l.mu.Unlock()
	}
	return
}

// Do borrows n tokens, runs op, and commits the borrow only if op succeeds.
//
// committed is a named result checked by a deferred closure: if op panics or
// returns an error, committed is still false when the defer runs, so the
// defer calls release() and the tokens go back to the pool. Only the success
// path sets committed = true, which is what tells the defer to leave the
// tokens spent. Without a named result for this, the defer would have no
// reliable signal for "did this operation actually complete" to key off.
func (l *Limiter) Do(n int, op func() error) (committed bool, err error) {
	release, berr := l.borrow(n)
	if berr != nil {
		err = berr
		return
	}

	defer func() {
		if !committed {
			release()
		}
	}()

	if err = op(); err != nil {
		return
	}
	committed = true
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/tokenlimiter"
)

func main() {
	l := tokenlimiter.NewLimiter(5)

	committed, err := l.Do(3, func() error { return nil })
	fmt.Printf("successful op: committed=%v err=%v available=%d\n", committed, err, l.Available)

	committed, err = l.Do(2, func() error { return errors.New("op failed") })
	fmt.Printf("aborted op: committed=%v err=%v available=%d\n", committed, err, l.Available)

	_, err = l.Do(100, func() error { return nil })
	fmt.Printf("over-budget request: err=%v available=%d\n", err, l.Available)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
successful op: committed=true err=<nil> available=2
aborted op: committed=false err=op failed available=2
over-budget request: err=tokenlimiter: insufficient tokens available=2
```

### Tests

Create `tokenlimiter_test.go`:

```go
package tokenlimiter

import (
	"errors"
	"testing"
)

func TestDoCommitsOnSuccess(t *testing.T) {
	t.Parallel()

	l := NewLimiter(5)
	committed, err := l.Do(3, func() error { return nil })
	if err != nil {
		t.Fatalf("Do: unexpected error: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true on success")
	}
	if l.Available != 2 {
		t.Fatalf("Available = %d, want 2 (tokens spent)", l.Available)
	}
}

func TestDoRepaysOnAbort(t *testing.T) {
	t.Parallel()

	l := NewLimiter(5)
	wantErr := errors.New("op failed")
	committed, err := l.Do(3, func() error { return wantErr })

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if committed {
		t.Fatal("committed = true, want false on aborted op")
	}
	if l.Available != 5 {
		t.Fatalf("Available = %d, want 5 (tokens repaid)", l.Available)
	}
}

func TestDoRejectsInsufficientTokens(t *testing.T) {
	t.Parallel()

	l := NewLimiter(2)
	ran := false
	_, err := l.Do(5, func() error { ran = true; return nil })

	if !errors.Is(err, ErrInsufficientTokens) {
		t.Fatalf("err = %v, want ErrInsufficientTokens", err)
	}
	if ran {
		t.Fatal("op ran despite insufficient tokens")
	}
	if l.Available != 2 {
		t.Fatalf("Available = %d, want 2 (untouched)", l.Available)
	}
}

func TestDoSequentialBorrowAndRepayLeavesPoolConsistent(t *testing.T) {
	t.Parallel()

	l := NewLimiter(5)
	for i := 0; i < 10; i++ {
		_, _ = l.Do(1, func() error { return errors.New("always aborts") })
	}
	if l.Available != 5 {
		t.Fatalf("Available = %d, want 5 after repeated aborted borrows", l.Available)
	}
}
```

## Review

`Do` is correct when a successful `op` permanently spends exactly `n`
tokens, a failing `op` returns every one of those tokens to the pool, and a
request that cannot be satisfied never runs `op` at all. The named result
`committed` is the signal the deferred closure keys its repayment decision
on — the defer does not need to know *why* an operation aborted, only
whether it reached the one line that flips `committed` to true. The mistake
to avoid is releasing tokens unconditionally in the defer without checking
`committed`: that would repay a successful operation's tokens too, silently
inflating the pool back toward its starting size no matter how much work
actually completed.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex)
- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-cryptographic-key-cache-invalidate.md](23-cryptographic-key-cache-invalidate.md) | Next: [25-transaction-snapshot-restore-on-abort.md](25-transaction-snapshot-restore-on-abort.md)
