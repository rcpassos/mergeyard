package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
	webassets "github.com/rcpassos/mergeyard/web"
)

// Scheduler is the scheduler control boundary. Implementations must be safe
// for concurrent requests; pause stops new claims while existing runs continue.
// The scheduler owns publication of scheduler.paused and scheduler.resumed.
type Scheduler interface {
	Pause(context.Context) error
	Resume(context.Context) error
	Paused() bool
}

var errorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Server serves the local dashboard and the runtime's shared event bus.
type Server struct {
	bus       *events.Bus
	scheduler Scheduler
	templates *template.Template
	// token is the browser CSRF token. It is rendered only into pages served
	// to an authenticated dashboard session.
	token string
	// secret authenticates the CLI API. It is never rendered into a page.
	secret []byte
	// owner is the dashboard owner credential held in origin-scoped browser
	// storage; it rotates with the process, so a restarted runtime locks old
	// browser sessions out.
	owner ownerCredential
	// tokens holds single-use dashboard link tokens until they are exchanged.
	tokens   map[string]time.Time
	tokensMu sync.Mutex
	handler  http.Handler
	// operations and workspace are set for the runtime-backed dashboard.
	operations Operations
	workspace  string
	dashboard  *dashboard
}

const (
	// DashboardSessionHeader carries the owner session. The dashboard script
	// keeps the value in origin-scoped storage, so the browser sends it only to
	// the runtime's exact scheme, host and port. Cookies cannot do that: a
	// cookie for 127.0.0.1 is shared with every port on the host.
	DashboardSessionHeader = "X-Mergeyard-Session"
	// dashboardSessionFragment hand-off key. The exchange redirect puts the
	// session in the URL fragment, which the browser never sends to a server,
	// and the dashboard script moves it into origin-scoped storage.
	dashboardSessionFragment = "session"
	// DashboardTokenLifetime bounds how long a printed link stays usable.
	DashboardTokenLifetime = 15 * time.Minute
)

// ownerCredential is the dashboard owner session value together with the exact
// authority it was handed over for. Browser storage already binds the value to
// scheme, host and port; the runtime checks the authority as well, so a request
// to another loopback port stays locked even if it somehow presents the value.
type ownerCredential struct {
	mu     sync.RWMutex
	value  string
	origin string
}

// bindOrigin records the authority the owner followed the one-time link at.
func (o *ownerCredential) bindOrigin(origin string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.origin = origin
}

// authenticated reports whether the request carries the credential for the
// authority it was handed over at.
func (o *ownerCredential) authenticated(r *http.Request) bool {
	o.mu.RLock()
	origin := o.origin
	o.mu.RUnlock()
	if origin == "" || r.Host != origin {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(DashboardSessionHeader)), []byte(o.value)) == 1
}

// New constructs an HTTP handler without opening a socket. The caller owns the
// bus and scheduler and must keep them alive until the server stops.
func New(bus *events.Bus, scheduler Scheduler) (*Server, error) {
	return newServer(bus, scheduler, nil, "")
}

