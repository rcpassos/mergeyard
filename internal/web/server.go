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
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
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

// Server serves the local dashboard and the runtime's shared event bus.
type Server struct {
	bus       *events.Bus
	scheduler Scheduler
	templates *template.Template
	token     string
	handler   http.Handler
}

// New constructs an HTTP handler without opening a socket. The caller owns the
// bus and scheduler and must keep them alive until the server stops.
func New(bus *events.Bus, scheduler Scheduler) (*Server, error) {
	if bus == nil || scheduler == nil {
		return nil, &fault.Error{Code: "internal.web_dependencies", Message: "Web server requires an event bus and scheduler"}
	}
	templates, err := template.ParseFS(webassets.Files, "templates/*.html")
	if err != nil {
		return nil, &fault.Error{Code: "internal.web_templates", Message: "Could not parse dashboard templates", Err: err}
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, &fault.Error{Code: "internal.web_csrf", Message: "Could not generate CSRF token", Err: err}
	}
	s := &Server{bus: bus, scheduler: scheduler, templates: templates, token: hex.EncodeToString(secret[:])}
	static, err := fs.Sub(webassets.Files, "static")
	if err != nil {
		return nil, &fault.Error{Code: "internal.web_assets", Message: "Could not open embedded assets", Err: err}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.render(w, "layout") })
	mux.HandleFunc("GET /scheduler", func(w http.ResponseWriter, r *http.Request) { s.render(w, "scheduler") })
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /events", s.stream)
	mux.HandleFunc("POST /scheduler/pause", s.control)
	mux.HandleFunc("POST /scheduler/resume", s.control)
	s.handler = mux
	return s, nil
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
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
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
	s.handler.ServeHTTP(w, r)
}

func (s *Server) render(w http.ResponseWriter, name string) {
	data := struct {
		Paused bool
		Token  string
	}{s.scheduler.Paused(), s.token}
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
	if r.URL.Path == "/scheduler/pause" {
		err = s.scheduler.Pause(r.Context())
	} else {
		err = s.scheduler.Resume(r.Context())
	}
	if err != nil {
		http.Error(w, "Could not change scheduler state", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "scheduler")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
