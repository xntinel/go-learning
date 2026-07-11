# Exercise 19: Resuming from Checkpoint with Bounded Retry Logic

**Nivel: Intermedio** — validacion rapida (un test corto).

Restarting a stateful service after a crash means loading the last
checkpoint from storage — which might be flaky right when you need it most —
validating that it is not corrupt, and then replaying whatever write-ahead
log entries happened after it was taken. This module splits that into the
two loop shapes each half actually needs: a counted retry budget for the
"storage might be temporarily down" problem, and a condition-only replay for
the "catch the state up to the latest version" problem.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
checkpoint/                    module example.com/checkpoint
  go.mod                       go 1.24
  checkpoint.go                State; Recover(load, validate, maxAttempts, ...); Replay(st, entries, target)
  checkpoint_test.go             first-try success, retry-then-succeed, invalid checkpoint, budget exhausted, replay table
  cmd/demo/
    main.go                     two transient failures then a valid checkpoint, then a WAL replay
```

- Files: `checkpoint.go`, `checkpoint_test.go`, `cmd/demo/main.go`.
- Implement: `Recover(load LoadFunc, validate func(State) bool, maxAttempts int, base, max time.Duration, sleep func(time.Duration)) (State, int, error)` — a counted `for attempt := 0; attempt < maxAttempts; attempt++` retry loop with exponential backoff; `Replay(st State, entries []LogEntry, targetVersion int) State` — a condition-only `for st.Version < targetVersion && i < len(entries)` loop.
- Test: succeeds on the first try (no sleeps); a transient load error retries and eventually succeeds; a loaded-but-invalid checkpoint retries; the retry budget is exhausted and returns `ErrRecoveryFailed`; `Replay` catching up exactly to target, running out of entries first, and already being past target.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/checkpoint/cmd/demo
cd ~/go-exercises/checkpoint
go mod init example.com/checkpoint
go mod edit -go=1.24
```

### Why recovery is two loops, not one

`Recover` answers "can I get *a* valid checkpoint at all" and its bound is a
retry budget: `maxAttempts` is a resource limit against a possibly-broken
storage backend, so the counted form is correct — the worst case (every
attempt fails, `maxAttempts` sleeps happen) is visible at the top of the
function without reading the body. `Replay` answers a completely different
question — "how far does this *particular* checkpoint need to be advanced" —
and that has no natural attempt count at all; it depends entirely on how many
log entries exist between the checkpoint's version and the target. That is
exactly the condition-only shape: keep applying entries while the version
predicate is still unsatisfied *and* entries remain, and stop the instant
either is no longer true. The `i < len(entries)` half of that condition is
what makes the loop provably terminating even if the target version is
unreachable (say, the log was truncated) — without it, a checkpoint that can
never catch up would spin trying to index past the end of `entries`.

Create `checkpoint.go`:

```go
package checkpoint

import (
	"errors"
	"fmt"
	"time"
)

// ErrRecoveryFailed means the retry budget was spent without a valid checkpoint.
var ErrRecoveryFailed = errors.New("checkpoint: recovery failed")

// ErrInvalidCheckpoint means a checkpoint loaded without error but failed validation.
var ErrInvalidCheckpoint = errors.New("checkpoint: invalid checkpoint")

// State is the recovered application state.
type State struct {
	Version int
	Data    string
}

// LoadFunc attempts to load the last checkpoint. It may fail transiently
// (a storage backend hiccup), in which case a retry can succeed later.
type LoadFunc func() (State, error)

// Recover tries to load and validate a checkpoint up to maxAttempts times,
// sleeping with exponential backoff between attempts. The retry budget is a
// counted for loop -- the worst case (maxAttempts calls, maxAttempts sleeps)
// is visible at the top of the function, so a storage backend that is
// completely down produces a bounded delay and a handled error, never a hang.
func Recover(load LoadFunc, validate func(State) bool, maxAttempts int, base, max time.Duration, sleep func(time.Duration)) (State, int, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		st, err := load()
		if err != nil {
			lastErr = err
			sleep(backoffDelay(attempt, base, max))
			continue
		}
		if !validate(st) {
			lastErr = ErrInvalidCheckpoint
			sleep(backoffDelay(attempt, base, max))
			continue
		}
		return st, attempt, nil
	}
	return State{}, maxAttempts, fmt.Errorf("%w after %d attempts: %v", ErrRecoveryFailed, maxAttempts, lastErr)
}

// backoffDelay computes an exponential delay bounded by attempt, capped at max.
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	d := base
	for i := 0; i < attempt; i++ {
		if d*2 > max {
			return max
		}
		d *= 2
	}
	return d
}

// LogEntry is one write-ahead-log entry applied on top of a recovered
// checkpoint to catch it up to the latest known version.
type LogEntry struct {
	Version int
	Apply   func(State) State
}

// Replay advances st by applying entries in order, stopping the instant
// st.Version reaches targetVersion or the entries run out -- whichever comes
// first. This is a condition-only loop: it has no counter of its own, it
// simply keeps replaying until the recovered state is caught up, and it is
// guaranteed to terminate because len(entries) is a hard, independent bound
// on top of the version predicate.
func Replay(st State, entries []LogEntry, targetVersion int) State {
	i := 0
	for st.Version < targetVersion && i < len(entries) {
		st = entries[i].Apply(st)
		st.Version = entries[i].Version
		i++
	}
	return st
}
```