func newServer(bus *events.Bus, scheduler Scheduler, operations Operations, workspace string) (*Server, error) {
	if bus == nil || scheduler == nil {
		return nil, &fault.Error{Code: "internal.web_dependencies", Message: "Web server requires an event bus and scheduler"}
	}
	templates, err := parseTemplates()
	if err != nil {
		return nil, &fault.Error{Code: "internal.web_templates", Message: "Could not parse dashboard templates", Err: err}
	}
	var csrf, session [32]byte
	if _, err := rand.Read(csrf[:]); err != nil {
		return nil, &fault.Error{Code: "internal.web_csrf", Message: "Could not generate CSRF token", Err: err}
	}
	if _, err := rand.Read(session[:]); err != nil {
		return nil, &fault.Error{Code: "internal.web_session", Message: "Could not generate the dashboard session", Err: err}
	}
	s := &Server{bus: bus, scheduler: scheduler, operations: operations, workspace: workspace, templates: templates,
		token: hex.EncodeToString(csrf[:]), owner: ownerCredential{value: hex.EncodeToString(session[:])}, tokens: make(map[string]time.Time)}
	static, err := fs.Sub(webassets.Files, "static")
	if err != nil {
		return nil, &fault.Error{Code: "internal.web_assets", Message: "Could not open embedded assets", Err: err}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /queue", s.page)
	mux.HandleFunc("GET /runs/{id}", s.page)
	mux.HandleFunc("GET /runs/{id}/output", s.output)
	mux.HandleFunc("GET /settings", s.page)
	mux.HandleFunc("POST /runs/{id}/stop", s.stopRunPage)
	mux.HandleFunc("POST /runs/{id}/retry", s.retryRunPage)
	mux.HandleFunc("POST /runs/{id}/takeover", s.takeoverRunPage)
	mux.HandleFunc("POST /runs/{id}/handback", s.handbackRunPage)
	mux.HandleFunc("POST /runs/{id}/requeue", s.requeueRunPage)
	mux.HandleFunc("GET /scheduler", func(w http.ResponseWriter, r *http.Request) { s.render(w, "scheduler") })
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /events", s.stream)
	mux.HandleFunc("POST /scheduler/pause", s.control)
	mux.HandleFunc("POST /scheduler/resume", s.control)
	if operations != nil {
		mux.HandleFunc("POST /harnesses/{harness}/check", s.checkHarness)
		mux.HandleFunc("GET /api/status", s.status)
		mux.HandleFunc("GET /api/plan", s.plan)
		mux.HandleFunc("GET /api/runs/{id}/watch", s.watchRun)
		mux.HandleFunc("POST /api/runs/{id}/stop", s.stopRun)
		mux.HandleFunc("POST /api/runs/{id}/retry", s.retryRun)
		mux.HandleFunc("POST /api/runs/{id}/takeover", s.takeoverRun)
		mux.HandleFunc("POST /api/runs/{id}/handback", s.handbackRun)
		mux.HandleFunc("POST /api/runs/{id}/requeue", s.requeueRun)
		mux.HandleFunc("POST /api/scheduler/pause", s.control)
		mux.HandleFunc("POST /api/scheduler/resume", s.control)
		mux.HandleFunc("POST /api/harnesses/{harness}/check", s.checkHarness)
		mux.HandleFunc("POST /api/dashboard/session", s.dashboardSession)
	}
	// The CLI API answers only callers that can prove they share the workspace
	// secret, and the runtime proves itself in return. The browser dashboard
	// answers only the owner session set by the one-time link printed by
	// mergeyard start, and keeps its origin, body and CSRF requirements.
	s.handler = s.dashboardGate(mux)
	if operations != nil {
		if s.secret, err = runtimeauth.EnsureSecret(workspace); err != nil {
			return nil, err
		}
		s.handler = runtimeauth.APIHandler(s.handler, s.secret)
	}
	return s, nil
}

// NewDashboardToken mints a single-use dashboard link token. mergeyard start
// prints it and mergeyard open requests a fresh one; the exchange hands the
// owner session to origin-scoped browser storage.
func (s *Server) NewDashboardToken() string {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return ""
	}
	value := hex.EncodeToString(token[:])
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	now := time.Now()
	for existing, expiry := range s.tokens {
		if now.After(expiry) {
			delete(s.tokens, existing)
		}
	}
	s.tokens[value] = now.Add(DashboardTokenLifetime)
	return value
}

// consumeToken exchanges one link token for the owner session. Tokens are
// single-use, so a captured link cannot be replayed.
func (s *Server) consumeToken(token string) bool {
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	expiry, ok := s.tokens[token]
	if !ok {
		return false
	}
	delete(s.tokens, token)
	return time.Now().Before(expiry)
}

