# Blank Identifier and Shadowing: Silent Failure Modes in Backend Code — Concepts

The two most productive sources of silent, happy-path-passing bugs in a Go
backend are not exotic. They are the underscore and the colon-equals. A shadowed
`err` makes a failed database write look like a success. A discarded comma-ok
turns a cache miss into a spurious zero value. A dropped `rows.Err()` truncates a
result set with no error to show for it. A shadowed named return inside a
deferred transaction guard commits data that should have rolled back. Every one
of these compiles, passes a naive test that only exercises the happy path, and
ships. The discipline this lesson trains is small and mechanical: treat every
`_` as an explicit, defensible decision, and structure your error variables so
that reassignment (`=`) versus new declaration (`:=`) is always deliberate. The
exercises that follow build the real artifacts where these bugs live — a
repository scan loop, a transaction boundary, an idempotency cache, a config
loader, a retry classifier, a worker pool — and each one contains a test that
fails if you make the mistake.

## Concepts

### The blank identifier is not a variable

`_` is a write-only sink, not a storage location. You can assign to it, and the
right-hand side is still fully evaluated (for its side effects, and for type and
arity checking), but the value is thrown away. What you cannot do is read it
back. `_` has no address, so `&_` is illegal; it cannot be passed as an argument;
it cannot appear on the right-hand side of an assignment. This is exactly why you
cannot skip a column with `rows.Scan(&id, _, &name)` — `Scan` needs a pointer to
a real, writable destination for each column, and `_` is not one. Skipping a
column requires a genuine throwaway variable (`var discard sql.RawBytes`) whose
sole purpose is to receive and be ignored.

The corollary is that `_` on the left of `:=` or `=` is a statement of intent:
"this value is produced, and I am deliberately dropping it." That intent must be
true and it must be obvious to the next reader.

### The legitimate discards, and the one you must justify

There is a short list of blank uses that are idiomatic and correct: the range
index when you only want the value (`for _, v := range xs`), an unwanted extra
return value, a blank side-effect import, and a compile-time interface guard.
Discarding an *error* is the one case that is almost never on that list.
`event, _ := Decode(raw)` is a landmine: the decode can fail, the error is gone,
and the corruption surfaces three layers downstream where it is impossible to
trace. Discard an error only when failure is genuinely impossible or explicitly
irrelevant, and make that reasoning obvious at the call site — ideally with a
comment, never by reflex.

### The compile-time interface guard

`var _ T = (*Impl)(nil)` asks the compiler to prove that `*Impl` satisfies `T`,
at build time, with zero runtime cost and zero allocation. `(*Impl)(nil)` is a
typed nil pointer: it allocates nothing, unlike `&Impl{}` which would construct a
value. The moment a method signature drifts out of conformance — a renamed
parameter type, a dropped return — the build breaks at the guard, pointing
straight at the type and the interface it was meant to implement. It is
documentation the compiler enforces. For an *optional* capability (a handler that
may also implement `Flusher`), the compile-time guard does not fit; you check for
it at runtime with a comma-ok type assertion instead.

### `:=` declares; nested scopes shadow

`:=` declares a new variable if at least one name on its left is new in the
*current* scope. The subtlety is "current scope": the body of every `if`, `for`,
`switch`, and `{ }` block is a fresh scope. Inside a nested block, a `:=` on a
name that already exists in an enclosing scope does not reassign the outer one —
it declares a brand-new variable that shadows it, even when the name and type are
identical. The outer variable is untouched and still holds its old value.

The most dangerous instance is an error:

```go
func load() (Config, error) {
	cfg, err := parse()          // outer err
	if err != nil {
		return Config{}, err
	}
	if v, err := lookup(); err != nil {  // inner err shadows outer
		cfg.V = v
	}
	return cfg, err              // returns the OUTER err (nil), not the inner one
}
```

The `err` inside the `if` init is a fresh variable. The `return cfg, err` at the
bottom reads the outer `err`, which is still `nil`. A real failure from `lookup`
is reported as success. Either return immediately inside the inner block, or
declare the results once and reassign with `=`.

### The named-return transaction trap

Named returns plus a deferred commit-or-rollback guard are a shadow trap with
teeth. The pattern is:

```go
func (r *Repo) Transfer(...) (err error) {
	tx, _ := r.db.Begin()
	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()
	if _, err := debit(tx); err != nil {  // BUG: := shadows the named return
		return err
	}
	return credit(tx)
}
```

The deferred func inspects the *named return* `err`. But `if _, err := debit(tx)`
declares a new inner `err`; the `return err` copies its value into the named
return, so on that path it works — but if the intent was for the guard to see a
failure set anywhere in the body, the shadow silently defeats it, and any path
that sets a local `err` without a matching `return` lets the defer see `nil` and
commit corrupted state. Assign the named return with `=`, not `:=`.

### comma-ok: three states collapsed into one

The comma-ok idiom appears in three places, and discarding the second value
destroys information in each:

- Map index: `v, ok := m[k]`. Dropping `ok` conflates "absent" with
  "present but stored as the zero value." For an idempotency guard or a
  cache-presence check, those are opposite decisions.
