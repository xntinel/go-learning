# Service Layer Pattern — Concepts

Repositories hand back rows; HTTP handlers parse requests. Neither is the right home for the rules that decide what an application actually *does*. Placing an order means checking inventory, charging a card, persisting the order, and sending a confirmation, a sequence that spans several repositories and an external payment API. Bury that in a handler and it cannot be tested without a web server; bury it in a repository and data access is now tangled with policy. The service layer is the seam built precisely for this work: a thin layer of use-case methods that orchestrate the steps, own the failure handling, and depend on narrow interfaces rather than concrete storage. This file is the conceptual foundation; read it once and you will have everything you need to reason through each exercise, which builds one self-contained Go module apiece.

## Concepts

### The Service Owns The Pipeline, Not The Data

A service holds no rows of its own. Each step of a use case delegates to a repository or an external processor; the service is the choreographer that sequences those calls, decides which failure aborts the rest, and decides what to do about the steps that already succeeded. The single most useful test of a service method is to ask: if you deleted every repository implementation and kept only the interfaces, would the method still read as a clear statement of the business rule? If yes, the policy lives in the right place. This is also why a service method reads top to bottom like a recipe — validate, resolve, reserve, charge, persist, notify — with each line naming a verb the domain cares about, never a SQL statement or an HTTP call.

### Declare Narrow Interfaces At The Point Of Use

The service does not import a database package. It declares, next to itself, the exact interfaces it needs: an `OrderRepository` with `Save` and `NextID`, an `InventoryRepository` with `GetStock`, `Reserve`, and `Release`, a `PaymentProcessor` with one `Charge` method. This is the consumer-defined interface idiom that Go pushes you toward, and it has three payoffs. Tests substitute a hand-written fake in two lines. The dependency arrow points from the concrete storage adapter toward the service's interface, never the reverse, which is the whole of the dependency-inversion principle in one sentence. And each interface stays small — a method per verb the service actually calls — so an adapter implements only what is used, not a sprawling repository contract.

### Dependencies Are Injected And Validated Once

A service is wired through its constructor: every repository and processor it needs is passed in, the constructor rejects a nil one, and the fields stay unexported. This is the difference between a service that fails loudly at startup with "all dependencies are required" and one that panics with a nil-pointer dereference halfway through a customer's checkout. Construction is the one place where a misconfiguration is cheap to catch; spend the four lines there. Exposing the wiring back through small accessor methods (an `Orders()` that returns the injected repository) lets tests and demos assert on what was actually injected without making the fields public.

### Compensation Beats Database Transactions When A Step Is External

When every step touches one database, `BEGIN`/`COMMIT`/`ROLLBACK` is the whole story and the database guarantees atomicity for free. The moment one step is a third-party API — charging a card, calling a shipping provider — that guarantee evaporates: there is no transaction that spans your database and someone else's payment gateway. The only tool left is the compensating action. Each step records what it did; on a later failure the service runs the inverse operations, in reverse order, to undo the work that already landed. Reserving inventory is paired with releasing it; the service tracks every successful reservation in a slice and, if the charge is declined, releases them all. Reverse order matters whenever later steps depend on earlier ones, and tracking *every* successful step (not just the last) is what stops inventory from slowly leaking on each failed checkout. This is the Saga idea in miniature, and it is why the order's status, not a database transaction, is the real source of truth about whether the order placed.

### A Transaction Boundary Is Itself An Injectable Dependency

Compensation is for steps that *cannot* share a transaction. When the steps *can* — debiting one account and crediting another in the same database — you want real atomicity, and the cleanest way to give a service that power without coupling it to a specific database is to make the transaction boundary an interface. A unit of work exposes one method, conventionally "run this closure inside a transaction": it begins a transaction, hands the closure a transaction-scoped store, commits if the closure returns nil, and rolls back if the closure returns any error. The service's job shrinks to writing the closure — load both accounts, check the balance, debit, credit — and trusting the boundary to make it all-or-nothing. The debit and the credit either both commit or both vanish, even if the closure already applied the debit to its working copy before the credit step failed. The service never writes `Begin` or `Commit`; it expresses intent and lets the unit of work enforce atomicity. Swapping the in-memory implementation for one backed by a real `*sql.Tx` changes no business logic at all.