// dashboardGate locks the dashboard to the owner session. API and asset routes
// pass through to their own boundary. Every POST still requires an exact Origin
// and the CSRF token: dashboard forms get it from an owner page, and the CLI
// gets it from the proof-protected status snapshot.
func (s *Server) dashboardGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		api := strings.HasPrefix(r.URL.Path, "/api/")
		if !api && !s.owner.authenticated(r) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				w.Header().Add("Vary", "HX-Request")
				w.Header().Add("Vary", DashboardSessionHeader)
				if r.URL.Path == "/events" {
					http.Error(w, "This dashboard is locked to the workspace owner; run mergeyard open for a new link.", http.StatusForbidden)
					return
				}
				// A browser navigation has no script-provided header yet. The
				// script moves the stored session into the request; a page
				// without one stays on the lock message.
				if r.Header.Get("HX-Request") == "true" {
					s.renderData(w, "locked-content", nil)
					return
				}
				page, title := pageMeta(r.URL.Path)
				s.renderData(w, "layout", pageData{Boot: true, Page: page, Title: title, Path: r.URL.Path})
				return
			}
			http.Error(w, "This dashboard is locked to the workspace owner; run mergeyard open for a new link.", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if r.Header.Get("Origin") != "http://"+r.Host || strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
				http.Error(w, "Invalid request origin", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Invalid request body", http.StatusBadRequest)
				return
			}
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostForm.Get("csrf_token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
				http.Error(w, "Invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Listen accepts only a numeric 127.0.0.1 address, including port zero for
// ephemeral listeners. Wildcards, other interfaces, and DNS names are rejected.
func Listen(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return nil, &fault.Error{Code: "config.web_address", Path: address, Message: "Dashboard must bind to 127.0.0.1:<port>", Err: err}
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, &fault.Error{Code: "internal.web_listen", Path: address, Message: "Could not listen for dashboard requests", Err: err}
	}
	return listener, nil
}

// Serve owns a loopback listener until cancellation, closes SSE streams and
// drains in-flight HTTP requests before returning. It does not close the bus.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return &fault.Error{Code: "config.web_address", Message: "Dashboard listener must be bound to 127.0.0.1"}
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpServer := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return serveCtx }}
	done := make(chan struct{})
	shutdown := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			grace, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			err := httpServer.Shutdown(grace)
			if err != nil {
				httpServer.Close()
			}
			shutdown <- err
		case <-done:
		}
	}()
	err := httpServer.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		if err := <-shutdown; err != nil {
			return &fault.Error{Code: "internal.web_shutdown", Message: "Could not drain dashboard requests", Err: err}
		}
		return nil
	}
	return &fault.Error{Code: "internal.web_serve", Message: "Could not serve dashboard requests", Err: err}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Validate Host even on GET: DNS rebinding must never expose the token.
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil || host != "127.0.0.1" {
		http.Error(w, "Invalid dashboard host", http.StatusForbidden)
		return
	}
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && local.String() != r.Host {
		http.Error(w, "Invalid dashboard host", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/static/favicon.svg" {
		// The mark follows the color scheme through its own inline style sheet;
		// it runs no script and loads nothing.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'none'")
	} else {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	}
	if token := r.URL.Query().Get("token"); token != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/static/") {
		s.exchangeToken(w, r, token)
		return
	}
	s.handler.ServeHTTP(w, r)
}

// exchangeToken trades the one-time link for the owner session. The session
// travels in the redirect fragment, which browsers keep out of requests, and
// the dashboard script stores it for this exact origin. No cookie is set, so a
// page on any other loopback port cannot receive it.
func (s *Server) exchangeToken(w http.ResponseWriter, r *http.Request, token string) {
	if !s.consumeToken(token) {
		http.Error(w, "This dashboard link is invalid or expired; run mergeyard open for a new one.", http.StatusForbidden)
		return
	}
	s.owner.bindOrigin(r.Host)
	target := r.URL.Path
	if target == "" {
		target = "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target+"#"+dashboardSessionFragment+"="+s.owner.value, http.StatusSeeOther)
}

func (s *Server) render(w http.ResponseWriter, name string) {
	data := struct {
		Paused bool
		Token  string
	}{s.scheduler.Paused(), s.token}
	s.renderData(w, name, data)
}

func (s *Server) renderData(w http.ResponseWriter, name string, data any) {
	var body bytes.Buffer
	if err := s.templates.ExecuteTemplate(&body, name, data); err != nil {
		http.Error(w, "Could not render dashboard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(body.Bytes())
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	var err error
	if strings.HasSuffix(r.URL.Path, "/pause") {
		err = s.scheduler.Pause(r.Context())
	} else {
		err = s.scheduler.Resume(r.Context())
	}
	if err != nil {
		s.actionError(w, r, err)
		return
	}
	if r.Header.Get("Accept") == "application/json" {
		s.json(w, struct {
			Paused bool `json:"paused"`
		}{s.scheduler.Paused()})
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "scheduler")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func errorCode(err error, fallback string) string {
	var failure *fault.Error
	if errors.As(err, &failure) && errorCodePattern.MatchString(failure.Code) {
		return failure.Code
	}
	return fallback
}

func logDashboardError(ctx context.Context, action string, err error) {
	slog.ErrorContext(ctx, "Dashboard action failed", "action", action, "error_code", errorCode(err, "internal.dashboard"), "error", err)
}

func (s *Server) actionError(w http.ResponseWriter, r *http.Request, err error) {
	s.respondError(w, r, err, "internal.scheduler_control", http.StatusInternalServerError, "Could not complete the action. Check the application log for details.")
}

func (s *Server) respondError(w http.ResponseWriter, r *http.Request, err error, fallback string, status int, message string) {
	code := errorCode(err, fallback)
	slog.ErrorContext(r.Context(), "Dashboard action failed", "error_code", code, "action", r.URL.Path, "error", err)
	w.Header().Set("X-Mergeyard-Error-Code", code)
	if code == "internal.run_not_found" {
		status = http.StatusNotFound
	}
	if code == "internal.transition_invalid" || code == "run.terminal" || code == "phase.not_running" {
		status = http.StatusConflict
	}
	http.Error(w, code+": "+message, status)
}
