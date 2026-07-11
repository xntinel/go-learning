# Integrator Project: Idempotent Payments Gateway

A minimal, real payments-creation gateway: three HTTP endpoints backed by Postgres for durable, idempotent writes and Redis for per-caller rate limiting. Postgres is the system of record because a payment must survive a crash and must never be double-created when a client retries; Redis is the rate limiter because it is fast enough to check on every request without becoming the bottleneck, and because its failure mode can be deliberately decoupled from payment correctness. The surface is deliberately small — create, look up, health-check — because the point of this project is to get the idempotency and rate-limiting patterns exactly right, not to build a full ledger.

## What you'll build

```text
payments-gateway/
  go.mod                        module example.com/payments-gateway; go 1.26
  internal/
    domain/
      money.go                  Money value object + validation errors
      payment.go                Payment struct + PaymentStatus state machine
    store/
      postgres.go                idempotency-aware repository over pgx (pgxpool.Pool interface, so pgxmock can substitute it)
    ratelimit/
      limiter.go                 GCRA/token-bucket Lua-script limiter over go-redis (redis.Cmdable interface, so miniredis can substitute it) + fail-open wrapper
    api/
      handlers.go                the 3 HTTP handlers
      middleware.go               panic-recovery, slog request logging, rate-limit guard, composition helper
      server.go                   NewServer(...) wiring routes with Go 1.22+ net/http.ServeMux method+path patterns (no router framework)
  cmd/
    server/
      main.go                    real main(): env config (DATABASE_URL, REDIS_ADDR, PORT), pgxpool.New, redis.NewClient, migration-at-boot (idempotent CREATE TABLE IF NOT EXISTS), http.Server with graceful shutdown via signal.NotifyContext(SIGINT, SIGTERM) + Shutdown(ctx). A run(ctx, cfg) error function separate from main() for testability.
  internal/api/gateway_test.go     the offline test suite: pgxmock + miniredis + httptest
docker-compose.yml               postgres:16-alpine + redis:7-alpine with healthchecks and the env vars main() expects, for running the real thing locally
```

Files: `go.mod`, `internal/domain/money.go`, `internal/domain/payment.go`, `internal/store/postgres.go`, `internal/ratelimit/limiter.go`, `internal/api/middleware.go`, `internal/api/handlers.go`, `internal/api/server.go`, `cmd/server/main.go`, `internal/api/gateway_test.go`.
Implement: `domain.NewMoney`/`domain.Payment`; `store.Store` with `Migrate`, `CreatePayment` (fresh/replay/conflict), `GetPayment`, `Ping`; `ratelimit.Limiter` with `Allow` and `AllowFailOpen`; the three HTTP handlers and the middleware stack; `run(ctx, cfg) error` and `main()`.
Test: money/payment validation; the three idempotency paths against `pgxmock`; the rate limiter allow/deny/fail-open against `miniredis`; an end-to-end `httptest` suite covering create (201), replay (200, identical body), conflict (409), get by id (200/404), rate-limit exceeded (429), and `/healthz` (200).
Verify: `go test -race ./...`

### Why money is an integer, not a float

`amount_minor` is an `int64` counting the smallest unit of the currency (cents for USD, for example), never a `float64`. Floating-point binary fractions cannot represent most decimal amounts exactly — `0.1 + 0.2` is not `0.3` in IEEE 754 — so a float balance drifts under repeated arithmetic and can round a charge to the wrong cent. Financial systems universally settle on integer minor units (or arbitrary-precision decimals) for exactly this reason. `domain.Money` also validates the currency code shape at construction time, so a malformed amount or currency never reaches the database as a typed domain error (`ErrInvalidAmount`, `ErrInvalidCurrency`) rather than a generic string.

### Why idempotency needs a fingerprint, and why it must be one atomic transaction

An `Idempotency-Key` alone only proves "this is a retry of *some* earlier request" — it says nothing about whether it is a retry of *this* request. A caller (or a bug, or a proxy that retries too aggressively) could reuse a key with a different payload, and without a check the gateway would either silently return an unrelated payment or silently create a second one. The fix is a fingerprint: a hash of the normalized request payload stored alongside the key. On replay, a matching fingerprint proves the payload is identical and it is safe to hand back the original response; a mismatched fingerprint is a genuine conflict, reported as `409`, not silently resolved.

The classic bug in a hand-rolled idempotency layer is a pre-check: `SELECT` for the key, and if absent, `INSERT`. Between the `SELECT` and the `INSERT` there is a window — however small — where two concurrent requests carrying the same key can both see "not found" and both proceed to insert, producing two payment rows for one logical request. This is exactly the race idempotency keys exist to prevent, reintroduced by the naive implementation of the check. The correct approach, used here, is to attempt the `INSERT` directly inside a transaction and let Postgres's unique constraint on `idempotency_keys.key` be the arbiter: whichever concurrent request's `INSERT` reaches the index first wins outright, and every other concurrent request receives a unique-violation error (`pgconn.PgError` with `Code == "23505"`) that unambiguously means "someone already claimed this key" — at which point the loser rolls back its own attempt and reads the winner's stored row to decide replay-or-conflict. A unique index is enforced atomically by the database's own concurrency control; no amount of application-level locking is needed to get the same guarantee, and no window exists for two inserts to both "win".

### Why rate limiting needs an atomic Lua script, not GET-then-INCR

The same race shows up in rate limiting: reading a counter and then incrementing it as two separate Redis round trips leaves a window where multiple concurrent requests read the same "9 of 10 used" value and all decide they're within budget, over-admitting the bucket. Redis's `EVAL` runs a Lua script as a single atomic step on the server — no other command can interleave with it — so reading the bucket's state, refilling it for elapsed time, and debiting it can happen as one indivisible operation. This project implements a token-bucket limiter (the same family of algorithm as GCRA — Generic Cell Rate Algorithm — used by libraries like `redis_rate`): each caller gets a bucket that refills continuously at a steady rate and can hold up to `burst` tokens, so the caller is held to a smooth long-run rate while still being able to burst above it briefly, which is what most real APIs actually want (a hard fixed window unfairly slams the door shut at the window boundary or unfairly allows two full windows in a row at the boundary).