- Type assertion: `v, ok := x.(T)`. Dropping `ok` conflates "matched" with
  "did not match" — and the single-value form `v := x.(T)` *panics* when `x`
  does not hold `T`. The comma-ok form never panics.
- Channel receive: `v, ok := <-ch`. Dropping `ok` conflates "received a real
  value" with "channel closed, this is the zero value." A worker drain loop that
  ignores `ok` never terminates cleanly.

For assertions where the target may be *wrapped* in an error chain, prefer
`errors.As` over a bare assertion — a directly-asserted `err.(T)` fails on a
wrapped error even when a `T` is buried inside it.

### rows.Err() after the loop is mandatory

`for rows.Next() { ... }` exits when `Next` returns `false`, and `false` means one
of two very different things: the result set ended normally, or a driver error
ended iteration early. Only `rows.Err()`, checked *after* the loop, tells them
apart. Skip it and a mid-stream connection error silently truncates your result
set into a shorter, plausible-looking, wrong answer. Pair every `rows.Next()`
loop with a post-loop `rows.Err()` check and a `defer rows.Close()`.

### Blank imports belong at the composition root

`import _ "some/driver"` runs that package's `init()` for its side effects —
registering a SQL driver, an image format, a codec — and imports nothing usable.
Because the effect is invisible init-ordering, it belongs at the composition root
(`main`, or a dedicated wiring package), never buried in a leaf business package.
A blank import deep in business logic couples that package's behavior to hidden
init ordering and makes the real dependency impossible to see. When a needed
registration is missing, a well-designed registry returns a typed "unknown X"
error at lookup time rather than failing to compile — so the failure is a clear
runtime message at the composition root, not a mystery.

### Go 1.22 fixed loop capture, not shadowing

Go 1.22+ gives each loop iteration its own copy of the loop variable, which
eliminated the classic goroutine-capture bug (`go func(){ use(v) }()` inside a
range loop no longer all sees the last value). That change is unrelated to
lexical shadowing. Declaring a fresh `err` in a nested block is still a live
hazard in modern Go; do not assume the loop-var fix covers it.

### The shadow analyzer is off by default

`go vet` does not detect shadowing in its default run. The `shadow` analyzer
(`golang.org/x/tools/go/analysis/passes/shadow`) exists but is off by default
because it produces false positives on legitimate intentional shadows; you opt in
explicitly with `go vet -vettool=$(which shadow)` in CI. Static analysis only
catches a subset. The reliable defense is behavioral: table tests that exercise
the *failure* paths (where a shadow returns the wrong result) and `-race` for the
concurrent conflations. This lesson leans on that: every module has a test that
fails specifically when the shadow or the discard bug is present.

## Common Mistakes

### Swallowing an error with `_`

Wrong: `event, _ := Decode(raw)`. The failure is invisible until corrupt data
surfaces downstream with no stack to blame. Fix: handle it, or justify the
discard explicitly at the call site.

### Shadowing an error in a nested init and returning the outer one

Wrong: an inner `if x, err := f(); err != nil` binds a fresh `err`, and a later
`return err` at the outer scope returns the outer (usually `nil`) value, so a real
failure reports success. Fix: return from the inner scope, or reassign the outer
with `=`.

### A shadowed inner err inside a deferred transaction guard

Wrong: `if _, err := step(tx); err != nil` inside a `Transfer` whose deferred
guard inspects the named return — the guard sees `nil` and commits. Fix: assign
the named return with `=`.

### Treating a discarded map comma-ok as a hit

Wrong: `v, _ := cache[k]` and then acting as if a value was found. A stored zero
value and a missing key are indistinguishable. Fix: `v, ok := cache[k]` and
branch on `ok`.

### Forgetting rows.Err() after the loop

Wrong: reading rows in a `for rows.Next()` loop and returning the slice without
checking `rows.Err()`. A driver error that ends iteration early truncates the
result silently. Fix: check `rows.Err()` after the loop; `defer rows.Close()`.

### Trying to skip a column with `_`, or sharing one throwaway across scans

Wrong: `rows.Scan(&id, _, &name)` does not compile; and reusing a single
throwaway destination across concurrent scans is a data race. Fix: a real
per-scan throwaway variable (`var discard sql.RawBytes`).

### A single-value type assertion that panics

Wrong: `t := err.(Retryable)` panics on the first error that does not implement
the interface. Fix: comma-ok `t, ok := err.(Retryable)`, and `errors.As` for
wrapped chains.

### A blank side-effect import buried in business logic

Wrong: `import _ "app/codec/gzip"` inside a leaf package, coupling it to hidden
init ordering. Fix: keep side-effect imports at `main` or a wiring package.

### Assuming Go 1.22 loop-var scoping fixed shadowing in general

Wrong: believing nested-scope shadowing of arbitrary variables is a solved
problem. It is not; only loop-variable capture was fixed.

### `var _ = expensiveSideEffect()` to force execution

Wrong: using a blank package-level var to trigger a side effect. Fix: an explicit
`init()` or an explicit wiring call, so the effect is visible.

Next: [01-event-processor-no-shadow.md](01-event-processor-no-shadow.md)
