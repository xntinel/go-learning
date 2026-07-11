# Constructor Functions and Validation — Concepts

A constructor is the one place in a service where untyped, untrusted input — an
environment variable, a JSON request body, a config file line, a CLI flag —
becomes a value the rest of the program is allowed to trust. Everything upstream
of the constructor is a `string` that might be garbage; everything downstream is
a `Config`, an `Email`, a `Money` whose invariants hold by construction. The
senior design question is blunt: can I build an invalid instance of this type?
If the answer is yes, that is a bug, because now every function that receives the
type has to defensively re-check it, and sooner or later one of them forgets. A
good constructor discharges the obligation to validate exactly once, at the
boundary, so no downstream code ever repeats it. This file is the conceptual
foundation for the independent exercises that follow; read it once and each
exercise becomes an application of the same discipline to a different real
backend artifact.

## Concepts

### A constructor is a trust boundary

Think of the type system as a claim about what is true. When a function signature
says `func Charge(m Money)`, it is asserting that any `Money` that reaches it is a
real, well-formed monetary amount in a known currency. That assertion is only
worth anything if there is exactly one door into the `Money` type and that door
validates. The constructor is that door. Its job is to convert the wide,
permissive input space (any `int64` and any `string`) into the narrow space of
valid values, rejecting everything else with an error. Once it has done so, the
invariant "this `Money` has a known currency" is true everywhere the type is
used, for free, with no re-checking. Break the single-door property — export the
fields, add a second unvalidated path — and the assertion collapses into a
comment nobody can rely on.

### Parse, don't validate

There are two ways to handle a raw string like an email address. The first is to
validate: write `func IsValidEmail(s string) bool`, pass the `string` around, and
call the validator at each use. The problem is that the validator returns a
`bool` and throws away everything it learned; the obligation to have checked
travels with the value as a social convention, not as a type, so every consumer
must remember to re-check or trust that someone else did. The second way is to
parse: write `func NewEmail(s string) (Email, error)` that returns a distinct
`Email` type whose underlying string is unexported. Now the only way to hold an
`Email` is to have gone through the parser, so a function that accepts an `Email`
cannot receive an unvalidated string — the compiler enforces it. Parsing pushes
the check to the boundary and encodes its result in the type; validating leaves
the check scattered and repeatable. "Make illegal states unrepresentable" is the
same idea stated as a goal: choose types such that the invalid combinations
cannot be written down.

### Aggregate every error with errors.Join

An operator fixing a broken config wants to see every problem in one pass. A
constructor that returns on the first bad field forces a miserable loop: fix the
host, redeploy, discover the port is wrong, redeploy, discover the mode is wrong,
redeploy. `errors.Join` (Go 1.20+) collects a slice of errors into a single error
whose `Error()` concatenates the messages with newlines and whose `Unwrap()
[]error` lets `errors.Is` and `errors.As` find every wrapped sentinel inside it.
The pattern is to accumulate failures into a `[]error`, and at the end return
`errors.Join(errs...)` — which is `nil` when the slice is empty, so the happy
path needs no special case. The operator sees all three typos at once; the
machine can still branch on any individual one.

### The sentinel-versus-wrapped contract

Errors serve two audiences with opposite needs. A human reading a log wants a
specific message: `port must be between 1 and 65535: 70000 out of range`. A
program deciding what to do wants a stable identity it can compare against,
independent of the wording. The idiom that serves both is a package-level
sentinel plus wrapping: declare `var ErrInvalidPort = errors.New("port must be
between 1 and 65535")`, and at the failure site return `fmt.Errorf("%w: %d out of
range", ErrInvalidPort, n)`. The `%w` verb wraps the sentinel so
`errors.Is(err, ErrInvalidPort)` is true, while the surrounding text carries the
human detail. Callers branch on the sentinel, never on the message. The moment
code does `strings.Contains(err.Error(), "port")` to make a decision, it has
coupled itself to prose that will be reworded, and `errors.Join` will break it
anyway by embedding the message in a larger string.

### Optional configuration: options, builders, positional args

Once a type has more than a couple of optional settings, the shape of its
constructor becomes a design decision. Three patterns dominate. A long positional
signature — `NewClient(url, timeout, retries, transport, ...)` — is rigid: every
call site must pass every argument in order, adding a parameter breaks every
caller, and `NewClient(u, 0, 0, nil)` at the call site is unreadable. Functional
options — `NewClient(url, WithTimeout(5*time.Second), WithRetries(3))` — make each
setting a self-describing function that mutates a private options struct;
defaults are natural (an unset option simply does not run), the set is
order-independent, adding an option is backward-compatible, and the constructor
validates the fully-assembled result once at the end. Builders — `New().Table("t")
.Limit(10).Build()` — stage mutable state across chained calls and defer
validation to `Build()`, which is a good fit when construction is genuinely
multi-step or when you want to accumulate errors across setters. Options favor
immutability and one-shot construction; builders favor staged, fluent assembly.
Pick by evolution pressure and by how the call site reads.

### Must-style constructors: correct panic versus latent crash

`regexp.MustCompile` and `template.Must` panic instead of returning an error.
That is not laziness; it is a precise statement about failure modes. They are
meant for package-level values initialized at program start —
`var emailRe = regexp.MustCompile(...)` — where the pattern is a compile-time
constant written by the programmer. If that constant is malformed, the binary is
fundamentally broken and should refuse to start, loudly, at init, rather than
limp along and fail later. The panic is the correct failure mode because the
input is trusted and the failure is unshippable. The identical `Must` call
becomes a bug the instant its argument comes from a request body, a config file,
or any runtime source: now a user's typo can panic the process, converting a
recoverable validation error into a crash. The rule is mechanical: `Must*` on a
literal you control, `New*` returning an error on anything from the outside.

