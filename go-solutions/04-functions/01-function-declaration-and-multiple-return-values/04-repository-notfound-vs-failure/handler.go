package userrepo

import (
	"errors"
	"net/http"
)

func UserHandler(repo *Repo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		u, err := repo.FindByID(r.Context(), id)
		switch {
		case errors.Is(err, ErrNotFound):
			http.Error(w, "user not found", http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(u.Name))
	}
}