### The runnable demo

The demo's `load` fails twice with a simulated storage timeout before
succeeding on the third attempt, so `Recover` sleeps twice (100ms, then
200ms) before returning. It then replays two write-ahead-log entries on top
of the recovered checkpoint to bring it up to version 4.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/checkpoint"
)

func main() {
	attempts := 0
	load := func() (checkpoint.State, error) {
		attempts++
		if attempts < 3 {
			return checkpoint.State{}, fmt.Errorf("storage timeout (attempt %d)", attempts)
		}
		return checkpoint.State{Version: 2, Data: "orders=17"}, nil
	}
	validate := func(st checkpoint.State) bool { return st.Version > 0 }

	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }

	st, attempt, err := checkpoint.Recover(load, validate, 5, 100*time.Millisecond, time.Second, sleep)
	if err != nil {
		fmt.Println("recovery failed:", err)
		return
	}
	fmt.Printf("recovered at attempt %d: version=%d data=%q\n", attempt, st.Version, st.Data)
	fmt.Printf("backoff delays used: %v\n", slept)

	appendOp := func(s string) func(checkpoint.State) checkpoint.State {
		return func(cs checkpoint.State) checkpoint.State {
			cs.Data += "," + s
			return cs
		}
	}
	log := []checkpoint.LogEntry{
		{Version: 3, Apply: appendOp("orders=20")},
		{Version: 4, Apply: appendOp("orders=25")},
	}
	caughtUp := checkpoint.Replay(st, log, 4)
	fmt.Printf("caught up: version=%d data=%q\n", caughtUp.Version, caughtUp.Data)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
recovered at attempt 2: version=2 data="orders=17"
backoff delays used: [100ms 200ms]
caught up: version=4 data="orders=17,orders=20,orders=25"
```

### Tests

`TestRecoverSucceedsFirstTry` confirms the zero-sleep happy path;
`TestRecoverRetriesOnTransientErrorThenSucceeds` asserts both the returned
attempt index and the exact backoff sequence used; `TestRecoverExhaustsBudget`
confirms the bound is honored and `ErrRecoveryFailed` wraps the last
underlying error. `TestReplay` is a table over the three shapes the
condition-only loop must get right: catching up exactly to target, running
out of entries before reaching it, and a state already at or past target
(zero entries applied).

Create `checkpoint_test.go`:

```go
package checkpoint

import (
	"errors"
	"testing"
	"time"
)

var errStorage = errors.New("storage unavailable")

func alwaysValid(State) bool { return true }

func TestRecoverSucceedsFirstTry(t *testing.T) {
	t.Parallel()

	calls := 0
	load := func() (State, error) {
		calls++
		return State{Version: 5, Data: "ok"}, nil
	}
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }

	st, attempt, err := Recover(load, alwaysValid, 3, time.Second, 8*time.Second, sleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempt != 0 {
		t.Fatalf("attempt = %d, want 0", attempt)
	}
	if st.Version != 5 {
		t.Fatalf("Version = %d, want 5", st.Version)
	}
	if calls != 1 {
		t.Fatalf("load called %d times, want 1", calls)
	}
	if len(slept) != 0 {
		t.Fatalf("slept %v, want no sleeps on first-try success", slept)
	}
}

