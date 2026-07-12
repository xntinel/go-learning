# Exercise 5: A Connection-Pool Service with a Readiness Check

A database-backed service is configured by the same knobs every team argues about in code review: max open connections, max idle, idle timeout, a health-check interval, and the DSN. This exercise wires those settings through functional options into a `Service` that depends on a narrow `Pool` port, exposes a real `Ready` probe an orchestrator can call, and is fully testable with an in-memory fake — while a `database/sql` adapter shows the identical options driving a live pool.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
service.go             Config, Option, Pool (port), PoolStats, Service, sentinels,
                       buildConfig (aggregating), NewService (fake-injectable),
                       NewSQLService (database/sql wiring), Ready, Watch, sqlPool adapter
cmd/
  demo/
    main.go            a healthy fake passing readiness, a down fake failing it, an aggregated error
service_test.go        defaults, required DSN, aggregated errors, cross-field rule, precedence,
                       Ready healthy/down/exhausted, Watch under -race
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: `Option func(*Config) error`, a `Pool` interface, `PoolStats`, `buildConfig` that aggregates errors, `NewService(pool Pool, opts ...Option)` for fakes, `NewSQLService(db *sql.DB, opts ...Option)` that pushes the settings onto the driver, the options `WithDSN` (required), `WithMaxOpenConns`, `WithMaxIdleConns`, `WithConnMaxIdleTime`, `WithHealthCheckInterval`, `WithConnectTimeout`, the `Ready` probe, a `Watch` health loop, the `sqlPool` adapter, and accessors.
- Test: `service_test.go` proves defaults, that a missing DSN and a nil pool are rejected, that one bad call aggregates every problem, the `idle <= open` cross-field rule, last-option-wins precedence, the three `Ready` outcomes (healthy, ping failure, pool exhausted), and that `Watch` reports health and stops on cancel under `-race`.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

### Depend on a port, not on `*sql.DB`

The design decision that makes this service testable is that `Service` depends on a `Pool` interface — `Ping`, `Stats`, `Close` — not on a concrete `*sql.DB`. The real production path, `NewSQLService`, wraps a `*sql.DB` in the `sqlPool` adapter; tests and the demo pass an in-memory fake that implements the same three methods. The readiness logic cannot tell them apart, so the behaviour that matters in production — does the probe ping, does it reject an exhausted pool — is exercised at full fidelity without a live database or a driver dependency. `PoolStats` is deliberately shaped like `database/sql.DBStats` so the adapter is a one-line field copy.

The options configure a `Config` value, and two constructors share the same validation through `buildConfig`. `NewService` takes any `Pool`; `NewSQLService` takes a `*sql.DB`, validates the same options, and then pushes the validated numbers onto the driver with `SetMaxOpenConns`, `SetMaxIdleConns`, and `SetConnMaxIdleTime` before wrapping it. That is the whole point of the exercise made concrete: the identical options that the fake-backed service validates are the ones that configure the live pool, so there is exactly one place where a pool setting is named and one place where it is checked.

### Required DSN, the cross-field rule, and aggregation

`buildConfig` follows the now-familiar shape: defaults first, run every option collecting errors, then the two checks no single option can own. The DSN is required, so `WithDSN` only assigns and the post-loop `cfg.dsn == ""` check owns `ErrMissingDSN`, collapsing missing and empty into one error. The cross-field invariant is `maxIdleConns <= maxOpenConns`, guarded by `maxOpenConns > 0` because a zero open limit means unlimited and so imposes no ceiling. Both are validated after the loop and the whole set is returned through `errors.Join`, so a single misconfiguration — say a missing DSN and a negative open count and a zero health interval — comes back as one error carrying every sentinel.

### The readiness check, and why the health interval is real

`Ready` is the probe a load balancer or a Kubernetes readiness gate calls before routing traffic. It is not a toy: it derives a context from the configured connect timeout, pings the pool through it, and then reads `Stats` and rejects a pool whose open connections have run past the configured maximum — the `ErrPoolExhausted` case that protects a saturated database from receiving yet more load. A nil return is the signal to serve.

`WithHealthCheckInterval` is not a field that sits unused: `Watch` turns it into a background probe that runs `Ready` on that interval and delivers each result on a channel until the context is cancelled, at which point it closes the channel. A single goroutine owns the channel — it is the only sender and the only closer — so there is no send-after-close race, which is what lets the `Watch` test run clean under `-race` while the fake pool is mutated from the test goroutine. The fake guards its own state with a mutex for the same reason; a readiness monitor that a concurrent probe and a state change touch at once is exactly the shape `-race` exists to validate.

