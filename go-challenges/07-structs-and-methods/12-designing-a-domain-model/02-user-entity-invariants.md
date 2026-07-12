# Exercise 2: A User as an Entity with Enforced Invariants

A `User` is an entity: it has a stable identity, and it evolves — renames,
deposits, withdrawals — while staying the same user. This module builds it with
unexported fields, a validating constructor, read-only accessors, and value-
receiver methods that return a modified copy while preserving the balance-non-
negative invariant.

This module is fully self-contained: its own `go mod init`, all code inline
(including a small embedded `Balance`), its own demo and tests. Nothing here
imports another exercise.

## What you'll build

```text
user/                       independent module: example.com/user
  go.mod                    go 1.26
  user.go                   Balance value object + User entity; NewUser, accessors, Rename/Deposit/Withdraw
  cmd/
    demo/
      main.go               runnable demo: create, rename, deposit, withdraw, overdraw
  user_test.go              table tests: reject empty fields, rename immutability, deposit/withdraw, overdraw
```

- Files: `user.go`, `cmd/demo/main.go`, `user_test.go`.
- Implement: a `User` with unexported `id`/`name`/`email`/`balance`, `NewUser` validating each field, `ID`/`Name`/`Email`/`Balance` accessors, and `Rename`/`Deposit`/`Withdraw` returning a modified copy.
- Test: `NewUser` rejects each empty field with its sentinel; `Rename("")` is rejected and leaves the original unchanged; deposit-then-withdraw drives the balance end-to-end; a withdraw past the balance returns `ErrOverdraw` without mutating the receiver.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/12-designing-a-domain-model/02-user-entity-invariants/cmd/demo
cd go-solutions/07-structs-and-methods/12-designing-a-domain-model/02-user-entity-invariants
```

### Identity, not fields, defines an entity

A value object is defined by its fields; an entity is defined by its identity. A
`User` keeps the same id through every rename and every balance change — it is the
*same* user even after all its other fields have changed. That is why the id is
validated once at construction and then never mutated: it is the thing that stays
constant while the state evolves.

The construction discipline is identical to the value object's. Fields are
unexported, so no external package can assign them; `NewUser` is the only door and
it rejects an empty id, name, or email, each with its own sentinel error so a
caller can tell exactly which field was wrong. Read-only accessors (`ID`, `Name`,
`Email`, `Balance`) expose the state without exposing a way to corrupt it.

The mutating methods use the copy-return idiom. `Rename`, `Deposit`, and
`Withdraw` all take a *value* receiver, so they operate on a copy of the `User`;
they modify that copy and return it, leaving the caller's original untouched. This
gives the entity an immutable feel at the call site — `u2, err := u.Withdraw(amt)`
never changes `u` — which makes accidental shared mutation impossible. Crucially,
each method preserves the invariant on the way through: `Rename("")` is rejected
and returns the receiver unchanged with `ErrEmptyName`; `Withdraw` delegates to
`Balance.Sub`, so a withdraw past the balance returns `ErrOverdraw` and does not
apply the change. The balance-non-negative invariant holds no matter the sequence
of operations.

Create `user.go`:

```go
package user

import (
	"errors"
	"fmt"
)

var (
	ErrEmptyID     = errors.New("user: id is required")
	ErrEmptyName   = errors.New("user: name is required")
	ErrEmptyEmail  = errors.New("user: email is required")
	ErrNonNegative = errors.New("user: amount must be non-negative")
	ErrOverdraw    = errors.New("user: cannot withdraw more than the balance")
)

// Balance is an immutable value object embedded in the User entity.
type Balance struct {
	amount int64
	cents  int64
}

func NewBalance(amount, cents int64) (Balance, error) {
	if amount < 0 || cents < 0 {
		return Balance{}, fmt.Errorf("%w: got amount=%d cents=%d", ErrNonNegative, amount, cents)
	}
	return Balance{amount: amount, cents: cents}, nil
}

func (b Balance) Total() int64 { return b.amount*100 + b.cents }

func (b Balance) Add(other Balance) Balance {
	total := b.Total() + other.Total()
	return Balance{amount: total / 100, cents: total % 100}
}

func (b Balance) Sub(other Balance) (Balance, error) {
	total := b.Total() - other.Total()
	if total < 0 {
		return Balance{}, ErrOverdraw
	}
	return Balance{amount: total / 100, cents: total % 100}, nil
}

// User is an entity: defined by its id, mutated through value-receiver methods
// that return a modified copy while preserving invariants.
type User struct {
	id      string
	name    string
	email   string
	balance Balance
}

// NewUser is the only door into a User. It validates identity and required fields.
func NewUser(id, name, email string) (User, error) {
	if id == "" {
		return User{}, ErrEmptyID
	}
	if name == "" {
		return User{}, ErrEmptyName
	}
	if email == "" {
		return User{}, ErrEmptyEmail
	}
	return User{id: id, name: name, email: email}, nil
}

func (u User) ID() string       { return u.id }
func (u User) Name() string     { return u.name }
func (u User) Email() string    { return u.email }
func (u User) Balance() Balance { return u.balance }

