package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/nizarmah/tokenpool/internal/config"
	"github.com/nizarmah/tokenpool/internal/pool"
)

// adminRoutes manage the pool at runtime. Upstreams added here are saved
// to pool_file; upstreams from the config file can only be paused or reset.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/upstreams", s.withAdmin(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"upstreams": s.pool.List()})
	}))
	mux.HandleFunc("POST /admin/upstreams", s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		var u config.Upstream
		if !decodeUpstream(w, r, &u) {
			return
		}
		st, err := s.pool.Add(u)
		s.adminReply(w, r, http.StatusCreated, st, err, "upstream added")
	}))
	mux.HandleFunc("GET /admin/upstreams/{name}", s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		st, err := s.pool.Get(r.PathValue("name"))
		s.adminReply(w, r, http.StatusOK, st, err, "")
	}))
	mux.HandleFunc("PUT /admin/upstreams/{name}", s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		var u config.Upstream
		if !decodeUpstream(w, r, &u) {
			return
		}
		st, err := s.pool.Update(r.PathValue("name"), u)
		s.adminReply(w, r, http.StatusOK, st, err, "upstream updated")
	}))
	mux.HandleFunc("DELETE /admin/upstreams/{name}", s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Remove(r.PathValue("name")); err != nil {
			adminError(w, err)
			return
		}
		s.log.Info("upstream removed", "upstream", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /admin/upstreams/{name}/{op}", s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var st pool.Status
		var err error
		switch op := r.PathValue("op"); op {
		case "enable":
			st, err = s.pool.SetDisabled(name, false)
		case "disable":
			st, err = s.pool.SetDisabled(name, true)
		case "reset":
			st, err = s.pool.Reset(name)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown action " + op + ": use enable, disable or reset"})
			return
		}
		s.adminReply(w, r, http.StatusOK, st, err, "upstream "+r.PathValue("op"))
	}))
}

func (s *Server) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(presentedKey(r)), []byte(s.cfg.AdminKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid admin key"})
			return
		}
		next(w, r)
	}
}

func decodeUpstream(w http.ResponseWriter, r *http.Request, u *config.Upstream) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(u); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid upstream JSON: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) adminReply(w http.ResponseWriter, r *http.Request, status int, st pool.Status, err error, event string) {
	if err != nil {
		adminError(w, err)
		return
	}
	if event != "" {
		s.log.Info(event, "upstream", st.Name, "by", "admin", "remote", r.RemoteAddr)
	}
	writeJSON(w, status, st)
}

func adminError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, pool.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, pool.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, pool.ErrExists), errors.Is(err, pool.ErrReadOnly):
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