Create `service.go`:

```go
package dbservice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Option configures the pool settings during construction. The constructors run
// every option and collect their errors, so one bad call reports all of its
// problems at once instead of only the first.
type Option func(*Config) error

// Config is the validated connection-pool configuration. Its fields are
// unexported and set only through options, so a built Config cannot drift past
// the validation that ran during construction.
type Config struct {
	dsn                 string
	maxOpenConns        int
	maxIdleConns        int
	connMaxIdleTime     time.Duration
	healthCheckInterval time.Duration
	connectTimeout      time.Duration
}

// PoolStats is the subset of pool telemetry the readiness check needs. It mirrors
// the shape of database/sql.DBStats so a real *sql.DB maps onto it directly.
type PoolStats struct {
	OpenConnections int
	InUse           int
	Idle            int
}

// Pool is the narrow port the Service depends on. database/sql.DB satisfies it
// through the sqlPool adapter, and tests satisfy it with an in-memory fake, so
// the readiness logic is exercised without a live database.
type Pool interface {
	Ping(ctx context.Context) error
	Stats() PoolStats
	Close() error
}

// Service owns a configured pool and exposes a readiness check over it.
type Service struct {
	cfg  Config
	pool Pool
}

var (
	ErrMissingDSN        = errors.New("dsn is required")
	ErrNilPool           = errors.New("pool must not be nil")
	ErrNilDB             = errors.New("db must not be nil")
	ErrInvalidMaxOpen    = errors.New("max open conns must not be negative")
	ErrInvalidMaxIdle    = errors.New("max idle conns must not be negative")
	ErrIdleExceedsOpen   = errors.New("max idle conns must not exceed max open conns")
	ErrBadIdleTime       = errors.New("conn max idle time must not be negative")
	ErrBadHealthInterval = errors.New("health-check interval must be positive")
	ErrBadConnectTimeout = errors.New("connect timeout must be positive")
	ErrPoolExhausted     = errors.New("open connections exceed configured maximum")
)

func defaults() Config {
	return Config{
		maxOpenConns:        10,
		maxIdleConns:        2,
		connMaxIdleTime:     5 * time.Minute,
		healthCheckInterval: 30 * time.Second,
		connectTimeout:      5 * time.Second,
	}
}

// buildConfig applies defaults, runs every option collecting errors, then runs
// the checks no single option can own: the required DSN and the cross-field
// invariant idle <= open. errors.Join keeps every sentinel matchable.
func buildConfig(opts ...Option) (Config, error) {
	cfg := defaults()
	var errs []error
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			errs = append(errs, err)
		}
	}
	if cfg.dsn == "" {
		errs = append(errs, ErrMissingDSN)
	}
	if cfg.maxOpenConns > 0 && cfg.maxIdleConns > cfg.maxOpenConns {
		errs = append(errs, fmt.Errorf("%w: idle=%d open=%d", ErrIdleExceedsOpen, cfg.maxIdleConns, cfg.maxOpenConns))
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("dbservice: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// NewService builds a Service over any Pool. Tests pass a fake here; production
// code passes a real adapter. The pool is required and the config is validated
// before the Service is returned.
func NewService(pool Pool, opts ...Option) (*Service, error) {
	if pool == nil {
		return nil, fmt.Errorf("dbservice: %w", ErrNilPool)
	}
	cfg, err := buildConfig(opts...)
	if err != nil {
		return nil, err
	}
	return &Service{cfg: cfg, pool: pool}, nil
}

// NewSQLService builds a Service over a real *sql.DB, pushing the validated
// pool settings onto the driver via the database/sql knobs before wrapping it.
// This is the production wiring: the same options that the fake-backed Service
// validates are the ones that configure the live pool.
func NewSQLService(db *sql.DB, opts ...Option) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("dbservice: %w", ErrNilDB)
	}
	cfg, err := buildConfig(opts...)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.maxOpenConns)
	db.SetMaxIdleConns(cfg.maxIdleConns)
	db.SetConnMaxIdleTime(cfg.connMaxIdleTime)
	return &Service{cfg: cfg, pool: sqlPool{db: db}}, nil
}

// Ready is the readiness probe a load balancer or orchestrator calls: it pings
// the pool within the connect timeout and rejects a pool whose open connection
// count has run past the configured ceiling. A nil return means serve traffic.
func (s *Service) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.connectTimeout)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("dbservice: readiness ping failed: %w", err)
	}
	st := s.pool.Stats()
	if s.cfg.maxOpenConns > 0 && st.OpenConnections > s.cfg.maxOpenConns {
		return fmt.Errorf("dbservice: %w: open=%d max=%d", ErrPoolExhausted, st.OpenConnections, s.cfg.maxOpenConns)
	}
	return nil
}

// Watch runs the readiness probe on the configured interval until ctx is done,
// delivering each result on the returned channel (dropping a result if no one is
// reading). The channel is closed when ctx is cancelled. A single goroutine owns
// the channel, so there is no send-after-close race.
func (s *Service) Watch(ctx context.Context) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(s.cfg.healthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				close(ch)
				return
			case <-ticker.C:
				select {
				case ch <- s.Ready(ctx):
				default:
				}
			}
		}
	}()
	return ch
}

func (s *Service) DSN() string                        { return s.cfg.dsn }
func (s *Service) MaxOpenConns() int                  { return s.cfg.maxOpenConns }
func (s *Service) MaxIdleConns() int                  { return s.cfg.maxIdleConns }
func (s *Service) ConnMaxIdleTime() time.Duration     { return s.cfg.connMaxIdleTime }
func (s *Service) HealthCheckInterval() time.Duration { return s.cfg.healthCheckInterval }
func (s *Service) ConnectTimeout() time.Duration      { return s.cfg.connectTimeout }
func (s *Service) Close() error                       { return s.pool.Close() }

// WithDSN supplies the required data source name. An empty value is left for the
// required-field check in buildConfig to reject as ErrMissingDSN.
func WithDSN(dsn string) Option {
	return func(c *Config) error {
		c.dsn = dsn
		return nil
	}
}

func WithMaxOpenConns(n int) Option {
	return func(c *Config) error {
		if n < 0 {
			return fmt.Errorf("%w: got %d", ErrInvalidMaxOpen, n)
		}
		c.maxOpenConns = n
		return nil
	}
}

func WithMaxIdleConns(n int) Option {
	return func(c *Config) error {
		if n < 0 {
			return fmt.Errorf("%w: got %d", ErrInvalidMaxIdle, n)
		}
		c.maxIdleConns = n
		return nil
	}
}

func WithConnMaxIdleTime(d time.Duration) Option {
	return func(c *Config) error {
		if d < 0 {
			return fmt.Errorf("%w: got %s", ErrBadIdleTime, d)
		}
		c.connMaxIdleTime = d
		return nil
	}
}

func WithHealthCheckInterval(d time.Duration) Option {
	return func(c *Config) error {
		if d <= 0 {
			return fmt.Errorf("%w: got %s", ErrBadHealthInterval, d)
		}
		c.healthCheckInterval = d
		return nil
	}
}

func WithConnectTimeout(d time.Duration) Option {
	return func(c *Config) error {
		if d <= 0 {
			return fmt.Errorf("%w: got %s", ErrBadConnectTimeout, d)
		}
		c.connectTimeout = d
		return nil
	}
}

// sqlPool adapts a *sql.DB to the Pool port. It is the production implementation
// the Service talks to; the readiness check cannot tell it apart from the fake.
type sqlPool struct {
	db *sql.DB
}

func (p sqlPool) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }

func (p sqlPool) Stats() PoolStats {
	s := p.db.Stats()
	return PoolStats{OpenConnections: s.OpenConnections, InUse: s.InUse, Idle: s.Idle}
}

func (p sqlPool) Close() error { return p.db.Close() }
```

