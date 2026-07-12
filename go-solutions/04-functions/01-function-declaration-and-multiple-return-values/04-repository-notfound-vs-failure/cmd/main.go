package main

import (
	"context"
	"errors"
	"fmt"

	userrepo "github.com/sentinel/go-learning/go-solutions/04-functions/01-function-declaration-and-multiple-return-values/04-repository-notfound-vs-failure"
)

func main() {
	ctx := context.Background()
	repo := userrepo.NewRepo()
	repo.Add(userrepo.User{ID: "u1", Name: "alice"})

	if u, err := repo.FindByID(ctx, "u1"); err == nil {
		fmt.Printf("found: %s\n", u.Name)
	}
	_, err := repo.FindByID(ctx, "u2")
	fmt.Printf("absent: errors.Is(ErrNotFound)=%t\n", errors.Is(err, userrepo.ErrNotFound))
	repo.FailNextWith(errors.New("connection refused"))
	_, err = repo.FindByID(ctx, "u1")
	fmt.Printf("store down: errors.Is(ErrNotFound)=%t err=%v\n", errors.Is(err, userrepo.ErrNotFound), err)
}
