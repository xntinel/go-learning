package main

import (
	"errors"
	"fmt"

	user "github.com/sentinel/go-learning/go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/01-user-entity-and-validating-constructor"
)

func main() {
	u, err := user.New("  u1 ", " Alice ", "alice@example.com")
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("display=%s email=%s utc=%t\n",
		u.DisplayName(), u.Email, u.CreatedAt.Location() == u.CreatedAt.UTC().Location())

	bare := user.User{ID: "u2"}
	fmt.Printf("fallback=%s empty=%t\n", bare.DisplayName(), bare.IsEmpty())

	if _, err := user.New("  ", "Bob", ""); errors.Is(err, user.ErrEmptyID) {
		fmt.Println("rejected: empty id")
	}
}