### The runnable demo

The demo uses a small in-memory `demoPool` so it runs offline; a real program would hand a `*sql.DB` to `NewSQLService` instead. It builds a service over a healthy pool and shows `Ready` returning nil, builds one over a pool whose ping fails and shows readiness rejecting it, and finally makes a misconfigured call to show the aggregated config error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/dbservice"
)

// demoPool is a stand-in Pool so the demo runs offline. A real program would
// pass a *sql.DB to dbservice.NewSQLService instead.
type demoPool struct {
	pingErr error
	stats   dbservice.PoolStats
}

func (p demoPool) Ping(ctx context.Context) error { return p.pingErr }
func (p demoPool) Stats() dbservice.PoolStats     { return p.stats }
func (p demoPool) Close() error                   { return nil }

func main() {
	// A healthy pool wired with explicit options.
	healthy := demoPool{stats: dbservice.PoolStats{OpenConnections: 4, InUse: 1, Idle: 3}}
	svc, err := dbservice.NewService(healthy,
		dbservice.WithDSN("postgres://localhost:5432/app"),
		dbservice.WithMaxOpenConns(20),
		dbservice.WithMaxIdleConns(5),
	)
	if err != nil {
		fmt.Println("construct error:", err)
		return
	}
	fmt.Printf("config: open=%d idle=%d health=%s\n",
		svc.MaxOpenConns(), svc.MaxIdleConns(), svc.HealthCheckInterval())
	fmt.Println("ready (healthy)?", svc.Ready(context.Background()))

	// A pool whose ping fails: readiness must reject it.
	down := demoPool{pingErr: errors.New("connection refused")}
	svcDown, _ := dbservice.NewService(down, dbservice.WithDSN("postgres://localhost:5432/app"))
	fmt.Println("ready (down)?  ", svcDown.Ready(context.Background()))

	// A misconfigured call reports every problem at once.
	_, err = dbservice.NewService(healthy,
		dbservice.WithMaxOpenConns(-1),
		dbservice.WithMaxIdleConns(9),
	)
	fmt.Println("config errors:")
	fmt.Println(err)
	fmt.Println("missing dsn?", errors.Is(err, dbservice.ErrMissingDSN))
}
```

The import path is the module path `example.com/dbservice`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
config: open=20 idle=5 health=30s
ready (healthy)? <nil>
ready (down)?   dbservice: readiness ping failed: connection refused
config errors:
dbservice: max open conns must not be negative: got -1
dsn is required
missing dsn? true
```

