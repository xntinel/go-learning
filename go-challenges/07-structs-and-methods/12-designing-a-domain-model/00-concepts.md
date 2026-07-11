# Designing a Domain Model: Value Objects, Entities, and Invariants — Concepts

In a real backend the domain model is the load-bearing wall. If an invariant is
not enforced at the type boundary, corrupt state does not stay contained: it
flows into the database, into the events you publish, into other services that
trust your payloads, and no amount of validation bolted onto the HTTP handler
will save you, because the handler is only one of many doors. Workers, batch
jobs, migrations, message consumers, and tests all construct the same types, and
every one of them must be forced through the same checks. This lesson trains the
senior habits that keep a Go codebase honest at scale: unexported fields plus
constructors so illegal states are literally unrepresentable; value objects that
are immutable and comparable so they are safe as map keys and cheap to pass by
value; money as integer minor units with a currency tag so you never do float
arithmetic on cash or silently add USD to EUR; DTO/domain separation so JSON tags
and wire concerns never bleed into the model; optimistic-concurrency version
fields for lost-update protection; explicit state machines with legal-transition
tables for order and payment lifecycles; and thread-safe repositories that hand
out copies so callers cannot mutate shared aggregate state. Read this file once
and you have the model behind all ten independent modules that follow; each one is
shaped like a fragment of a production service, not a syntax demo.

## Concepts

### Value object vs entity

The first design decision for any domain type is whether it has an identity. A
*value object* has none: it is defined entirely by its fields, and two value
objects with equal fields are interchangeable. A five-dollar bill is a five-dollar
bill; you do not care *which* one you hold. An `EmailAddress`, a `Money`, a
`LedgerEntry` — these are values. An *entity*, by contrast, is defined by a stable
identity, usually an `ID`, and it may change state over time while remaining the
same entity. A `User` who renames themselves or deposits funds is still the same
user, tracked by the same id. Two `User` values with the same id are the same
user even if every other field differs; two with different ids are different users
even if every other field is identical.

This distinction is not academic; it drives three concrete decisions. Equality: a
value object compares by all its fields, an entity by its id. Mutability: a value
object is immutable (operations return new values), an entity has a lifecycle.
Lifecycle: an entity gets created, transitions through states, and is eventually
retired, while a value object simply *is*. Get the classification wrong and every
downstream choice — receivers, comparison, storage — inherits the confusion.

### Make illegal states unrepresentable

The strongest guarantee a Go type can offer is that an invalid instance cannot be
built at all. You achieve it with two tools working together: unexported fields,
so no external package can assign them, and a validating constructor, so the only
way to obtain a value is to pass through the check. The constructor and the
exported methods are the *only* doors into the type, and every door verifies the
invariant. A `Balance` whose `amount` field is unexported and whose `NewBalance`
rejects negatives can never hold a negative amount, no matter how careless the
caller. This is the whole game: push validity into the type system so that
"holding a `Balance`" is itself proof the balance is non-negative.

### Invariants live at the boundary, not at the caller

Validating in the HTTP handler is defense in depth and worth doing, but it is not
where the invariant lives. The domain type itself must reject bad input so the
rule holds no matter who calls it. Consider the paths that construct a `User` in a
mature service: the signup handler, a bulk-import worker, a data-migration script,
a message consumer replaying an event, a test fixture. If the "email is required"
rule lives only in the signup handler, every other path can create a user with no
email, and the corruption is discovered months later in a report that crashes on a
nil field. Put the rule in `NewUser` and all five paths inherit it for free. The
boundary of a type is its constructor and its exported methods; that is the one
place the invariant is guaranteed to run.

### Immutability and comparability of value objects

A value object's operations return new values rather than mutating the receiver.
`balance.Add(other)` yields a new `Balance`; it does not change `balance`. This
buys three things: the value is safe to share across goroutines without a lock
(no one can mutate it), it is safe to use as a map key (its identity as a key
never shifts underneath the map), and it is free of aliasing bugs (no two
references quietly point at the same mutable state). A value receiver expresses
this intent directly — the method operates on a copy and returns a copy.

Comparability is the companion property. A value object *should* be comparable so
`==` works and it can serve as a map key. In Go, a struct is comparable exactly
when all its fields are comparable, which forbids slice, map, and function fields.
So the moment you give a value object a `[]string` field, you lose `==` (it will
not compile as a map key, and comparing two of them panics at runtime). When
structural `==` is not the equality you want — because equality should ignore some
field, or normalize before comparing — provide an explicit `Equal` method and
document why `==` is not enough. But reach for that only when you must; plain
comparability is the cheaper, safer default.

