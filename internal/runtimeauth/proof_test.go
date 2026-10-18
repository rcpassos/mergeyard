package runtimeauth_test

import (
	"bytes"
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

func TestAPIHandlerProtectsAPIRoutesAndBindsTheAnswer(t *testing.T) {
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

	// An unsigned API request is rejected and carries no proof.
	response, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-Mergeyard-Error-Code") != "runtime.unauthenticated" {
		t.Fatalf("unsigned API request: %d %q", response.StatusCode, response.Header.Get("X-Mergeyard-Error-Code"))
	}
	if response.Header.Get(runtimeauth.ProofHeader) != "" {
		t.Fatal("unproven request received a response proof")
	}

	// A signed API request succeeds and the proof binds the exact body.
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
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("signed API request: %d %v", response.StatusCode, err)
	}
	if err := runtimeauth.VerifyResponse(response, secret, body); err != nil {
		t.Fatalf("runtime proof rejected: %v", err)
	}
	if err := runtimeauth.VerifyResponse(response, secret, append(body, ' ')); !errors.Is(err, runtimeauth.ErrUnauthenticated) {
		t.Fatalf("edited body accepted: %v", err)
	}
	if err := runtimeauth.VerifyResponse(response, []byte("other-secret"), body); err == nil {
		t.Fatal("foreign runtime proof accepted")
	}

	// Browser requests keep working without a secret and receive no proof.
	response, err = http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "dashboard" {
		t.Fatalf("browser page: %d %q %v", response.StatusCode, body, err)
	}
	if response.Header.Get(runtimeauth.ProofHeader) != "" {
		t.Fatal("browser request received a response proof")
	}

	// A caller without the secret cannot use the runtime as a proof oracle:
	// sending a nonce and a guessed proof yields no proof header.
	request, err = http.NewRequest(http.MethodGet, server.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(runtimeauth.NonceHeader, strings.Repeat("a", 32))
	request.Header.Set(runtimeauth.TimeHeader, "1")
	request.Header.Set(runtimeauth.ProofHeader, strings.Repeat("0", 64))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.Header.Get(runtimeauth.ProofHeader) != "" {
		t.Fatal("unproven nonce received a response proof")
	}
}

func TestResponseProofBindsStatusAndBody(t *testing.T) {
	secret := []byte("secret-under-test")
	nonce := strings.Repeat("b", 32)
	proof := runtimeauth.ResponseProof(secret, nonce, http.StatusOK, []byte("one"))
	if proof == runtimeauth.ResponseProof(secret, nonce, http.StatusOK, []byte("two")) {
		t.Fatal("body is not bound into the response proof")
	}
	if proof == runtimeauth.ResponseProof(secret, nonce, http.StatusAccepted, []byte("one")) {
		t.Fatal("status is not bound into the response proof")
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil)
	request.Header.Set(runtimeauth.NonceHeader, nonce)
	response := &http.Response{Request: request, StatusCode: http.StatusOK, Header: http.Header{runtimeauth.ProofHeader: {proof}}}
	if err := runtimeauth.VerifyResponse(response, secret, []byte("one")); err != nil {
		t.Fatalf("matching body rejected: %v", err)
	}
	if err := runtimeauth.VerifyResponse(response, secret, bytes.Repeat([]byte("x"), 1024)); err == nil {
		t.Fatal("different body accepted")
	}
}