The trade-off deliberately taken here is fail-open: if Redis itself is unreachable, the limiter logs a warning and allows the request rather than blocking it. This is a considered decision, not an accident — the correctness of payment creation is entirely owned by Postgres's idempotency guarantee, so the rate limiter's job is capacity protection, not correctness. Making the limiter's own outage take down checkout would convert an availability problem in a supporting system into an availability problem in the critical path, which is a strictly worse failure mode than temporarily admitting more traffic than intended. This is implemented as a named method, `Limiter.AllowFailOpen`, rather than a bare `if err != nil { return true }` buried in a handler, so the policy is visible, documented, and easy to find and reverse later.

### Why the HTTP surface is minimal, and how errors map to status codes

Three endpoints — create, get, health — is the whole surface a "does idempotent payment creation work" gateway needs; a settle/process/cancel endpoint would be a different, larger project. Every error handler in this codebase maps a Go error to an HTTP status by `errors.Is` against a small set of sentinel errors (`domain.ErrInvalidAmount`, `store.ErrIdempotencyConflict`, `store.ErrPaymentNotFound`) or `errors.As` against a concrete type (`*pgconn.PgError`), never by matching against `err.Error()` text. String matching against error messages breaks the moment a dependency rewords its error text; sentinel errors and typed errors are a stable, compiler-checked contract between layers.

### Middleware ordering

`Recover` must be outermost: a panic anywhere below it — in `RequestLogger`, in a handler, in a dependency — unwinds the call stack looking for a deferred `recover()`, and if nothing outside the panicking frame has one, the whole process crashes. Placing `Recover` outermost guarantees one consistent last line of defense regardless of what panics. `RequestLogger` sits just inside it, so every request that doesn't panic gets one structured log line with its final status code; a request that does panic is logged once by `Recover` with the panic value, not twice. The rate-limit check is deliberately *not* implemented as a third link in that outer chain: this project's required ordering puts body decode+validation (`422`) before the rate-limit check (`429`), and a generic `http.Handler` middleware wrapping the whole route would run before the handler's own body-decoding code ever executes. Instead, `RateLimitGuard.Allow` is a plain method the handler calls explicitly, after it has already validated headers and body, once it knows which `X-Api-Key` to charge.

### What pgxmock and miniredis prove, and what they don't

Both dependencies are mocked for the automated test suite so it runs in milliseconds with no infrastructure: `pgxmock` intercepts every SQL statement the store issues and lets a test assert the exact query shape and simulate a `23505` unique-violation without a real Postgres server, and `miniredis` is an in-process, protocol-compatible fake Redis server, so the Lua rate-limit script actually executes as real Lua against a real (fake) `EVAL` implementation, not a stub. This proves the application code drives its dependencies correctly: the right SQL with the right arguments in the right order inside the right transaction boundaries, and the right Redis commands with the right key structure. What it cannot prove is that Postgres's unique index truly serializes concurrent inserts, or that a live Redis server's `EVAL` is truly atomic under real network concurrency — those guarantees come from Postgres and Redis themselves, which is exactly why this project also ships a `docker-compose.yml` to run the real thing.

### Graceful shutdown

A payment create can be mid-flight in several ways: the idempotency-key row inserted but the transaction not yet committed, or the handler blocked waiting on Postgres or Redis. Killing the process outright severs that request and its client gets nothing — not even an error — leaving them unable to tell whether the payment was created. `signal.NotifyContext` catches `SIGINT`/`SIGTERM` and cancels a context instead of letting the default handler terminate the process immediately; `http.Server.Shutdown` then stops accepting new connections and waits (up to a bounded timeout) for in-flight requests to finish normally, so a payment creation that is already inside its transaction gets to complete and return a real response instead of being cut off mid-write.

### go.mod

Create `go.mod`:

```go
module example.com/payments-gateway

go 1.26

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/pashagolub/pgxmock/v3 v3.4.0
	github.com/redis/go-redis/v9 v9.21.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
```

### internal/domain/money.go

`Money` is the value object every amount in this system passes through. It rejects non-positive amounts and malformed currency codes at construction, with typed sentinel errors the handler layer maps to specific error codes.

Create `internal/domain/money.go`:

```go
package domain

import (
	"errors"
	"regexp"
)

// ErrInvalidAmount is returned when amount_minor is not a positive integer.
var ErrInvalidAmount = errors.New("amount_minor must be a positive integer")

// ErrInvalidCurrency is returned when currency is not a 3-letter uppercase
// ISO-4217-shaped code.
var ErrInvalidCurrency = errors.New("currency must be a 3-letter uppercase ISO 4217 code")

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Money is an integer minor-unit amount (e.g. cents) paired with a currency
// code. It is deliberately not a float: floating point cannot represent every
// decimal minor-unit amount exactly, and repeated arithmetic on money drifts.
type Money struct {
	AmountMinor int64
	Currency    string
}

// NewMoney validates amountMinor and currency and returns a Money, or a typed
// domain error identifying exactly which field was invalid.
func NewMoney(amountMinor int64, currency string) (Money, error) {
	if amountMinor <= 0 {
		return Money{}, ErrInvalidAmount
	}
	if !currencyPattern.MatchString(currency) {
		return Money{}, ErrInvalidCurrency
	}
	return Money{AmountMinor: amountMinor, Currency: currency}, nil
}
```

### internal/domain/payment.go

`Payment` is the durable record; `PaymentStatus` documents the full lifecycle a settlement worker would drive, even though this project only ever writes `pending`.

Create `internal/domain/payment.go`:

```go
package domain

import "time"

// PaymentStatus is the lifecycle state of a Payment. This project only ever
// writes Pending at creation time; Succeeded and Failed exist because the
// wire format documents the full state machine a settlement worker would
// drive later, even though that worker is out of scope here.
type PaymentStatus string

const (
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
)

// Payment is the durable record created by POST /v1/payments and returned by
// GET /v1/payments/{id}. Field tags fix the wire shape the gateway promises.
type Payment struct {
	ID          string        `json:"id"`
	AmountMinor int64         `json:"amount_minor"`
	Currency    string        `json:"currency"`
	Status      PaymentStatus `json:"status"`
	CreatedAt   time.Time     `json:"created_at"`
}
```

