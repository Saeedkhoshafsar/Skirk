package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// VersionInfo carries the build-time identification that the binary already
// knows about. The CLI layer passes it through to /api/status so the web UI
// can display "skirk vX.Y.Z (commit, date)".
type VersionInfo struct {
	Version string
	Commit  string
	Date    string
}

// Config bundles everything needed to spin up the web UI HTTP server.
type Config struct {
	// Addr is the "host:port" the server listens on. Required.
	Addr string

	// KitDir points at the skirk kit (the directory holding exit.json,
	// client.json, web-token, ...). Required.
	KitDir string

	// Auth controls who can hit the API. When NoAuth is true, the
	// constructor refuses to start unless the bind address is loopback
	// (or AllowPublicNoAuth is explicitly set).
	Auth AuthConfig

	// AllowPublicNoAuth is an escape hatch for running --no-auth on a
	// non-loopback bind. Strongly discouraged; only honoured when the
	// caller has acknowledged the risk on the CLI.
	AllowPublicNoAuth bool

	// TLSCertFile / TLSKeyFile, when both set, switch the server to HTTPS.
	TLSCertFile string
	TLSKeyFile  string

	// Ops is the implementation of MailboxOps the API delegates to. May
	// be nil during unit tests for read-only endpoints; mutating endpoints
	// will return 503 in that case.
	Ops MailboxOps

	// Version is the build-time version triple displayed by /api/status.
	Version VersionInfo

	// Logger is used for request and error logging. Defaults to log.Default()
	// when nil.
	Logger *log.Logger
}

// Server is the running Skirk Web UI HTTP server. It is created with New and
// driven by Serve; tests can also extract the http.Handler via Handler() to
// drive requests in-process.
type Server struct {
	cfg         Config
	auth        *authenticator
	ops         MailboxOps
	kitDir      string
	startedAt   time.Time
	versionInfo VersionInfo
	logger      *log.Logger

	mux *http.ServeMux
	srv *http.Server
}

// New constructs a Server. It validates the configuration and refuses unsafe
// combinations (e.g. --no-auth on a non-loopback bind without an explicit
// override).
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("web: Addr is required")
	}
	if strings.TrimSpace(cfg.KitDir) == "" {
		return nil, errors.New("web: KitDir is required")
	}
	if cfg.Auth.NoAuth {
		if !cfg.AllowPublicNoAuth && !addrIsLoopback(cfg.Addr) {
			return nil, fmt.Errorf("web: --no-auth refused on non-loopback bind %q; pass --allow-public-no-auth to override (NOT recommended)", cfg.Addr)
		}
	} else if strings.TrimSpace(cfg.Auth.Token) == "" {
		return nil, errors.New("web: auth token is required unless --no-auth is set")
	}
	if (cfg.TLSCertFile != "") != (cfg.TLSKeyFile != "") {
		return nil, errors.New("web: --tls-cert and --tls-key must be set together")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	s := &Server{
		cfg:         cfg,
		auth:        newAuthenticator(cfg.Auth),
		ops:         cfg.Ops,
		kitDir:      cfg.KitDir,
		startedAt:   time.Now(),
		versionInfo: cfg.Version,
		logger:      logger,
		mux:         http.NewServeMux(),
	}
	s.registerRoutes()

	s.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.withLogging(s.mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return s, nil
}

// Handler exposes the configured HTTP handler so tests can drive it via
// httptest.NewServer / httptest.NewRecorder without standing up the
// listener.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// Serve starts the HTTP(S) server and blocks until ctx is cancelled or the
// listener errors. It performs a graceful shutdown on cancel.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.cfg.Addr, err)
	}
	scheme := "http"
	if s.cfg.TLSCertFile != "" {
		scheme = "https"
	}
	s.logger.Printf("skirk web: listening on %s://%s (kit=%s, auth=%v)",
		scheme, ln.Addr().String(), s.kitDir, !s.cfg.Auth.NoAuth)

	errCh := make(chan error, 1)
	go func() {
		if s.cfg.TLSCertFile != "" {
			errCh <- s.srv.ServeTLS(ln, s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		} else {
			errCh <- s.srv.Serve(ln)
		}
	}()

	select {
	case <-ctx.Done():
		s.logger.Printf("skirk web: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("web: shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// registerRoutes wires every URL pattern the API exposes.
func (s *Server) registerRoutes() {
	// Public endpoints (no auth required).
	s.mux.HandleFunc("/api/login", s.auth.handleLogin)
	s.mux.HandleFunc("/api/logout", s.auth.handleLogout)

	// Protected API endpoints.
	s.mux.HandleFunc("/api/status", s.auth.requireAuth(s.handleStatus))
	s.mux.HandleFunc("/api/client-profile", s.auth.requireAuth(s.handleClientProfile))
	s.mux.HandleFunc("/api/mailboxes", s.auth.requireAuth(s.handleMailboxesRoot))
	s.mux.HandleFunc("/api/mailboxes/", s.auth.requireAuth(s.handleMailboxesSub))
	s.mux.HandleFunc("/api/oauth/device", s.auth.requireAuth(s.handleOAuthDevice))
	s.mux.HandleFunc("/api/oauth/poll", s.auth.requireAuth(s.handleOAuthPoll))

	// Static UI. embed.FS roots include the "static" prefix; strip it so
	// "/" maps to "static/index.html".
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// fs.Sub on a real embed.FS only fails for programmer errors
		// (typo in the //go:embed path). Bail loudly.
		panic(fmt.Errorf("web: embed static subdir: %w", err))
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
}

// handleMailboxesRoot dispatches GET/POST on /api/mailboxes.
func (s *Server) handleMailboxesRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMailboxList(w, r)
	case http.MethodPost:
		s.handleMailboxAdd(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleMailboxesSub dispatches /api/mailboxes/{label}[/promote] requests.
// We use manual parsing instead of pulling in a router framework to keep the
// dependency tree at zero.
func (s *Server) handleMailboxesSub(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/mailboxes/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	// Split off optional /promote suffix.
	parts := strings.SplitN(rest, "/", 2)
	label := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodDelete:
			s.handleMailboxRemove(w, r, label)
		case http.MethodPatch:
			s.handleMailboxRename(w, r, label)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "promote":
		s.handleMailboxPromote(w, r, label)
	default:
		writeError(w, http.StatusNotFound, "unknown mailbox sub-resource")
	}
}

// withLogging is a tiny request logger so operators can see what the dashboard
// is doing. It deliberately omits the path query string (which may contain a
// session id on OAuth poll URLs) and any auth headers.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		lw := &loggingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		s.logger.Printf("skirk web: %s %s %d %s (%s)",
			r.Method, r.URL.Path, lw.status, time.Since(started).Round(time.Millisecond),
			clientIP(r),
		)
	})
}

type loggingWriter struct {
	http.ResponseWriter
	status int
}

func (l *loggingWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

// addrIsLoopback returns true when host:port refers to a loopback address.
// Empty host means listen-on-all and is NOT loopback.
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
