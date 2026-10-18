package runtimeauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// NonceHeader carries a fresh random value per request.
	NonceHeader = "X-Mergeyard-Nonce"
	// TimeHeader carries the client's request time, bounding proof reuse.
	TimeHeader = "X-Mergeyard-Time"
	// ProofHeader carries the hex HMAC-SHA256 proof.
	ProofHeader = "X-Mergeyard-Proof"

	nonceBytes = 16
	// proofWindow bounds how long a captured request proof stays usable.
	proofWindow = 30 * time.Second
)

// ErrUnauthenticated reports a request or response without valid workspace
// secret proof.
var ErrUnauthenticated = errors.New("the workspace secret was not proven")

// SignRequest adds a fresh proof of workspace-secret knowledge to one request.
// The secret itself is never sent.
func SignRequest(request *http.Request, secret []byte, at time.Time) error {
	nonce, err := newNonce()
	if err != nil {
		return err
	}
	seconds := strconv.FormatInt(at.UTC().Unix(), 10)
	request.Header.Set(NonceHeader, nonce)
	request.Header.Set(TimeHeader, seconds)
	request.Header.Set(ProofHeader, proof(secret, requestMessage(request.Method, request.URL.RequestURI(), nonce, seconds)))
	return nil
}

// VerifyRequest validates one request's proof, including its freshness window.
// It is safe to call with attacker-controlled headers.
func VerifyRequest(request *http.Request, secret []byte, now time.Time) error {
	nonce := request.Header.Get(NonceHeader)
	seconds := request.Header.Get(TimeHeader)
	if !validNonce(nonce) || seconds == "" {
		return ErrUnauthenticated
	}
	unix, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return ErrUnauthenticated
	}
	if skew := now.UTC().Sub(time.Unix(unix, 0)); skew > proofWindow || skew < -proofWindow {
		return ErrUnauthenticated
	}
	expected := requestMessage(request.Method, request.URL.RequestURI(), nonce, seconds)
	if !hmac.Equal([]byte(request.Header.Get(ProofHeader)), []byte(proof(secret, expected))) {
		return ErrUnauthenticated
	}
	return nil
}

// VerifyResponse checks that the runtime proved knowledge of the workspace
// secret for one signed request.
func VerifyResponse(response *http.Response, secret []byte) error {
	if response.Request == nil {
		return ErrUnauthenticated
	}
	nonce := response.Request.Header.Get(NonceHeader)
	if !validNonce(nonce) {
		return ErrUnauthenticated
	}
	if !hmac.Equal([]byte(response.Header.Get(ProofHeader)), []byte(proof(secret, responseMessage(nonce)))) {
		return ErrUnauthenticated
	}
	return nil
}

// APIHandler requires a valid proof on /api paths and proves the runtime on
// every response to a signed request. Browser paths pass through untouched, so
// the dashboard keeps its existing host, origin and CSRF protections.
func APIHandler(next http.Handler, secret []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ProveResponse(w, r, secret)
		if !RequireProof(w, r, secret) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ProveResponse adds the runtime's own proof to a response to a signed request.
func ProveResponse(w http.ResponseWriter, r *http.Request, secret []byte) {
	if nonce := r.Header.Get(NonceHeader); validNonce(nonce) {
		w.Header().Set(ProofHeader, ResponseProof(secret, nonce))
	}
}

// ResponseProof returns the proof a runtime attaches to its answer to one
// request nonce.
func ResponseProof(secret []byte, nonce string) string { return proof(secret, responseMessage(nonce)) }

// RequireProof writes the rejection and reports false when an API request
// cannot prove the workspace secret. Non-API paths always pass.
func RequireProof(w http.ResponseWriter, r *http.Request, secret []byte) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	if err := VerifyRequest(r, secret, time.Now()); err != nil {
		w.Header().Set("X-Mergeyard-Error-Code", "runtime.unauthenticated")
		http.Error(w, "This runtime requires the workspace secret; start the CLI with this workspace's configuration.", http.StatusUnauthorized)
		return false
	}
	return true
}

// requestMessage binds a proof to one method and target.
func requestMessage(method, target, nonce, seconds string) string {
	return strings.Join([]string{"request", method, target, nonce, seconds}, "\n")
}

// responseMessage binds the runtime's answer to one request nonce.
func responseMessage(nonce string) string { return "response\n" + nonce }

func proof(secret []byte, message string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func newNonce() (string, error) {
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
}

func validNonce(nonce string) bool {
	if len(nonce) != hex.EncodedLen(nonceBytes) {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil
}