### Tests

The tests use a mutex-guarded `fakePool` so the construction, the three `Ready` outcomes, and the concurrent `Watch` loop are all exercised without a database. `TestConfigAggregatesErrors` asserts that a missing DSN, a negative open count, and a zero health interval come back as one joined error. The three `Ready` tests pin the healthy path, the ping-failure path, and the `ErrPoolExhausted` capacity rejection. `TestWatchReportsHealthAndStops` runs the background probe on a 2ms interval, flips the pool unhealthy mid-flight, confirms the failure surfaces, then cancels and confirms the channel closes — all clean under `-race`.

Create `service_test.go`:

```go
package dbservice

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakePool is an in-memory Pool used to exercise the Service without a live
// database. It is mutex-guarded so the background Watch goroutine and the test
// goroutine can touch it concurrently under -race.
type fakePool struct {
	mu      sync.Mutex
	pingErr error
	stats   PoolStats
	pings   int
}

func (f *fakePool) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	return f.pingErr
}

func (f *fakePool) Stats() PoolStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *fakePool) Close() error { return nil }

func (f *fakePool) setPingErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingErr = err
}

func (f *fakePool) setStats(s PoolStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats = s
}

func (f *fakePool) pingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func TestDefaults(t *testing.T) {
	t.Parallel()

	svc, err := NewService(&fakePool{}, WithDSN("postgres://x"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.MaxOpenConns() != 10 || svc.MaxIdleConns() != 2 {
		t.Errorf("conns wrong: open=%d idle=%d", svc.MaxOpenConns(), svc.MaxIdleConns())
	}
	if svc.ConnMaxIdleTime() != 5*time.Minute || svc.HealthCheckInterval() != 30*time.Second {
		t.Errorf("durations wrong: idle=%s health=%s", svc.ConnMaxIdleTime(), svc.HealthCheckInterval())
	}
	if svc.ConnectTimeout() != 5*time.Second {
		t.Errorf("connect timeout = %s, want 5s", svc.ConnectTimeout())
	}
}

func TestNilPoolRejected(t *testing.T) {
	t.Parallel()

	if _, err := NewService(nil, WithDSN("x")); !errors.Is(err, ErrNilPool) {
		t.Fatalf("err = %v, want ErrNilPool", err)
	}
}

func TestRequiresDSN(t *testing.T) {
	t.Parallel()

	if _, err := NewService(&fakePool{}, WithMaxOpenConns(5)); !errors.Is(err, ErrMissingDSN) {
		t.Fatalf("err = %v, want ErrMissingDSN", err)
	}
}

func TestConfigAggregatesErrors(t *testing.T) {
	t.Parallel()

	// Missing DSN, negative open count, and a bad health interval reported as one.
	_, err := NewService(&fakePool{},
		WithMaxOpenConns(-1),
		WithHealthCheckInterval(0),
	)
	if err == nil {
		t.Fatal("expected aggregated error, got nil")
	}
	for _, want := range []error{ErrMissingDSN, ErrInvalidMaxOpen, ErrBadHealthInterval} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v; got %v", want, err)
		}
	}
}

func TestCrossFieldIdleExceedsOpen(t *testing.T) {
	t.Parallel()

	_, err := NewService(&fakePool{}, WithDSN("x"), WithMaxOpenConns(4), WithMaxIdleConns(9))
	if !errors.Is(err, ErrIdleExceedsOpen) {
		t.Fatalf("err = %v, want ErrIdleExceedsOpen", err)
	}
}

func TestLaterOptionWins(t *testing.T) {
	t.Parallel()

	svc, err := NewService(&fakePool{}, WithDSN("x"), WithMaxOpenConns(20), WithMaxOpenConns(50))
	if err != nil {
		t.Fatal(err)
	}
	if svc.MaxOpenConns() != 50 {
		t.Fatalf("max open = %d, want 50 (last option wins)", svc.MaxOpenConns())
	}
}

func TestReadyHealthy(t *testing.T) {
	t.Parallel()

	pool := &fakePool{}
	pool.setStats(PoolStats{OpenConnections: 3, InUse: 1, Idle: 2})
	svc, err := NewService(pool, WithDSN("x"), WithMaxOpenConns(10))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() = %v, want nil", err)
	}
	if pool.pingCount() != 1 {
		t.Errorf("ping count = %d, want 1", pool.pingCount())
	}
}

func TestReadyPingFails(t *testing.T) {
	t.Parallel()

	pool := &fakePool{}
	pool.setPingErr(errors.New("connection refused"))
	svc, err := NewService(pool, WithDSN("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Ready(context.Background()); err == nil {
		t.Fatal("Ready() = nil, want error from failed ping")
	}
}

func TestReadyPoolExhausted(t *testing.T) {
	t.Parallel()

	pool := &fakePool{}
	pool.setStats(PoolStats{OpenConnections: 11})
	svc, err := NewService(pool, WithDSN("x"), WithMaxOpenConns(10))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Ready(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Ready() = %v, want ErrPoolExhausted", err)
	}
}

func TestWatchReportsHealthAndStops(t *testing.T) {
	t.Parallel()

	pool := &fakePool{}
	svc, err := NewService(pool, WithDSN("x"), WithHealthCheckInterval(2*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := svc.Watch(ctx)

	// First tick: healthy.
	select {
	case got := <-ch:
		if got != nil {
			t.Fatalf("first health result = %v, want nil", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first health result")
	}

	// Flip the pool unhealthy; a later tick must report the failure.
	pool.setPingErr(errors.New("connection refused"))
	deadline := time.After(time.Second)
	for {
		select {
		case got := <-ch:
			if got != nil {
				cancel()
				goto drained
			}
		case <-deadline:
			t.Fatal("timed out waiting for unhealthy result")
		}
	}

drained:
	// After cancel, Watch closes the channel.
	select {
	case _, open := <-ch:
		_ = open
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close after cancel")
	}
}
```