### internal/store/postgres.go

The repository. `DBPool` is the minimal interface the store needs from a pgx pool — `Begin`, `Exec`, `QueryRow`, `Ping` — which both `*pgxpool.Pool` and `pgxmock.PgxPoolIface` satisfy structurally, so tests substitute the mock with no adapter layer. `CreatePayment` is the core of the project: it attempts the insert inside a transaction and only falls back to the replay-or-conflict path on an actual unique-violation from Postgres, never on a pre-check.

Create `internal/store/postgres.go`:

```go
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"example.com/payments-gateway/internal/domain"
)

// ErrIdempotencyConflict is returned when an Idempotency-Key is reused with a
// request payload whose fingerprint does not match the original request.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different payload")

// ErrPaymentNotFound is returned when no payment matches the requested id.
var ErrPaymentNotFound = errors.New("payment not found")

const uniqueViolationCode = "23505"

// DBPool is the subset of *pgxpool.Pool this store depends on. It is
// satisfied both by *pgxpool.Pool in production and by pgxmock.PgxPoolIface
// in tests, so the store never has to branch on which one it was given.
type DBPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Ping(ctx context.Context) error
}

// Store is the idempotency-aware repository backing the payments gateway.
type Store struct {
	pool DBPool
}

// NewStore wraps a pool. The pool is owned by the caller (main in
// production, a pgxmock pool in tests); Store never closes it.
func NewStore(pool DBPool) *Store {
	return &Store{pool: pool}
}

// Migrate creates the two tables the gateway needs if they do not already
// exist. It is idempotent and safe to run on every boot.
func (s *Store) Migrate(ctx context.Context) error {
	const payments = `CREATE TABLE IF NOT EXISTS payments (
	id TEXT PRIMARY KEY,
	amount_minor BIGINT NOT NULL,
	currency TEXT NOT NULL,
	status TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`
	const idempotencyKeys = `CREATE TABLE IF NOT EXISTS idempotency_keys (
	key TEXT PRIMARY KEY,
	fingerprint TEXT NOT NULL,
	payment_id TEXT NOT NULL,
	response_body JSONB NOT NULL,
	response_status INT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`
	if _, err := s.pool.Exec(ctx, payments); err != nil {
		return fmt.Errorf("migrate payments table: %w", err)
	}
	if _, err := s.pool.Exec(ctx, idempotencyKeys); err != nil {
		return fmt.Errorf("migrate idempotency_keys table: %w", err)
	}
	return nil
}

// Ping verifies the pool can reach Postgres, for the health check.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// fingerprintOf hashes the normalized fields of a create request. Two
// requests with the same Idempotency-Key must produce the same fingerprint
// to be treated as a replay; any difference is a conflict.
func fingerprintOf(amountMinor int64, currency, description string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d|%s|%s", amountMinor, currency, description))
	return hex.EncodeToString(sum[:])
}

// FingerprintForTest exposes fingerprintOf to other packages' tests, so a
// test fixture can compute the exact fingerprint a given payload would
// produce without duplicating (and risking drifting from) the hash format.
func FingerprintForTest(amountMinor int64, currency, description string) string {
	return fingerprintOf(amountMinor, currency, description)
}

// newPaymentID generates an opaque, unguessable payment identifier.
func newPaymentID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate payment id: %w", err)
	}
	return "pay_" + hex.EncodeToString(b), nil
}

// CreatePayment performs the idempotent create. It returns the exact response
// body to send to the client and the HTTP status to send it with:
//
//   - a fresh key inserts a new payment and returns (body, 201, nil)
//   - a replayed key+payload returns the originally stored body, (body, 200, nil)
//   - a reused key with a different payload returns (nil, 0, ErrIdempotencyConflict)
//
// The fresh-vs-replay decision is made by attempting the insert and reacting
// to a unique-constraint violation, never by a pre-check SELECT: a pre-check
// has a race window between the check and the insert that two concurrent
// requests with the same key can both pass through, ending up with two
// payment rows. The unique index is the only thing that can atomically
// arbitrate which of two concurrent inserts wins.
func (s *Store) CreatePayment(ctx context.Context, idempotencyKey string, money domain.Money, description string) ([]byte, int, error) {
	fingerprint := fingerprintOf(money.AmountMinor, money.Currency, description)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	paymentID, err := newPaymentID()
	if err != nil {
		return nil, 0, err
	}
	payment := domain.Payment{
		ID:          paymentID,
		AmountMinor: money.AmountMinor,
		Currency:    money.Currency,
		Status:      domain.PaymentPending,
		CreatedAt:   time.Now().UTC(),
	}
	body, err := json.Marshal(payment)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal payment: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, fingerprint, payment_id, response_body, response_status)
		 VALUES ($1, $2, $3, $4, $5)`,
		idempotencyKey, fingerprint, payment.ID, body, http.StatusCreated,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
			// Someone already holds this key (a real request, or a concurrent
			// racer that won). Roll back this attempt before reading the
			// row it collided with: Postgres has put the transaction in an
			// aborted state, so no further statement on it would succeed.
			_ = tx.Rollback(ctx)
			committed = true // the deferred rollback above must not run twice
			return s.replayOrConflict(ctx, idempotencyKey, fingerprint)
		}
		return nil, 0, fmt.Errorf("insert idempotency key: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (id, amount_minor, currency, status, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		payment.ID, payment.AmountMinor, payment.Currency, string(payment.Status), payment.CreatedAt,
	); err != nil {
		return nil, 0, fmt.Errorf("insert payment: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit: %w", err)
	}
	committed = true

	return body, http.StatusCreated, nil
}

