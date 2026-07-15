package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mitchellh/go-homedir"
)

// loadOrCreateToken returns the access token used to protect the admin UI
// and GraphQL API. If `explicit` is non-empty (e.g. passed via --token) it
// is used as-is. Otherwise, a token persisted at ~/.watchersid/token is
// reused, or a new random one is generated and stored there on first run.
//
// This exists so that exposing WatchersID on a LAN or port-forwarded
// address doesn't hand a random script kiddie full read/write access to
// your intercepted traffic, projects and replay history.
func loadOrCreateToken(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	dir, err := homedir.Expand("~/.watchersid")
	if err != nil {
		return "", fmt.Errorf("failed to resolve config dir: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create config dir: %w", err)
	}

	tokenFile := filepath.Join(dir, "token")

	if data, err := os.ReadFile(tokenFile); err == nil && len(data) > 0 {
		return string(data), nil
	}

	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}

	token := hex.EncodeToString(raw)

	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("failed to persist token: %w", err)
	}

	return token, nil
}

// requireToken wraps a handler with HTTP Basic Auth, requiring the token as
// the password (username is ignored). Browsers will show a native login
// prompt for the admin UI; GraphQL/API clients can send the token via the
// standard Authorization: Basic header.
func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="WatchersID admin"`)
			http.Error(w, "Unauthorized: a valid WatchersID token is required.\n", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}
