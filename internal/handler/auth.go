package handler

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireAdmin guards destructive endpoints with a shared bearer token.
// The comparison is constant-time so response timing does not leak how much
// of a guessed token was correct.
func requireAdmin(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, presented, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
