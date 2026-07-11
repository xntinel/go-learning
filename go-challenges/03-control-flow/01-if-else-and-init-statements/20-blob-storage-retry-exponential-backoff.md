# Exercise 20: Blob Storage Upload: Classify Errors and Compute Backoff Intervals

**Nivel: Intermedio** — validacion rapida (un test corto).

An object storage upload can fail for reasons that will never change no
matter how many times you retry — a permission denied, a bucket that does
not exist — and for reasons that are purely transient — a dropped
connection, a throttled request. Retrying the first kind wastes time and
hides a configuration bug; refusing to retry the second kind turns a blip
into a failed upload. This module builds the classification guard and the
backoff calculation as two small, independently testable functions. It is
fully self-contained: its own `go mod init`, all code inline, its own test
file.

## What you'll build

```text
blobretry/                  independent module: example.com/blob-storage-retry-exponential-backoff
  go.mod                    go 1.24
  retry.go                  IsPermanent(err), ShouldRetry(err, attempt, max), NextDelay(attempt, base, max)
  retry_test.go             table: permanent vs transient, attempt exhaustion, backoff doubling + cap
```

- Files: `retry.go`, `retry_test.go`.
- Implement: `IsPermanent(err error) bool` using `errors.Is` against sentinels, `ShouldRetry(err error, attempt, maxAttempts int) bool` guarding nil, permanent, and attempt-exhausted in that order, and `NextDelay(attempt int, base, max time.Duration) time.Duration` doubling per attempt with a cap.
- Test: a table over a nil error, a permanent error, a transient error under and at the attempt limit, and a backoff sequence that doubles then caps.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/blobretry
cd ~/go-exercises/blobretry
go mod init example.com/blob-storage-retry-exponential-backoff
go mod edit -go=1.24
```

### Why the cap guard also has to catch overflow, not just "too big"

Exponential backoff computed as `base * 2^(attempt-1)` grows fast — by
attempt 20 the multiplier alone is over half a million, and multiplying
that by a `time.Duration` base can overflow the signed 64-bit nanosecond
range and wrap around to a negative duration. A cap guard written only as
`if delay > max` misses that case, because a wrapped-around negative
number is not `> max`; it looks small. The guard has to check for a
non-positive result explicitly and treat it the same as "too big" — both
mean "do not trust this computed value, use the cap instead."

Create `retry.go`:

```go
// Package blobretry classifies blob storage upload errors and computes
// exponential backoff intervals between retries.
package blobretry

import (
	"errors"
	"time"
)

// ErrPermissionDenied and ErrNotFound are permanent: retrying never helps.
var (
	ErrPermissionDenied = errors.New("permission denied")
	ErrNotFound         = errors.New("bucket or object not found")
)

// IsPermanent reports whether err is one of the sentinel permanent failure
// causes, checked by errors.Is so wrapping with %w does not break the check.
func IsPermanent(err error) bool {
	return errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrNotFound)
}

// ShouldRetry decides whether attempt should be retried. A nil error needs no
// retry. A permanent error is never retried, regardless of attempt count. A
// transient error is retried only while attempt is below maxAttempts.
func ShouldRetry(err error, attempt, maxAttempts int) bool {
	if err == nil {
		return false
	}
	if IsPermanent(err) {
		return false
	}
	if attempt >= maxAttempts {
		return false
	}
	return true
}

// NextDelay computes the backoff before retrying attempt (1-indexed): base
// doubled once per prior attempt, capped at max. A computed delay that is
// non-positive — which only happens from signed overflow at a very high
// attempt count — is treated the same as "too big" and clamped to max.
func NextDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	delay := base * time.Duration(uint64(1)<<uint(attempt-1))
	if delay > max || delay <= 0 {
		return max
	}
	return delay
}
```

### Tests

The table checks classification and the retry guard together, then a
separate test locks in the doubling sequence and the cap, including an
attempt high enough to force the overflow guard.

Create `retry_test.go`:

```go
package blobretry

import (
	"errors"
	"testing"
	"time"
)

func TestShouldRetry(t *testing.T) {
	t.Parallel()

	transient := errors.New("connection reset")

	tests := []struct {
		name        string
		err         error
		attempt     int
		maxAttempts int
		want        bool
	}{
		{name: "nil error needs no retry", err: nil, attempt: 1, maxAttempts: 5, want: false},
		{name: "permission denied never retries", err: ErrPermissionDenied, attempt: 1, maxAttempts: 5, want: false},
		{name: "not found never retries", err: ErrNotFound, attempt: 1, maxAttempts: 5, want: false},
		{name: "transient error retries under the limit", err: transient, attempt: 2, maxAttempts: 5, want: true},
		{name: "transient error stops at the limit", err: transient, attempt: 5, maxAttempts: 5, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ShouldRetry(tc.err, tc.attempt, tc.maxAttempts); got != tc.want {
				t.Errorf("ShouldRetry(%v, %d, %d) = %v, want %v", tc.err, tc.attempt, tc.maxAttempts, got, tc.want)
			}
		})
	}
}

func TestNextDelay(t *testing.T) {
	t.Parallel()

	base := 100 * time.Millisecond
	max := 10 * time.Second

	tests := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{name: "first attempt uses base", attempt: 1, want: 100 * time.Millisecond},
		{name: "second attempt doubles", attempt: 2, want: 200 * time.Millisecond},
		{name: "third attempt doubles again", attempt: 3, want: 400 * time.Millisecond},
		{name: "eventually caps at max", attempt: 10, want: max},
		{name: "very high attempt guards against overflow, caps at max", attempt: 100, want: max},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NextDelay(tc.attempt, base, max); got != tc.want {
				t.Errorf("NextDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`IsPermanent` is checked before the attempt-count guard in `ShouldRetry`,
which matters operationally: a permission error on attempt 1 must fail
immediately rather than burn through `maxAttempts` retries against a
credential that will never start working. Carry this forward: when a
retry guard has both an error-classification check and a budget check,
classification must run first — a budget only applies to failures worth
spending the budget on.

## Resources

- [AWS SDK for Go v2: Retry behavior](https://aws.github.io/aws-sdk-go-v2/docs/configuring-sdk/retries-timeouts/) — production guidance on classifying retryable errors.
- [Google Cloud: Retry strategy](https://cloud.google.com/storage/docs/retry-strategy) — the transient-vs-permanent distinction for object storage specifically.
- [Go Specification: Integer overflow](https://go.dev/ref/spec#Integer_overflow) — why a doubling computation needs an explicit non-positive guard, not just an upper-bound comparison.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-dns-lookup-cache-stale-fallback.md](19-dns-lookup-cache-stale-fallback.md) | Next: [21-leader-election-heartbeat-mutex-protected.md](21-leader-election-heartbeat-mutex-protected.md)
