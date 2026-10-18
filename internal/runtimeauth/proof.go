package runtimeauth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// Verified reports whether a request proves the workspace secret. Requests
// without a proof, such as browser requests, are not verified and receive no
// response proof.
func Verified(request *http.Request, secret []byte, now time.Time) bool {
	return VerifyRequest(request, secret, now) == nil
}

// VerifyResponse checks that the runtime proved knowledge of the workspace
// secret for one signed request, binding the exact status and body it answered
// with so a reply cannot be swapped or edited in transit.
func VerifyResponse(response *http.Response, secret []byte, body []byte) error {
	if response.Request == nil {
		return ErrUnauthenticated
	}
	nonce := response.Request.Header.Get(NonceHeader)
	if !validNonce(nonce) {
		return ErrUnauthenticated
	}
	expected := ResponseProof(secret, nonce, response.StatusCode, body)
	if !hmac.Equal([]byte(response.Header.Get(ProofHeader)), []byte(expected)) {
		return ErrUnauthenticated
	}
	return nil
}

// ResponseProof returns the proof a runtime attaches to its answer to one
// signed request.
func ResponseProof(secret []byte, nonce string, status int, body []byte) string {
	sum := sha256.Sum256(body)
	return proof(secret, responseMessage(nonce, status, hex.EncodeToString(sum[:])))
}

// APIHandler requires a valid proof on /api paths and passes browser requests
// through untouched. Responses to proven requests are buffered so the runtime
// can bind the proof to the exact status and body; a request that cannot prove
// the secret never receives a proof, so the runtime is not a proof oracle.
func APIHandler(next http.Handler, secret []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Verified(r, secret, time.Now()) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("X-Mergeyard-Error-Code", "runtime.unauthenticated")
				http.Error(w, "This runtime requires the workspace secret; start the CLI with this workspace's configuration.", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// Streaming responses are not proven; the CLI's API surface is small
		// JSON answers, and browsers never sign requests.
		recorder := &proofWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		recorder.finish(secret, r.Header.Get(NonceHeader))
	})
}

// proofWriter buffers one proven response so its status and body can be bound
// into the proof header before any byte reaches the client.
type proofWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *proofWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *proofWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *proofWriter) finish(secret []byte, nonce string) {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := w.body.Bytes()
	w.Header().Set(ProofHeader, ResponseProof(secret, nonce, status, body))
	w.ResponseWriter.WriteHeader(status)
	w.ResponseWriter.Write(body)
}

// requestMessage binds a proof to one method and target.
func requestMessage(method, target, nonce, seconds string) string {
	return strings.Join([]string{"request", method, target, nonce, seconds}, "\n")
}

// responseMessage binds the runtime's answer to one request, status and body.
func responseMessage(nonce string, status int, bodyHash string) string {
	return fmt.Sprintf("response\n%s\n%d\n%s", nonce, status, bodyHash)
}

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