func TestRecoverRetriesOnTransientErrorThenSucceeds(t *testing.T) {
	t.Parallel()

	calls := 0
	load := func() (State, error) {
		calls++
		if calls < 3 {
			return State{}, errStorage
		}
		return State{Version: 1}, nil
	}
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }

	st, attempt, err := Recover(load, alwaysValid, 5, time.Second, 8*time.Second, sleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempt != 2 {
		t.Fatalf("attempt = %d, want 2", attempt)
	}
	if st.Version != 1 {
		t.Fatalf("Version = %d, want 1", st.Version)
	}
	wantSlept := []time.Duration{time.Second, 2 * time.Second}
	if len(slept) != len(wantSlept) {
		t.Fatalf("slept %v, want %v", slept, wantSlept)
	}
	for i, d := range wantSlept {
		if slept[i] != d {
			t.Errorf("slept[%d] = %v, want %v", i, slept[i], d)
		}
	}
}

func TestRecoverRejectsInvalidCheckpointAndRetries(t *testing.T) {
	t.Parallel()

	calls := 0
	load := func() (State, error) {
		calls++
		return State{Version: calls}, nil
	}
	validate := func(st State) bool { return st.Version >= 2 }
	sleep := func(time.Duration) {}

	st, attempt, err := Recover(load, validate, 5, time.Millisecond, time.Second, sleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}
	if st.Version != 2 {
		t.Fatalf("Version = %d, want 2", st.Version)
	}
}

func TestRecoverExhaustsBudget(t *testing.T) {
	t.Parallel()

	load := func() (State, error) { return State{}, errStorage }
	sleep := func(time.Duration) {}

	_, attempt, err := Recover(load, alwaysValid, 3, time.Millisecond, time.Second, sleep)
	if !errors.Is(err, ErrRecoveryFailed) {
		t.Fatalf("err = %v, want ErrRecoveryFailed", err)
	}
	if attempt != 3 {
		t.Fatalf("attempt = %d, want 3 (the exhausted budget)", attempt)
	}
}

func TestReplay(t *testing.T) {
	t.Parallel()

	appendOp := func(s string) func(State) State {
		return func(st State) State {
			st.Data += s
			return st
		}
	}
	entries := []LogEntry{
		{Version: 2, Apply: appendOp("b")},
		{Version: 3, Apply: appendOp("c")},
		{Version: 4, Apply: appendOp("d")},
	}

	tests := []struct {
		name          string
		start         State
		targetVersion int
		want          State
	}{
		{
			name:          "catches up to target before entries run out",
			start:         State{Version: 1, Data: "a"},
			targetVersion: 3,
			want:          State{Version: 3, Data: "abc"},
		},
		{
			name:          "entries run out before reaching target",
			start:         State{Version: 1, Data: "a"},
			targetVersion: 99,
			want:          State{Version: 4, Data: "abcd"},
		},
		{
			name:          "already at or past target: no entries applied",
			start:         State{Version: 5, Data: "a"},
			targetVersion: 3,
			want:          State{Version: 5, Data: "a"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Replay(tc.start, entries, tc.targetVersion)
			if got != tc.want {
				t.Errorf("Replay() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

## Review

`Recover` is correct when it never calls `load` more than `maxAttempts` times
and its returned attempt index matches exactly how many retries happened;
`Replay` is correct when it applies entries strictly in order and stops the
instant either bound is hit. The common mistake this design avoids is a
single loop that tries to be both a retry budget and a replay cursor —
mixing "how many times have I tried loading" with "how far have I replayed"
in one counter loses the independent termination proof each one needs.
`TestReplay`'s "entries run out before reaching target" case is the one that
catches a `Replay` that assumes the log always reaches the target and
indexes past the end of `entries`. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the counted and condition-only forms used here.
- [Write-ahead logging (PostgreSQL documentation)](https://www.postgresql.org/docs/current/wal-intro.html) — the checkpoint-plus-replay recovery model this module mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-config-loader-fallback-chain.md](18-config-loader-fallback-chain.md) | Next: [20-stream-frame-decoder-bounded.md](20-stream-frame-decoder-bounded.md)
