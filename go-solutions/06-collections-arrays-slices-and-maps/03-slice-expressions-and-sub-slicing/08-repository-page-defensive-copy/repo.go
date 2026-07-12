package repo

import (
	"slices"
	"sync"
)

type Item struct {
	ID   int
	Name string
}

type Repo struct {
	mu    sync.RWMutex
	items []Item
}

func New(items []Item) *Repo {
	return &Repo{
		items: slices.Clone(items),
	}
}

func (r *Repo) Page(offset, limit int) []Item {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if offset < 0 {
		offset = 0
	}
	if offset > len(r.items) {
		offset = len(r.items)
	}
	hi := offset + limit
	if limit < 0 || hi < offset {
		hi = offset
	}
	if hi > len(r.items) {
		hi = len(r.items)
	}
	return slices.Clone(r.items[offset:hi:hi])
}

func (r *Repo) Set(i int, it Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[i] = it
}

func (r *Repo) Append(it Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, it)
}

func (r *Repo) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}