// replayOrConflict is reached only after a unique-constraint violation on
// idempotency_keys.key: some row for this key already exists. It compares
// fingerprints to tell a legitimate replay from a conflicting reuse.
func (s *Store) replayOrConflict(ctx context.Context, idempotencyKey, fingerprint string) ([]byte, int, error) {
	var storedFingerprint string
	var storedBody []byte
	row := s.pool.QueryRow(ctx,
		`SELECT fingerprint, response_body FROM idempotency_keys WHERE key = $1`, idempotencyKey)
	if err := row.Scan(&storedFingerprint, &storedBody); err != nil {
		return nil, 0, fmt.Errorf("look up idempotency key: %w", err)
	}
	if storedFingerprint != fingerprint {
		return nil, 0, ErrIdempotencyConflict
	}
	return storedBody, http.StatusOK, nil
}

// GetPayment looks up a payment by id, or ErrPaymentNotFound.
func (s *Store) GetPayment(ctx context.Context, id string) (domain.Payment, error) {
	var payment domain.Payment
	var status string
	row := s.pool.QueryRow(ctx,
		`SELECT id, amount_minor, currency, status, created_at FROM payments WHERE id = $1`, id)
	if err := row.Scan(&payment.ID, &payment.AmountMinor, &payment.Currency, &status, &payment.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Payment{}, ErrPaymentNotFound
		}
		return domain.Payment{}, fmt.Errorf("get payment %s: %w", id, err)
	}
	payment.Status = domain.PaymentStatus(status)
	return payment, nil
}
```

### internal/ratelimit/limiter.go

The rate limiter. `Limiter.Allow` runs the token-bucket Lua script atomically against `redis.Cmdable` (satisfied by `*redis.Client` in production and a miniredis-backed client in tests); `Limiter.AllowFailOpen` wraps it with this project's deliberate fail-open policy.

Create `internal/ratelimit/limiter.go`:

```go
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// script implements a token-bucket limiter (the same family as GCRA: a smooth
// sustained rate with a tolerated burst) as a single atomic Redis command.
// Reading the bucket's current state, refilling it for elapsed time, and
// writing back the debited state all happen inside one EVAL, so two
// concurrent requests for the same key can never interleave the way a
// GET-then-INCR pair can: with GET-then-INCR, two requests can both read the
// same "9 of 10 used" state and both decide they are allowed, over-admitting
// the bucket. EVAL runs the whole script as one atomic step on the Redis
// server, so there is no window between reading and writing.
const script = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local requested = tonumber(ARGV[4])

local bucket = redis.call("HMGET", key, "tokens", "timestamp")
local tokens = tonumber(bucket[1])
local timestamp = tonumber(bucket[2])

if tokens == nil then
	tokens = burst
	timestamp = now
end

local elapsed = math.max(0, now - timestamp)
tokens = math.min(burst, tokens + (elapsed / 1000.0) * rate)

local allowed = 0
local retry_after_ms = 0

if tokens >= requested then
	tokens = tokens - requested
	allowed = 1
else
	local deficit = requested - tokens
	retry_after_ms = math.ceil((deficit / rate) * 1000.0)
end

redis.call("HMSET", key, "tokens", tostring(tokens), "timestamp", tostring(now))
redis.call("PEXPIRE", key, math.ceil((burst / rate) * 1000.0) + 1000)

return {allowed, tostring(tokens), retry_after_ms}
`

// Result is the outcome of a single Allow check.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Limiter enforces a per-key token-bucket rate limit backed by Redis. It
// depends on redis.Cmdable rather than *redis.Client, so tests substitute a
// miniredis-backed client and production passes a real *redis.Client,
// interchangeably.
type Limiter struct {
	rdb   redis.Cmdable
	rate  float64
	burst float64
	sha   *redis.Script
}

// New builds a Limiter admitting ratePerSecond sustained requests per key,
// tolerating a burst up to burst tokens above that rate.
func New(rdb redis.Cmdable, ratePerSecond, burst float64) *Limiter {
	return &Limiter{rdb: rdb, rate: ratePerSecond, burst: burst, sha: redis.NewScript(script)}
}

// Burst reports the bucket capacity, used for the RateLimit-Limit header.
func (l *Limiter) Burst() int {
	return int(l.burst)
}

// ResetSeconds reports how long a fully drained bucket takes to refill,
// rounded up. It is used for the RateLimit-Reset header.
func (l *Limiter) ResetSeconds() int {
	return int(math.Ceil(l.burst / l.rate))
}

// Allow evaluates the rate-limit script for key, costing one token. A
// non-nil error means Redis itself is unavailable or misbehaving; it carries
// no verdict about whether the request should be allowed. Callers decide the
// policy for that case — see AllowFailOpen for the policy this gateway uses.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	now := float64(time.Now().UnixMilli())
	res, err := l.sha.Run(ctx, l.rdb, []string{"ratelimit:" + key}, l.rate, l.burst, now, 1).Slice()
	if err != nil {
		return Result{}, fmt.Errorf("run rate limit script: %w", err)
	}
	if len(res) != 3 {
		return Result{}, fmt.Errorf("rate limit script: unexpected response shape %v", res)
	}
	allowed, ok1 := res[0].(int64)
	remainingStr, ok2 := res[1].(string)
	retryAfterMs, ok3 := res[2].(int64)
	if !ok1 || !ok2 || !ok3 {
		return Result{}, fmt.Errorf("rate limit script: unexpected response types %#v", res)
	}
	var remaining float64
	if _, err := fmt.Sscanf(remainingStr, "%g", &remaining); err != nil {
		return Result{}, fmt.Errorf("rate limit script: parse remaining %q: %w", remainingStr, err)
	}
	return Result{
		Allowed:    allowed == 1,
		Remaining:  int(math.Floor(remaining)),
		RetryAfter: time.Duration(retryAfterMs) * time.Millisecond,
	}, nil
}

// AllowFailOpen wraps Allow with this gateway's deliberate degradation
// policy: if Redis is unreachable, log a warning and allow the request
// rather than blocking payment creation on the limiter's own availability.
// Postgres-level idempotency is the correctness backstop for payment
// creation; the limiter only protects capacity, so an outage of the limiter
// should not become an outage of checkout. This is a named method, not a
// fallthrough in a generic error branch, so the trade-off is visible at the
// call site and easy to find and reverse.
func (l *Limiter) AllowFailOpen(ctx context.Context, key string, logger *slog.Logger) Result {
	result, err := l.Allow(ctx, key)
	if err != nil {
		logger.WarnContext(ctx, "rate limiter unavailable, failing open", "error", err)
		return Result{Allowed: true, Remaining: l.Burst()}
	}
	return result
}
```

