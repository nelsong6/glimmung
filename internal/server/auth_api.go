package server

import (
	"context"
	"net/http"

	"github.com/nelsong6/glimmung/internal/auth"
)

type AuthResolver interface {
	ResolveCaller(ctx context.Context, r *http.Request) (auth.User, bool, bool)
}

type AuthMeResponse struct {
	SignedIn bool   `json:"signed_in"`
	IsAdmin  bool   `json:"is_admin"`
	Sub      string `json:"sub"`
	Email    string `json:"email"`
	Name     string `json:"name"`
}

func authMe(authResolver AuthResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authResolver == nil {
			writeJSON(w, http.StatusOK, AuthMeResponse{SignedIn: false, IsAdmin: false})
			return
		}
		user, isAdmin, ok := authResolver.ResolveCaller(r.Context(), r)
		if !ok {
			writeJSON(w, http.StatusOK, AuthMeResponse{SignedIn: false, IsAdmin: false})
			return
		}
		writeJSON(w, http.StatusOK, AuthMeResponse{
			SignedIn: true,
			IsAdmin:  isAdmin,
			Sub:      user.Sub,
			Email:    user.Email,
			Name:     user.Name,
		})
	}
}