// Rename returns a copy with the new name, or the receiver unchanged on error.
func (u User) Rename(name string) (User, error) {
	if name == "" {
		return u, ErrEmptyName
	}
	u.name = name
	return u, nil
}

// Deposit returns a copy whose balance is increased by amount.
func (u User) Deposit(amount Balance) User {
	u.balance = u.balance.Add(amount)
	return u
}

// Withdraw returns a copy whose balance is reduced, or the receiver unchanged
// with ErrOverdraw if the balance would go negative.
func (u User) Withdraw(amount Balance) (User, error) {
	newBalance, err := u.balance.Sub(amount)
	if err != nil {
		return u, err
	}
	u.balance = newBalance
	return u, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/user"
)

func main() {
	u, err := user.NewUser("u1", "Alice", "alice@example.com")
	if err != nil {
		panic(err)
	}

	u, _ = u.Rename("Alice Smith")
	fmt.Printf("name: %s\n", u.Name())

	deposit, _ := user.NewBalance(100, 0)
	u = u.Deposit(deposit)
	fmt.Printf("balance after deposit: %d cents\n", u.Balance().Total())

	withdraw, _ := user.NewBalance(30, 0)
	u, _ = u.Withdraw(withdraw)
	fmt.Printf("balance after withdraw: %d cents\n", u.Balance().Total())

	tooMuch, _ := user.NewBalance(1000, 0)
	if _, err := u.Withdraw(tooMuch); errors.Is(err, user.ErrOverdraw) {
		fmt.Println("overdraw rejected; balance unchanged")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name: Alice Smith
balance after deposit: 10000 cents
balance after withdraw: 7000 cents
overdraw rejected; balance unchanged
```

### Tests

The tests pin each invariant. Every empty field is rejected with its own sentinel;
`Rename("")` is rejected *and* leaves the original `User` unchanged (the copy-
return idiom means the receiver is never touched on the error path); deposit-then-
withdraw drives the balance end-to-end; and an overdraw returns `ErrOverdraw`
without mutating the receiver, which the test verifies by reading the original's
balance after the failed call.

Create `user_test.go`:

```go
package user

import (
	"errors"
	"testing"
)

func TestNewUserRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		id, uname, email string
		want             error
	}{
		{name: "empty id", id: "", uname: "Alice", email: "a@b", want: ErrEmptyID},
		{name: "empty name", id: "u1", uname: "", email: "a@b", want: ErrEmptyName},
		{name: "empty email", id: "u1", uname: "Alice", email: "", want: ErrEmptyEmail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewUser(tt.id, tt.uname, tt.email); !errors.Is(err, tt.want) {
				t.Fatalf("NewUser err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRenameRejectsEmptyAndKeepsOriginal(t *testing.T) {
	t.Parallel()
	u, _ := NewUser("u1", "Alice", "a@b")
	if _, err := u.Rename(""); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("Rename(\"\") err = %v, want ErrEmptyName", err)
	}
	if u.Name() != "Alice" {
		t.Fatalf("original mutated by failed Rename: %q", u.Name())
	}
}

func TestDepositThenWithdraw(t *testing.T) {
	t.Parallel()
	u, _ := NewUser("u1", "Alice", "a@b")

	deposit, _ := NewBalance(100, 0)
	u = u.Deposit(deposit)
	if got := u.Balance().Total(); got != 10000 {
		t.Fatalf("after deposit balance = %d, want 10000", got)
	}

	withdraw, _ := NewBalance(30, 0)
	u, err := u.Withdraw(withdraw)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Balance().Total(); got != 7000 {
		t.Fatalf("after withdraw balance = %d, want 7000", got)
	}
}

func TestWithdrawOverdrawDoesNotMutate(t *testing.T) {
	t.Parallel()
	u, _ := NewUser("u1", "Alice", "a@b")
	deposit, _ := NewBalance(50, 0)
	u = u.Deposit(deposit)

	tooMuch, _ := NewBalance(100, 0)
	if _, err := u.Withdraw(tooMuch); !errors.Is(err, ErrOverdraw) {
		t.Fatalf("Withdraw err = %v, want ErrOverdraw", err)
	}
	if got := u.Balance().Total(); got != 5000 {
		t.Fatalf("overdraw mutated the receiver: balance = %d, want 5000", got)
	}
}
```

## Review

The `User` is correct when identity is fixed at construction and every state
change flows through a method that preserves the invariant. The two subtle tests
are the immutability ones: `Rename("")` and the overdraw both return the receiver
*unchanged*, which only holds because the methods use value receivers and mutate a
copy. The mistakes to avoid are exporting the fields (a caller sets a negative
balance directly and every check is void) and mixing semantics — a `User` is an
entity, so it has identity and a lifecycle, unlike the `Balance` value it holds.
Run `go test -race` to confirm no shared mutable state leaks across the value-
receiver methods.

## Resources

- [Effective Go: Methods (pointers vs. values)](https://go.dev/doc/effective_go#methods) — why value receivers give copy-return semantics.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — field visibility.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-balance-value-object.md](01-balance-value-object.md) | Next: [03-value-object-equality-and-map-keys.md](03-value-object-equality-and-map-keys.md)
