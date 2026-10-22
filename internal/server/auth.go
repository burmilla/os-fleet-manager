package server

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/burmilla/os-fleet-manager/internal/fleet"
	"golang.org/x/crypto/bcrypt"
)

// verified caches successful bcrypt checks so that clients reporting every
// few minutes do not cost a bcrypt round each time. The key includes the
// hash, so rotating a password in fleet.yml invalidates it.
var verified sync.Map

var errUnauthorized = errors.New("authentication required")

// authenticate checks HTTP Basic credentials against the htpasswd entries
// of the configuration. It returns nil when no credentials were sent.
func authenticate(cfg *fleet.Config, r *http.Request) (*fleet.Credential, error) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return nil, nil
	}
	cred, found := cfg.Credentials()[user]
	if !found {
		return nil, errUnauthorized
	}
	key := sha256.Sum256([]byte(user + "\x00" + pass + "\x00" + cred.Hash))
	if _, ok := verified.Load(key); ok {
		return &cred, nil
	}
	if bcrypt.CompareHashAndPassword([]byte(cred.Hash), []byte(pass)) != nil {
		return nil, errUnauthorized
	}
	verified.Store(key, true)
	return &cred, nil
}

type authError struct {
	code int
	msg  string
}

func (e *authError) Error() string { return e.msg }

func writeAuthError(w http.ResponseWriter, err error) {
	var ae *authError
	if !errors.As(err, &ae) {
		ae = &authError{http.StatusUnauthorized, err.Error()}
	}
	if ae.code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="BurmillaOS Fleet Manager"`)
	}
	http.Error(w, ae.msg, ae.code)
}

// authorize decides which Swarm + Environment a request for hostname acts
// in. Credentials bind the request to their Swarm + Environment. A server
// which belongs to a protected Swarm + Environment, by the servers: section
// or by its earlier reports, must use credentials of exactly that one.
func (s *Server) authorize(cfg *fleet.Config, r *http.Request, hostname, swarm, environment string, records map[string]record) (string, string, error) {
	cred, err := authenticate(cfg, r)
	if err != nil {
		return "", "", &authError{http.StatusUnauthorized, "invalid credentials"}
	}

	// Where the server belongs regardless of what this request claims.
	var boundSwarm, boundEnv, source string
	if srv, ok := cfg.Servers[hostname]; ok && (srv.Swarm != "" || srv.Environment != "") {
		boundSwarm, boundEnv, source = srv.Swarm, srv.Environment, "the servers: section of the fleet configuration"
	} else if rec, ok := records[hostname]; ok && cfg.Protected(rec.Report.Swarm, rec.Report.Environment) {
		boundSwarm, boundEnv, source = rec.Report.Swarm, rec.Report.Environment, "its earlier reports (add it to servers: to move it)"
	}

	if cred != nil {
		if source != "" && (boundSwarm != cred.Swarm || boundEnv != cred.Environment) {
			return "", "", &authError{http.StatusForbidden, fmt.Sprintf("%s belongs to %s/%s by %s, not to %s/%s",
				hostname, boundSwarm, boundEnv, source, cred.Swarm, cred.Environment)}
		}
		return cred.Swarm, cred.Environment, nil
	}

	if source != "" {
		swarm, environment = boundSwarm, boundEnv
	}
	if cfg.Protected(swarm, environment) {
		return "", "", &authError{http.StatusUnauthorized, fmt.Sprintf("Swarm %s environment %s requires authentication", swarm, environment)}
	}
	return swarm, environment, nil
}
