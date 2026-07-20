// Package api exposes a small HTTP admin API for third-party systems to
// manage anytls users and query their usage.
package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"anytls/user"

	"github.com/sirupsen/logrus"
)

type Server struct {
	manager    *user.Manager
	apiKey     string
	serverAddr string
	root       *http.ServeMux
}

// NewServer builds the admin HTTP server: a small JSON API under /api/,
// protected by a static API key, and an unauthenticated static admin web UI
// at / that talks to the API from the browser using a key entered by hand.
// serverAddr is the anytls protocol listen address (the `-l` flag), exposed
// read-only via /api/server so the web UI can build per-user connection
// links/QR codes without the admin having to type the port in by hand.
func NewServer(manager *user.Manager, apiKey string, serverAddr string) *Server {
	s := &Server{manager: manager, apiKey: apiKey, serverAddr: serverAddr}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/users", s.handleUsers)
	apiMux.HandleFunc("/api/users/", s.handleUserByID)
	apiMux.HandleFunc("/api/server", s.handleServerInfo)

	s.root = http.NewServeMux()
	s.root.Handle("/api/", s.authMiddleware(apiMux))
	s.root.Handle("/", http.FileServer(http.FS(webFS())))
	return s
}

func (s *Server) ListenAndServe(addr string) error {
	logrus.Infoln("[API] Listening", addr)
	return http.ListenAndServe(addr, s.root)
}

// ListenAndServeTLS serves the admin API/web UI over HTTPS using the same
// certificate/key the anytls protocol listener uses, so a real certificate
// obtained for the server (e.g. via install.sh --domain) also covers the
// admin page instead of it being stuck on plain HTTP.
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) error {
	logrus.Infoln("[API] Listening (TLS)", addr)
	return http.ListenAndServeTLS(addr, certFile, keyFile, s.root)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(s.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