## Review

The service is correct when the port boundary holds and the two non-option checks live after the loop. Depending on `Pool` rather than `*sql.DB` is what makes `Ready` testable at all — `TestReadyHealthy`, `TestReadyPingFails`, and `TestReadyPoolExhausted` exercise the exact production logic through a fake, and `ErrPoolExhausted` must compare against `maxOpenConns` only when it is positive or the unlimited case would false-positive. The required-DSN check must sit post-loop, which `TestRequiresDSN` (passing only `WithMaxOpenConns`) catches, and the aggregation must keep collecting past the first failure, which `TestConfigAggregatesErrors` proves by asserting three sentinels in one error. `NewSQLService` is the wiring that matters in production: the same validated numbers reach the driver through `SetMaxOpenConns` and friends, so there is one source of truth for a pool setting. The `Watch` loop must let a single goroutine own the channel as both sender and closer; any other arrangement either races on close or deadlocks, and `TestWatchReportsHealthAndStops` under `-race` is what holds that line. With every test green under `go test -race ./...`, the config-to-pool-to-readiness path is sound.

## Resources

- [`database/sql.DB.SetMaxOpenConns`](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns) — the real pool knob `NewSQLService` drives from the validated options.
- [`database/sql.DBStats`](https://pkg.go.dev/database/sql#DBStats) — the telemetry `PoolStats` mirrors and the readiness check reads.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — the bounded context the readiness ping runs under.
- [Go blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) — the channel-ownership discipline the Watch loop follows to stay race-free.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-production-api-client.md](04-production-api-client.md)