### internal/api/middleware.go

The error envelope, the panic-recovery and request-logging middleware (composed with `Use`, `Recover` outermost), and `RateLimitGuard`, the explicit (not chained) rate-limit check the create handler calls once it has validated the request enough to be worth charging quota for.

Create `internal/api/middleware.go`:

```go
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"example.com/payments-gateway/internal/ratelimit"
)

// apiError and errorEnvelope define the error envelope every error response
// in this gateway uses: {"error": {"code": "...", "message": "..."}}.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: apiError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Recover is the outermost middleware: a panic anywhere below it (including
// inside RequestLogger or a handler) is caught here, logged, and turned into
// a 500 with the standard error envelope instead of crashing the process or
// leaking a bare stack trace to the client. It must wrap everything else, so
// it is always listed first in the Use call in server.go.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(r.Context(), "panic recovered", "panic", rec)
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusWriter captures the status code a handler wrote, so RequestLogger can
// log it even though http.ResponseWriter has no getter for it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// RequestLogger logs one structured line per request: method, path, status,
// and duration. It sits inside Recover, so a panic is logged once by Recover
// with the panic value, and RequestLogger simply never gets to log that
// request (there is no double-logging to reconcile).
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			logger.InfoContext(r.Context(), "request",
				"method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
		})
	}
}

// Use composes middleware around a handler. The first middleware listed ends
// up outermost, so Use(mux, Recover(logger), RequestLogger(logger)) runs
// Recover, then RequestLogger, then mux — matching how they are written.
func Use(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RateLimitGuard checks a request against the rate limiter and, when denied,
// writes the 429 response itself. It is deliberately not chained as generic
// http.Handler middleware: this gateway's ordering requires the JSON body to
// be decoded and validated (422) before the rate-limit check (429) runs, and
// a mux-level middleware would run before the handler's body ever executes.
// Instead handlers.go calls Allow explicitly, after its own header and body
// validation, once it knows which X-Api-Key to charge.
type RateLimitGuard struct {
	limiter *ratelimit.Limiter
	logger  *slog.Logger
}

// NewRateLimitGuard builds a guard around limiter.
func NewRateLimitGuard(limiter *ratelimit.Limiter, logger *slog.Logger) *RateLimitGuard {
	return &RateLimitGuard{limiter: limiter, logger: logger}
}

// Allow checks apiKey, always setting the RateLimit-* headers. When the
// request is denied it also sets Retry-After and writes the 429 response,
// and returns false — the caller must stop processing without writing
// anything else. On limiter failure (Redis down) it fails open per
// Limiter.AllowFailOpen and returns true.
func (g *RateLimitGuard) Allow(w http.ResponseWriter, r *http.Request, apiKey string) bool {
	result := g.limiter.AllowFailOpen(r.Context(), apiKey, g.logger)

	w.Header().Set("RateLimit-Limit", strconv.Itoa(g.limiter.Burst()))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(result.Remaining))
	w.Header().Set("RateLimit-Reset", strconv.Itoa(g.limiter.ResetSeconds()))
	if !result.Allowed {
		retryAfter := int(result.RetryAfter.Seconds())
		if result.RetryAfter%time.Second != 0 {
			retryAfter++
		}
		if retryAfter < 1 {
			retryAfter = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests, retry later")
		return false
	}
	return true
}
```

### internal/api/handlers.go

The three HTTP handlers. `CreatePayment` enforces the required order: header presence (`400`), body decode+validation (`422`), rate limit (`429`), then the idempotent create (`201`/`200`/`409`). `GetPayment` and `Healthz` are straightforward by comparison.

Create `internal/api/handlers.go`:

```go
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"

	"example.com/payments-gateway/internal/domain"
	"example.com/payments-gateway/internal/ratelimit"
	"example.com/payments-gateway/internal/store"
)

// Handlers holds the three HTTP handlers' shared dependencies.
type Handlers struct {
	store   *store.Store
	limiter *RateLimitGuard
	rdb     redis.Cmdable
	logger  *slog.Logger
}

// NewHandlers wires the dependencies the three endpoints need.
func NewHandlers(st *store.Store, limiter *ratelimit.Limiter, rdb redis.Cmdable, logger *slog.Logger) *Handlers {
	return &Handlers{
		store:   st,
		limiter: NewRateLimitGuard(limiter, logger),
		rdb:     rdb,
		logger:  logger,
	}
}

type createPaymentRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Description string `json:"description,omitempty"`
}

// CreatePayment implements POST /v1/payments. Order matters here: header
// checks (400) run first since they are cheap endpoint-shape validation;
// then body decode+validation (422); then the rate-limit check (429), run
// only once the request is well-formed enough to be worth charging quota
// for; then the idempotent create against Postgres (201/200/409).
func (h *Handlers) CreatePayment(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	apiKey := r.Header.Get("X-Api-Key")
	if apiKey == "" {
		writeError(w, http.StatusBadRequest, "missing_api_key", "X-Api-Key header is required")
		return
	}

	var req createPaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request_body", "request body must be valid JSON matching the payment schema")
		return
	}

	money, err := domain.NewMoney(req.AmountMinor, req.Currency)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidAmount):
			writeError(w, http.StatusUnprocessableEntity, "invalid_amount", err.Error())
		case errors.Is(err, domain.ErrInvalidCurrency):
			writeError(w, http.StatusUnprocessableEntity, "invalid_currency", err.Error())
		default:
			writeError(w, http.StatusUnprocessableEntity, "invalid_payment", err.Error())
		}
		return
	}

	if !h.limiter.Allow(w, r, apiKey) {
		return
	}

	body, status, err := h.store.CreatePayment(r.Context(), idempotencyKey, money, req.Description)
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_key_conflict", "Idempotency-Key was reused with a different payload")
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "create payment failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create payment")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// GetPayment implements GET /v1/payments/{id}.
func (h *Handlers) GetPayment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	payment, err := h.store.GetPayment(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrPaymentNotFound) {
			writeError(w, http.StatusNotFound, "payment_not_found", "no payment with that id")
			return
		}
		h.logger.ErrorContext(r.Context(), "get payment failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to fetch payment")
		return
	}
	writeJSON(w, http.StatusOK, payment)
}

// Healthz implements GET /healthz: 200 only if both Postgres and Redis are
// reachable, 503 with a per-dependency detail otherwise.
func (h *Handlers) Healthz(w http.ResponseWriter, r *http.Request) {
	dbErr := h.store.Ping(r.Context())
	redisErr := h.rdb.Ping(r.Context()).Err()

	if dbErr == nil && redisErr == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	details := map[string]string{"status": "degraded"}
	if dbErr != nil {
		details["postgres"] = dbErr.Error()
	}
	if redisErr != nil {
		details["redis"] = redisErr.Error()
	}
	writeJSON(w, http.StatusServiceUnavailable, details)
}
```