### Money is integer minor units plus a currency, never a float

`float64` cannot represent `0.10` exactly, so a chain of float additions on cash
drifts by fractions of a cent and eventually a reconciliation report is off by a
penny that no one can explain. The fix every payments system converges on is to
store money as an integer count of the *minor unit* — cents for USD, pence for
GBP — in an `int64`, and to do all arithmetic on those integers. Exactness is
restored: `10 + 20` cents is exactly `30` cents, forever.

The second half of correct money is a currency tag. An amount without a currency
is a number, not money, and adding `100` USD-cents to `100` EUR-cents to get
`200` of nothing is a silent data-corruption bug. Attach a `Currency` to the
amount and make `Add`/`Sub` reject a currency mismatch, and that bug becomes a
loud, rejected operation at the exact call site that caused it. Money is the
canonical value object: immutable, comparable, and defined entirely by its amount
and currency.

### Value vs pointer receivers and the copy-return idiom

Value-receiver methods that return a modified copy give an entity an
immutable *feel*: `u2, err := u.Withdraw(amount)` leaves `u` untouched and yields
a new `User`. This style makes accidental shared mutation impossible and reads
cleanly. Pointer receivers are the right choice when you genuinely need in-place
mutation, when the struct is large enough that copying it per call is wasteful, or
when the type must not be copied at all — most importantly any type that embeds a
`sync.Mutex` or `sync.RWMutex`, because copying a mutex after first use breaks it
and `go vet` flags it. The rule of thumb: value receivers for small immutable
values, pointer receivers for stateful, lock-holding, or large types. Be
consistent within a type — mixing receiver kinds on one type is a smell.

### DTO/domain separation

JSON tags, wire field names, and serialization concerns belong on a *DTO* (data
transfer object), not on the domain type. The wire shape is a contract with the
outside world; the domain shape is a contract with your invariants. Coupling them
means a rename in the API forces a change to the model, and worse, that decoding a
payload straight into the domain struct bypasses the constructor and lets illegal
states in through the network. The discipline is: keep the domain type's fields
unexported and tag-free, define a separate DTO with the json tags, map explicitly
between them, and on decode re-run the constructor so a deserialized object is
always valid. Implementing `json.Marshaler`/`json.Unmarshaler` on the domain type
lets it participate in `encoding/json` while keeping the DTO an internal detail,
and the `UnmarshalJSON` path is where you re-validate.

### Aggregate roots own invariants across a cluster of objects

Some invariants span more than one object. "The running balance implied by this
ledger never goes negative" is a rule about a *collection* of entries, not any
single entry. The pattern is an *aggregate root*: one entity that is the sole
entry point to a cluster of child entities and value objects, holds them
privately, and enforces the rules that span them. External code talks only to the
root; it never reaches a child directly. `Account.Post(entry)` is the only way to
add a `LedgerEntry`, and it checks the cross-entry invariant before appending.
Accessors return *copies* of the internal collection so that handing a caller the
list of entries does not hand them a way to mutate the aggregate's private state.
The boundary is the whole point; a leaked internal slice silently breaches it.

### State machines as data

A lifecycle — `Pending`, `Paid`, `Shipped`, `Delivered`, `Cancelled` — is a set
of legal transitions. Scattering those rules across `if` statements in handlers
guarantees that some path eventually allows an illegal move like `Shipped ->
Pending`. Encode the transitions as *data* instead: a table mapping each state to
the set of states it may move to. `Transition(to)` consults the table, rejects any
move not in it with a sentinel error, and records the change. Terminal states map
to an empty set, so they are final by construction. This turns lifecycle rules
into a single testable table rather than logic smeared across the codebase, and it
makes "what transitions are legal?" a question you answer by reading one map.

### Optimistic concurrency with a version field

When two workers load the same entity, both modify it, and both save, the second
save silently overwrites the first — a *lost update*. Pessimistic locking (hold a
lock for the whole read-modify-write) prevents it but does not scale across
processes. Optimistic concurrency does: give the entity a monotonically increasing
`Version`, and make the update *conditional* on the version the caller last saw.
If the stored version still equals the caller's expected version, the write
succeeds and bumps the version; if not, someone else wrote in between and the
update is rejected with a conflict error so the caller can reload and retry. This
mirrors exactly what a SQL `UPDATE ... WHERE id = ? AND version = ?` does, and
what an event store's expected-version check does. No long-held locks, and lost
updates become detected conflicts.

