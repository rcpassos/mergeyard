package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/rcpassos/mergeyard/internal/runtimeauth"
)

// runtimeServer serves a fake runtime that shares the workspace secret the way
// the real runtime does. Handlers see only requests that proved the secret.
func runtimeServer(t *testing.T, root string, handler http.Handler) *httptest.Server {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	secret, err := runtimeauth.EnsureSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(runtimeauth.APIHandler(handler, secret))
}