### internal/api/server.go

Wires the three routes onto a stdlib `http.ServeMux` using Go 1.22+ method+path patterns, then applies the middleware stack.

Create `internal/api/server.go`:

```go
package api

import (
	"log/slog"
	"net/http"

	"github.com/redis/go-redis/v9"

	"example.com/payments-gateway/internal/ratelimit"
	"example.com/payments-gateway/internal/store"
)

// NewServer wires the three endpoints onto a stdlib ServeMux using Go 1.22+
// method+path patterns, then wraps it with panic-recovery (outermost) and
// request logging. No router framework: the patterns and r.PathValue are
// enough for three routes.
func NewServer(st *store.Store, limiter *ratelimit.Limiter, rdb redis.Cmdable, logger *slog.Logger) http.Handler {
	h := NewHandlers(st, limiter, rdb, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/payments", h.CreatePayment)
	mux.HandleFunc("GET /v1/payments/{id}", h.GetPayment)
	mux.HandleFunc("GET /healthz", h.Healthz)

	return Use(mux, Recover(logger), RequestLogger(logger))
}
```

### cmd/server/main.go

The real entrypoint. `run(ctx, cfg, logger) error` holds all the actual work so it can be tested with a cancellable context without calling `os.Exit`; `main()` is a thin wrapper that wires signals and exits non-zero on error.

Create `cmd/server/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"example.com/payments-gateway/internal/api"
	"example.com/payments-gateway/internal/ratelimit"
	"example.com/payments-gateway/internal/store"
)

// config is read once at boot from the environment. Real deployments set
// DATABASE_URL and REDIS_ADDR; PORT defaults to 8080 and REDIS_ADDR defaults
// to localhost:6379 so the binary is easy to run against docker-compose.yml.
type config struct {
	databaseURL string
	redisAddr   string
	port        string
}

func loadConfig() config {
	cfg := config{
		databaseURL: os.Getenv("DATABASE_URL"),
		redisAddr:   os.Getenv("REDIS_ADDR"),
		port:        os.Getenv("PORT"),
	}
	if cfg.port == "" {
		cfg.port = "8080"
	}
	if cfg.redisAddr == "" {
		cfg.redisAddr = "localhost:6379"
	}
	return cfg
}

// run holds all of main's real work behind a context, kept separate from
// main() so it can be exercised from a test with a cancellable context and
// without calling os.Exit.
func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	if cfg.databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	defer func() { _ = rdb.Close() }()

	repo := store.NewStore(pool)
	if err := repo.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	limiter := ratelimit.New(rdb, 5, 10)
	handler := api.NewServer(repo, limiter, rdb, logger)

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		// A payment create may be mid-flight (already inserted the
		// idempotency key, not yet committed, or waiting on Postgres/Redis).
		// Shutdown stops accepting new connections and gives in-flight
		// requests a bounded window to finish instead of severing them.
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return <-serveErr
	case err := <-serveErr:
		return err
	}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, loadConfig(), logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}
```

### internal/api/gateway_test.go

The offline end-to-end suite: `pgxmock` stands in for Postgres and asserts the exact SQL and arguments for the fresh/replay/conflict paths, and `miniredis` stands in for Redis so the token-bucket Lua script runs for real. `httptest` drives all three endpoints through the fully wired `NewServer`.

Create `internal/api/gateway_test.go`:

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v3"
	"github.com/redis/go-redis/v9"

	"example.com/payments-gateway/internal/domain"
	"example.com/payments-gateway/internal/ratelimit"
	"example.com/payments-gateway/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(bytesDiscard{}, nil))
}

// bytesDiscard is an io.Writer that discards everything, keeping test output
// quiet without importing io.Discard's exact type in every call site.
type bytesDiscard struct{}

func (bytesDiscard) Write(p []byte) (int, error) { return len(p), nil }

func newTestLimiter(t *testing.T) *ratelimit.Limiter {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	// Generous limit so functional tests never trip it by accident; the
	// rate-limit exceeded test builds its own tight limiter.
	return ratelimit.New(rdb, 1000, 1000)
}

func newTestServer(t *testing.T, pool store.DBPool, limiter *ratelimit.Limiter) http.Handler {
	t.Helper()
	if limiter == nil {
		limiter = newTestLimiter(t)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewServer(store.NewStore(pool), limiter, rdb, testLogger())
}

func decodeError(t *testing.T, body *bytes.Buffer) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body: %s)", err, body.String())
	}
	return env
}