### Encapsulation under concurrency

A repository shared across goroutines must guard its store and must not leak
mutable references to it. Guarding means a mutex, and specifically an `RWMutex`
when reads dominate: many readers hold `RLock` concurrently, a writer takes the
exclusive `Lock`. Not leaking means accessors return *copies* of stored values and
never return the internal map or a pointer into it — because a returned pointer is
a channel through which a caller mutates shared state without holding the lock,
reintroducing the exact data race the encapsulation was meant to prevent. The two
rules work together: guard the store, and hand out copies. `go test -race` is the
arbiter; a repository that passes under `-race` with concurrent readers and
writers is honest, one that does not is broken no matter how clean it looks.

## Common Mistakes

### Exporting struct fields so callers bypass the constructor

Wrong: a `User` with exported `Name`, `Email`, `Balance` fields. Any caller
assigns them directly, and every invariant the constructor enforced is gone —
someone sets `Balance` to a negative value and the type is powerless to stop it.

Fix: keep fields unexported and expose read-only accessors plus methods that
preserve invariants. The only mutation path is a method that checks the rule.

### Putting a type's operations in free functions

Wrong: `func Add(a, b Balance) Balance` as a package-level function. The data is
owned by the type, so the operation should be too; a free function invites a
second, subtly different implementation elsewhere.

Fix: methods on the type — `func (b Balance) Add(other Balance) Balance`. The type
that owns the data owns the operations on it.

### Mixing value and entity semantics

Wrong: a mutable value object, or an immutable entity with no identity. A `Balance`
whose methods mutate in place, or a `User` compared by all fields instead of id,
behaves inconsistently with its peers and confuses every caller.

Fix: value objects are immutable and compare by fields; entities have identity,
compare by id, and evolve through a lifecycle. Pick one per type and hold to it.

### Representing money as a float

Wrong: `type Money float64` and `a + b`, which loses cents over time, or adding
two `Money` values of different currencies with no guard.

Fix: `int64` minor units and a `Currency` tag; `Add`/`Sub` reject a currency
mismatch. No float ever touches cash.

### Giving a value object a slice, map, or func field

Wrong: an `EmailAddress` or `Money` with a `[]string` field, then using it as a
map key or comparing with `==`. It will not compile as a map key and comparing
panics at runtime.

Fix: keep value-object fields comparable (no slice/map/func). If you truly need a
collection inside, it is probably an entity or aggregate, not a value object.

### Letting the wire format dictate the model

Wrong: json tags on the domain struct, and `json.Unmarshal` straight into it,
which skips the constructor and lets a payload with an empty required field create
an illegal object over the network.

Fix: a separate DTO carries the json tags; `UnmarshalJSON` maps into the DTO and
then re-runs the constructor, so a decoded object is always valid.

### Returning the internal slice or map from an accessor

Wrong: `func (a *Account) Entries() []LedgerEntry { return a.entries }`, which
hands the caller a reference into the aggregate's private state to mutate at will.

Fix: return a copy — `slices.Clone(a.entries)` — so the boundary holds.

### Updating an entity with no version check

Wrong: `store[id] = updated` with no comparison, so two concurrent writers
silently clobber each other and one update is lost with no error.

Fix: a `Version` field and a conditional update that rejects a stale version with
a conflict error, mirroring `WHERE version = ?`.

### Guarding a repository wrongly, or not at all

Wrong: a plain `Mutex` where reads vastly outnumber writes (needless contention),
or no lock at all, shipping a data race that only surfaces under load.

Fix: an `RWMutex` — `RLock` for `Get`/`List`, `Lock` for `Save` — and prove it
with `go test -race`.

### Copying a struct that embeds a mutex, or a pointer receiver on a value object

Wrong: a pointer receiver on an immutable value object whose whole point is
copy-return semantics, or copying a struct that embeds a `sync.Mutex` (which must
not be copied after first use; `go vet` flags it).

Fix: value receivers for immutable values; for lock-holding types use pointer
receivers everywhere and never copy the value.

Next: [01-balance-value-object.md](01-balance-value-object.md)
