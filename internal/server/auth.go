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

// authorize checks that a request may act for hostname as a member of
// swarm/environment (from the API path). A protected Swarm + Environment
// needs credentials of exactly that one. A server which is reporting to a
// protected Swarm + Environment cannot be claimed from elsewhere until its
// reports there have gone stale, so a moved server waits a few check
// intervals before it can report in its new place.
func (s *Server) authorize(cfg *fleet.Config, r *http.Request, hostname, swarm, environment string, records map[string]record) error {
	cred, err := authenticate(cfg, r)
	if err != nil {
		return &authError{http.StatusUnauthorized, "invalid credentials"}
	}
	if cred != nil && (cred.Swarm != swarm || cred.Environment != environment) {
		return &authError{http.StatusForbidden, fmt.Sprintf("credentials are for %s/%s, not for %s/%s",
			cred.Swarm, cred.Environment, swarm, environment)}
	}
	if cred == nil && cfg.Protected(swarm, environment) {
		return &authError{http.StatusUnauthorized, fmt.Sprintf("Swarm %s environment %s requires authentication", swarm, environment)}
	}
	if rec, ok := records[hostname]; ok {
		oldSwarm, oldEnv := rec.Report.Swarm, rec.Report.Environment
		if (oldSwarm != swarm || oldEnv != environment) && cfg.Protected(oldSwarm, oldEnv) {
			d := cfg.Resolve(hostname, oldSwarm, oldEnv, s.Now())
			if s.Now().Sub(rec.LastSeen) <= staleAfter(d) {
				return &authError{http.StatusForbidden, fmt.Sprintf("%s reports to %s/%s; it can move once it has stopped reporting there for %s",
					hostname, oldSwarm, oldEnv, staleAfter(d))}
			}
		}
	}
	return nil
}