func TestCreatePayment_Fresh(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO idempotency_keys").
		WithArgs("key-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), http.StatusCreated).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("INSERT INTO payments").
		WithArgs(pgxmock.AnyArg(), int64(1500), "USD", "pending", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	srv := newTestServer(t, mock, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(
		`{"amount_minor":1500,"currency":"USD","description":"widget"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	req.Header.Set("X-Api-Key", "client-a")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var payment domain.Payment
	if err := json.Unmarshal(rec.Body.Bytes(), &payment); err != nil {
		t.Fatalf("decode payment: %v", err)
	}
	if payment.AmountMinor != 1500 || payment.Currency != "USD" || payment.Status != domain.PaymentPending {
		t.Fatalf("unexpected payment: %+v", payment)
	}
	if payment.ID == "" {
		t.Fatal("expected a generated payment id")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCreatePayment_ReplaySamePayload(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()

	storedBody := []byte(`{"id":"pay_existing","amount_minor":1500,"currency":"USD","status":"pending","created_at":"2024-01-01T00:00:00Z"}`)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO idempotency_keys").
		WithArgs("key-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), http.StatusCreated).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"})
	mock.ExpectRollback()
	rows := mock.NewRows([]string{"fingerprint", "response_body"}).
		AddRow(fingerprintForTest(1500, "USD", "widget"), storedBody)
	mock.ExpectQuery("SELECT fingerprint, response_body FROM idempotency_keys").
		WithArgs("key-1").
		WillReturnRows(rows)

	srv := newTestServer(t, mock, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(
		`{"amount_minor":1500,"currency":"USD","description":"widget"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	req.Header.Set("X-Api-Key", "client-a")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Equal(bytes.TrimSpace(rec.Body.Bytes()), storedBody) {
		t.Fatalf("replay body = %s, want the original stored body %s", rec.Body.String(), storedBody)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCreatePayment_ReusedKeyDifferentPayload(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO idempotency_keys").
		WithArgs("key-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), http.StatusCreated).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"})
	mock.ExpectRollback()
	rows := mock.NewRows([]string{"fingerprint", "response_body"}).
		AddRow(fingerprintForTest(1500, "USD", "widget"), []byte(`{"id":"pay_existing"}`))
	mock.ExpectQuery("SELECT fingerprint, response_body FROM idempotency_keys").
		WithArgs("key-1").
		WillReturnRows(rows)

	srv := newTestServer(t, mock, nil)

	// Different amount than the fingerprint above was computed for.
	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(
		`{"amount_minor":2500,"currency":"USD","description":"widget"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	req.Header.Set("X-Api-Key", "client-a")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	env := decodeError(t, rec.Body)
	if env.Error.Code != "idempotency_key_conflict" {
		t.Fatalf("error code = %q, want idempotency_key_conflict", env.Error.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCreatePayment_MissingHeaders(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()
	srv := newTestServer(t, mock, nil)

	cases := []struct {
		name           string
		idempotencyKey string
		apiKey         string
		wantErrorCode  string
	}{
		{"missing idempotency key", "", "client-a", "missing_idempotency_key"},
		{"missing api key", "key-1", "", "missing_api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(
				`{"amount_minor":1500,"currency":"USD"}`))
			if tc.idempotencyKey != "" {
				req.Header.Set("Idempotency-Key", tc.idempotencyKey)
			}
			if tc.apiKey != "" {
				req.Header.Set("X-Api-Key", tc.apiKey)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
			env := decodeError(t, rec.Body)
			if env.Error.Code != tc.wantErrorCode {
				t.Fatalf("error code = %q, want %q", env.Error.Code, tc.wantErrorCode)
			}
		})
	}
}

func TestCreatePayment_InvalidAmount(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()
	srv := newTestServer(t, mock, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(
		`{"amount_minor":0,"currency":"USD"}`))
	req.Header.Set("Idempotency-Key", "key-1")
	req.Header.Set("X-Api-Key", "client-a")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeError(t, rec.Body)
	if env.Error.Code != "invalid_amount" {
		t.Fatalf("error code = %q, want invalid_amount", env.Error.Code)
	}
}

func TestGetPayment_Found(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()

	createdAt := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := mock.NewRows([]string{"id", "amount_minor", "currency", "status", "created_at"}).
		AddRow("pay_abc", int64(500), "EUR", "pending", createdAt)
	mock.ExpectQuery("SELECT id, amount_minor, currency, status, created_at FROM payments").
		WithArgs("pay_abc").
		WillReturnRows(rows)

	srv := newTestServer(t, mock, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/payments/pay_abc", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var payment domain.Payment
	if err := json.Unmarshal(rec.Body.Bytes(), &payment); err != nil {
		t.Fatalf("decode payment: %v", err)
	}
	if payment.ID != "pay_abc" || payment.AmountMinor != 500 || payment.Currency != "EUR" {
		t.Fatalf("unexpected payment: %+v", payment)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetPayment_NotFound(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()

	rows := mock.NewRows([]string{"id", "amount_minor", "currency", "status", "created_at"})
	mock.ExpectQuery("SELECT id, amount_minor, currency, status, created_at FROM payments").
		WithArgs("pay_missing").
		WillReturnRows(rows)

	srv := newTestServer(t, mock, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/payments/pay_missing", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeError(t, rec.Body)
	if env.Error.Code != "payment_not_found" {
		t.Fatalf("error code = %q, want payment_not_found", env.Error.Code)
	}
}

func TestCreatePayment_RateLimitExceeded(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	// burst of 1: the first request consumes the only token, the second is denied.
	limiter := ratelimit.New(rdb, 1, 1)

	srv := NewServer(store.NewStore(mock), limiter, rdb, testLogger())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO idempotency_keys").
		WithArgs("key-a", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), http.StatusCreated).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("INSERT INTO payments").
		WithArgs(pgxmock.AnyArg(), int64(1000), "USD", "pending", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	makeReq := func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader(
			`{"amount_minor":1000,"currency":"USD"}`))
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("X-Api-Key", "client-b")
		return req
	}

	first := httptest.NewRecorder()
	srv.ServeHTTP(first, makeReq("key-a"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first request status = %d, want 201, body=%s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	srv.ServeHTTP(second, makeReq("key-b"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429, body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header on the 429 response")
	}
	env := decodeError(t, second.Body)
	if env.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want rate_limit_exceeded", env.Error.Code)
	}
}

func TestHealthz_OK(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()
	mock.ExpectPing()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	srv := NewServer(store.NewStore(mock), ratelimit.New(rdb, 5, 10), rdb, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthz_Degraded(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer mock.Close()
	mock.ExpectPing().WillReturnError(context.DeadlineExceeded)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // Redis is now unreachable through this client.

	srv := NewServer(store.NewStore(mock), ratelimit.New(rdb, 5, 10), rdb, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

func TestRateLimiter_FailsOpenWhenRedisUnreachable(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // simulate Redis being down

	limiter := ratelimit.New(rdb, 5, 10)
	result := limiter.AllowFailOpen(t.Context(), "any-client", testLogger())
	if !result.Allowed {
		t.Fatal("expected AllowFailOpen to allow the request when Redis is unreachable")
	}
}

// fingerprintForTest reproduces store's unexported fingerprint format for
// test fixtures. It intentionally mirrors fingerprintOf's field order and
// separators; a change to one without the other should be caught by
// TestCreatePayment_ReplaySamePayload failing.
func fingerprintForTest(amountMinor int64, currency, description string) string {
	return store.FingerprintForTest(amountMinor, currency, description)
}
```

### Running the real thing

`docker-compose.yml` brings up a real Postgres and a real Redis with healthchecks, so the true unique-constraint enforcement and true `EVAL` atomicity the mocks stand in for during tests are exercised for real. Point `main()` at them with `DATABASE_URL` and `REDIS_ADDR`.

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: gateway
      POSTGRES_PASSWORD: gateway
      POSTGRES_DB: payments
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U gateway -d payments"]
      interval: 5s
      timeout: 5s
      retries: 5

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 5s
      retries: 5
```

```bash
export DATABASE_URL="postgres://gateway:gateway@localhost:5432/payments"
export REDIS_ADDR="localhost:6379"
export PORT=8080
docker compose up -d
go run ./cmd/server
```

Create a payment:

```bash
curl -i -X POST http://localhost:8080/v1/payments \
  -H 'Idempotency-Key: 3f9a-order-9182' \
  -H 'X-Api-Key: client-42' \
  -H 'Content-Type: application/json' \
  -d '{"amount_minor": 1999, "currency": "USD", "description": "annual plan"}'

HTTP/1.1 201 Created
Content-Type: application/json

{"id":"pay_1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d","amount_minor":1999,"currency":"USD","status":"pending","created_at":"2026-07-05T18:04:11Z"}
```

Replay with the same `Idempotency-Key` and the identical payload:

```bash
curl -i -X POST http://localhost:8080/v1/payments \
  -H 'Idempotency-Key: 3f9a-order-9182' \
  -H 'X-Api-Key: client-42' \
  -H 'Content-Type: application/json' \
  -d '{"amount_minor": 1999, "currency": "USD", "description": "annual plan"}'

HTTP/1.1 200 OK
Content-Type: application/json

{"id":"pay_1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d","amount_minor":1999,"currency":"USD","status":"pending","created_at":"2026-07-05T18:04:11Z"}
```

Get it by id:

```bash
curl -i http://localhost:8080/v1/payments/pay_1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d

HTTP/1.1 200 OK
Content-Type: application/json

{"id":"pay_1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d","amount_minor":1999,"currency":"USD","status":"pending","created_at":"2026-07-05T18:04:11Z"}
```

## Review

The mistake that reintroduces the exact race idempotency keys exist to close is a pre-check `SELECT` followed by a separate `INSERT`: two concurrent requests carrying the same key can both see "no row yet" and both insert, producing two payments for one logical request. The fix is to attempt the insert first and treat a unique-constraint violation (`pgconn.PgError` with `Code == "23505"`) as the signal to look up what already exists, inside the same transaction boundary the insert used. The same shape of bug shows up in rate limiting as GET-then-INCR: two requests can read the same counter value before either writes it back, both concluding they are under budget. An atomic `EVAL` running a Lua script closes that window the same way the unique index does for Postgres.

A second common mistake is forgetting the fingerprint check entirely and treating any request with a known key as an automatic replay — that silently returns the wrong payment's data to a caller who reused a key by mistake with a different payload, instead of surfacing the conflict as a `409`. A third is representing money as a float, which drifts under arithmetic and can round a charge to the wrong amount; minor-unit integers do not have that failure mode. A fourth is fail-closed rate limiting: if a limiter takes down request handling the moment Redis is unreachable, an outage in a capacity-protection system becomes an outage in checkout, which is a worse failure than temporarily under-limiting. A fifth is skipping graceful shutdown, so a deploy or a scale-down event severs in-flight payment creations mid-transaction instead of giving them a bounded window to finish and respond.

To confirm the idempotency logic is actually correct rather than merely well-intentioned, look at the exact SQL and argument sequence the `pgxmock` expectations assert in the test suite — the insert attempt, the simulated `23505`, the rollback, and the subsequent fingerprint lookup, all in the order the store code actually issues them. To confirm the rate limiter's atomicity claim rather than just trusting the Lua, read the script itself: every read of the bucket's state and every write back to it happens inside the same `EVAL`, with no Go-side round trip in between. And to confirm both systems' real guarantees rather than only their mocked stand-ins, run the suite again against the `docker-compose.yml` Postgres and Redis — the unique index and the `EVAL` atomicity are properties of those real systems, not of `pgxmock` or `miniredis`, which only prove the application drives them correctly.

## Resources

- [Brandur Ozols: Idempotency keys](https://brandur.org/idempotency-keys) — the canonical description of the fingerprint-plus-unique-constraint pattern this project implements.
- [Stripe: Designing robust and predictable APIs with idempotency](https://stripe.com/blog/idempotency) — how a production payments API applies the same pattern at scale.
- [Brandur Ozols: Rate limiting, Cells, and GCRA](https://brandur.org/rate-limiting) — the token-bucket/GCRA algorithm this project's Lua script implements, and why a hard fixed window is the wrong shape for most APIs.
- [pashagolub/pgxmock](https://github.com/pashagolub/pgxmock) — the pgx-compatible mock this project's tests use to assert exact SQL and simulate a unique-violation without a live Postgres server.