### The zero value is a design decision, not an accident

Every Go type has a zero value that a caller can obtain without touching your
constructor — `var c Config` or a `Config` field left unset in a struct literal.
You must decide, deliberately, what that zero value means. There are two good
answers and one bad one. Make it useful: `sync.Mutex`, `bytes.Buffer`, and
`strings.Builder` are all designed so their zero value is immediately usable, no
constructor required, which is why you can embed a `sync.Mutex` in a struct and
just call `Lock`. Or make un-constructed use fail loudly: keep an unexported
`initialized bool` (or a nil internal map) and have every method return a typed
`ErrNotInitialized` when called on a zero value, so a skipped constructor
surfaces as a clean error at the boundary rather than a nil-panic deep in a
handler. The dangerous answer is the accidental middle ground — a zero value that
half-works, where some methods succeed and one panics on a nil map — because it
turns a construction bug into a crash three call frames away from its cause.

### Normalization belongs in the constructor

Some types are used as keys: a cache keyed by endpoint, a connection pool keyed by
address. Those uses depend on equal inputs producing equal keys. `HTTP://Example.COM:443/`
and `https://example.com` are the same endpoint, but as raw strings they are
distinct map keys that silently double your connections and halve your cache hit
rate. The fix is to canonicalize at construction: lowercase the host, drop the
default port, trim the trailing slash, default the scheme — once, in the
constructor — and store the canonical form. Downstream code compares and hashes
the canonical value and never re-normalizes. Normalization is idempotent by
design: normalizing an already-canonical value returns it unchanged, which is the
property a test should pin.

### Separate I/O from validation from construction

A constructor that reads a file, makes a network call, and validates is doing
three jobs, and it is untestable without all three: you cannot unit-test the
validation without a filesystem or a network. Split them. Read and decode in one
function (the impure, I/O-bound part), validate the decoded value in a pure
function, and construct from the validated parts. Where the constructor genuinely
needs to read the environment, inject the boundary — pass a
`lookup func(string) (string, bool)` rather than calling `os.Getenv` directly — so
a test can supply a map and exercise every validation branch deterministically,
with no global state and no environment mutation.

### Immutability through value receivers and unexported fields

A value object with unexported fields and only value-receiver methods cannot be
mutated after construction: there is no exported field to assign and no
pointer-receiver method to reassign through. That immutability is what makes it
safe to share a constructed value across goroutines without a lock and to use it
as a map key, because its observable state can never change out from under a
reader. The constructor validates once; immutability guarantees the validated
state stays valid for the value's entire lifetime.

## Common Mistakes

### Returning on the first validation error

Wrong: `if host == "" { return ErrMissingHost }` and stop, so the operator
discovers one problem per redeploy. Fix: accumulate every failure into a `[]error`
and return `errors.Join(errs...)` so all problems surface in a single pass.

### Constructing first and validating afterward

Wrong: build the struct, then validate, leaving a partially-valid value reachable
if the caller ignores the error. Fix: validate before returning; on failure return
the zero value and the error, never a half-built object the caller can misuse.

### A generic error the caller cannot branch on

Wrong: `return errors.New("invalid config")`, which forces callers to string-match
if they need to react. Fix: package-level sentinels wrapped with
`fmt.Errorf("%w: ...", ErrX)` and matched with `errors.Is`.

### Matching errors by string instead of errors.Is/As

Wrong: `err.Error() == "..."` or `strings.Contains(err.Error(), "port")`, which
breaks the moment the message is reworded or the error is wrapped by
`errors.Join`. Fix: `errors.Is` for identity, `errors.As` to extract a typed
error like a `FieldError`.

### Exporting a value object's fields "for convenience"

Wrong: exporting `Amount` and `Currency` on a `Money` so any package can assign
them past the invariant, making the constructor's validation meaningless. Fix:
unexported fields plus accessor methods.

### Calling Must* on runtime input

Wrong: `regexp.MustCompile(userPattern)`, turning a recoverable validation error
into a process-killing panic. Fix: `Must*` only for compile-time-constant
patterns; the error-returning `New*` for anything from a user or config.

### Hand-rolling parsers the stdlib already ships

Wrong: a regex for email or host:port validation that gets the RFC or the format
subtly wrong. Fix: `net/mail.ParseAddress`, `net/netip.ParseAddrPort`,
`net/url.Parse` — parsers that implement the actual rules.

### Letting a default mask an explicit zero

Wrong: reading a setting with the single-return form so `""`, `0`, or `false`
looks identical to unset, and a default silently overwrites a value the operator
explicitly set to zero. Fix: use the two-return `LookupEnv`/map `ok` form to
distinguish "unset" from "set to zero".

### Skipping normalization so equal inputs become distinct keys

Wrong: using raw endpoint strings as cache or pool keys, so two spellings of the
same endpoint double connections and miss cache. Fix: canonicalize in the
constructor and prove idempotence with a test.

### Trusting a zero value that actually requires construction

Wrong: relying on the zero value of a type with a nil internal map or an unset
client, causing a nil-panic deep in a handler. Fix: either make the zero value
genuinely useful or guard it with an `initialized` flag and a clean
`ErrNotInitialized`.

Next: [01-config-load-errors-join.md](01-config-load-errors-join.md)
