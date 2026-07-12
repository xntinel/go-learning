package user

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrEmptyID   = errors.New("user id is required")
	ErrEmptyName = errors.New("user name is required")
)

type User struct {
	ID        string
	Name      string
	Email     string
	CreatedAt time.Time
}

func New(id, name, email string) (User, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		return User{}, ErrEmptyID
	}
	if name == "" {
		return User{}, ErrEmptyName
	}
	return User{
		ID:        id,
		Name:      name,
		Email:     strings.TrimSpace(email),
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (u User) IsEmpty() bool {
	return u.ID == "" && u.Name == "" && u.Email == ""
}

func (u User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	return u.ID
}
