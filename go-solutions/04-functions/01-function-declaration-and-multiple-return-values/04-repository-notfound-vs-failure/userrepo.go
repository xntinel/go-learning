package userrepo

import (
	"context"
	"errors"
	"fmt"
)

var ErrNotFound = errors.New("user not found")

type User struct {
	ID   string
	Name string
}

type Repo struct {
	users    map[string]User
	failNext error
}

func NewRepo() *Repo {
	return &Repo{
		users: make(map[string]User),
	}
}

func (r *Repo) Add(u User) {
	r.users[u.ID] = u
}

func (r *Repo) FailNextWith(err error) {
	r.failNext = err
}

func (r *Repo) FindByID(ctx context.Context, id string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, fmt.Errorf("find user %q: %w", id, err)
	}
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return User{}, fmt.Errorf("find user %q: %w", id, err)
	}
	u, ok := r.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}