### Validation And Error Mapping Are The Service's Border Control

A service sits on two borders, and it polices both. On the inbound border it validates untrusted input *before* any work begins: an order with no items, a negative quantity, a malformed email. Good validation runs every check in one pass and returns all the problems at once — a structured error listing each bad field — so a caller fixes everything in one round-trip instead of discovering errors one at a time. On the outbound border it translates storage failures into the domain's own vocabulary: a repository's "unique constraint violated" becomes the service's "email already registered," and any unrecognized storage error is hidden behind a generic internal error rather than leaked. The reason is coupling. If callers branch on a storage-specific error, swapping Postgres for anything else breaks every caller; if they branch on the service's stable domain errors, the storage layer is free to change. Sentinel errors wrapped with `%w` (recovered with `errors.Is`) and typed errors (recovered with `errors.As`) are the two tools that make this boundary precise.

### Non-Critical Side Effects Must Not Block The Main Path

A customer who has paid should not lose their order because an email server is down. Sending the confirmation is best-effort: log the failure and return success, because the order is already charged and saved by the time the notification is attempted. Returning a notification error as the operation's error is one of the most common and most damaging service-layer bugs — a transient SMTP outage cancels orders that completed perfectly. The discipline is to classify every step as critical (its failure aborts and compensates) or non-critical (its failure is logged and swallowed), and to be honest about which is which. Most side effects that happen *after* the point of no return are non-critical by construction.

## Common Mistakes

### Treating The Service Like A Repository

The wrong instinct is to give the service methods that hide whether a save goes to SQL or to memory, so the service ends up holding a database handle and issuing queries. The service is then untestable without a database and the business rules are buried inside data access. The fix is the narrow consumer-defined interface: the service depends on verbs (`Save`, `Reserve`, `Charge`), and the concrete storage implements them. The service never knows whether the data lives in Postgres, in memory, or behind an HTTP call.

### Using A Database Transaction To Wrap An External API Call

Writing `tx.Begin(); ...; processor.Charge(...); tx.Commit()` where `Charge` is an HTTP request looks atomic but is not. The charge can succeed while the commit fails on a network blip, and now the customer is charged for an order that was never recorded. The fix is to recognize that the database transaction cannot extend across the API boundary, and to use a compensating action instead: record what each step did and undo it on failure. Conversely, do not reach for compensation when a plain transaction would do — two writes to the same database belong inside one unit of work, not a hand-rolled saga.

### Compensating Only The Last Step

On a payment failure, releasing only the most recently reserved item leaves every earlier reservation stranded, slowly draining stock for orders that never placed. The fix is to track every successful reservation in a slice and release them all, in reverse order, on any later failure. The slice lives in the use-case method and disappears when the call returns; it is the entire compensation mechanism.

### Returning Notification Failures As Operation Failures

`if err := notifier.Send(...); err != nil { return err }` turns a best-effort side effect into a hard failure, cancelling orders that were already charged and saved whenever the mail server hiccups. The fix is to log the notification failure and return success. The order is the operation; the notification is a courtesy.

### Leaking Storage Errors Through The Service Boundary

Letting a repository's `ErrConflict` or `sql.ErrNoRows` propagate unchanged means every caller now depends on the storage technology. Swap the database and every caller's error handling breaks. The fix is to map storage errors to the service's own domain errors at the boundary — a uniqueness clash becomes `ErrEmailTaken`, an unknown failure becomes a generic `ErrInternal` — keeping the original cause attached with `%v` for logs but never exposing it as the thing callers branch on.

### Validating Field By Field With Early Returns

Returning on the first bad field forces the caller into a frustrating loop: fix the email, resubmit, learn the name is empty, fix it, resubmit, learn the age is out of range. The fix is to run every check in one pass, accumulate the problems into a structured validation error, and return them all together so the caller corrects the input in a single round-trip.

---

Next: [01-order-service.md](01-order-service.md)
