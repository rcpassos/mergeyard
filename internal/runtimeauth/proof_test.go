package runtimeauth_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/runtimeauth"
)

func TestSignedRequestRoundTrip(t *testing.T) {
	secret := []byte("secret-under-test")
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/runs/run-1/stop?note=one", nil)
	if err := runtimeauth.SignRequest(request, secret, time.Now()); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get(runtimeauth.ProofHeader) == "" || strings.Contains(request.Header.Get(runtimeauth.ProofHeader), string(secret)) {
		t.Fatalf("proof header leaked the secret: %q", request.Header.Get(runtimeauth.ProofHeader))
	}
	if err := runtimeauth.VerifyRequest(request, secret, time.Now()); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	if err := runtimeauth.VerifyRequest(request, []byte("other-secret"), time.Now()); err == nil {
		t.Fatal("foreign secret accepted")
	}
}

func TestVerifyRequestBindsMethodTargetTimeAndNonce(t *testing.T) {
	secret := []byte("secret-under-test")
	now := time.Now()
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Method = http.MethodGet },
		func(r *http.Request) { r.URL.RawQuery = "note=other" },
		func(r *http.Request) { r.Header.Set(runtimeauth.NonceHeader, strings.Repeat("a", 32)) },
		func(r *http.Request) { r.Header.Set(runtimeauth.TimeHeader, "1") },
		func(r *http.Request) { r.Header.Del(runtimeauth.ProofHeader) },
	} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/runs/run-1/stop", nil)
		if err := runtimeauth.SignRequest(request, secret, now); err != nil {
			t.Fatal(err)
		}
		mutate(request)
		if err := runtimeauth.VerifyRequest(request, secret, now); err == nil {
			t.Fatalf("tampered request accepted: %+v", request.Header)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil)
	if err := runtimeauth.SignRequest(request, secret, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := runtimeauth.VerifyRequest(request, secret, now); !errors.Is(err, runtimeauth.ErrUnauthenticated) {
		t.Fatalf("stale proof accepted: %v", err)
	}
}

func TestAPIHandlerProvesTheRuntimeAndProtectsAPIRoutes(t *testing.T) {
	secret := []byte("secret-under-test")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"workspace": "/workspace"})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("dashboard"))
	})
	server := httptest.NewServer(runtimeauth.APIHandler(mux, secret))
	defer server.Close()

	// An unsigned API request is rejected without leaking a token.
	response, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-Mergeyard-Error-Code") != "runtime.unauthenticated" {
		t.Fatalf("unsigned API request: %d %q", response.StatusCode, response.Header.Get("X-Mergeyard-Error-Code"))
	}

	// A signed API request succeeds and the response proves the runtime.
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeauth.SignRequest(request, secret, time.Now()); err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("signed API request: %d", response.StatusCode)
	}
	if err := runtimeauth.VerifyResponse(response, secret); err != nil {
		t.Fatalf("runtime proof rejected: %v", err)
	}
	if err := runtimeauth.VerifyResponse(response, []byte("other-secret")); err == nil {
		t.Fatal("foreign runtime proof accepted")
	}

	// Browser pages keep working without any secret.
	response, err = http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "dashboard" {
		t.Fatalf("browser page: %d %q %v", response.StatusCode, body, err)
	}
}
