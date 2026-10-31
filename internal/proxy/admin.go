package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

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
		case "enable", "resume":
			st, err = s.pool.SetDisabled(name, false)
		case "disable", "pause":
			st, err = s.pool.SetDisabled(name, true)
		case "reset":
			st, err = s.pool.Reset(name)
		case "default":
			if err = s.pool.SetDefault(name); err == nil {
				st, err = s.pool.Get(name)
			}
		case "priority":
			var body struct {
				Priority *int `json:"priority"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			if body.Priority == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"priority": N}; lower is tried first`})
				return
			}
			st, err = s.pool.SetPriority(name, *body.Priority)
		case "min_client_priority":
			var body struct {
				MinClientPriority *config.ClientPriority `json:"min_client_priority"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			if body.MinClientPriority == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": `send {"min_client_priority": "low" | "normal" | "high"}`})
				return
			}
			st, err = s.pool.SetMinClientPriority(name, config.NormalizePriority(*body.MinClientPriority))
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "unknown action " + op + ": use pause, resume, reset, default, priority or min_client_priority"})
			return
		}
		s.adminReply(w, r, http.StatusOK, st, err, "upstream "+r.PathValue("op"))
	}))
	mux.HandleFunc("GET /admin/status", s.withAdmin(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.status())
	}))
	setStrategy := s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Strategy string `json:"strategy"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if err := s.pool.SetStrategy(body.Strategy); err != nil {
			adminError(w, err)
			return
		}
		s.log.Info("strategy changed", "strategy", body.Strategy, "by", "admin", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, s.status())
	})
	mux.HandleFunc("PUT /admin/strategy", setStrategy)
	mux.HandleFunc("POST /admin/strategy", setStrategy)
	mux.HandleFunc("DELETE /admin/default", s.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.SetDefault(""); err != nil {
			adminError(w, err)
			return
		}
		s.log.Info("default upstream cleared", "by", "admin", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, s.status())
	}))
}

// statusView is GET /admin/status: how the pool routes right now and how
// much quota each upstream has left, in the order requests try them.
type statusView struct {
	pool.Summary
	PinModels    bool              `json:"pin_models"`
	ModelAliases map[string]string `json:"model_aliases,omitempty"`
	// Callers lists each caller's name and priority, never its key.
	Callers []callerView `json:"callers"`
}

type callerView struct {
	Name     string                `json:"name"`
	Priority config.ClientPriority `json:"priority"`
	// Upstreams are the upstreams the caller tries first (client_upstreams).
	Upstreams []string `json:"upstreams,omitempty"`
}

func (s *Server) status() statusView {
	callers := make([]callerView, 0, len(s.cfg.ClientKeys))
	for name := range s.cfg.ClientKeys {
		callers = append(callers, callerView{Name: name, Priority: s.cfg.PriorityOf(name), Upstreams: s.cfg.UpstreamsFor(name)})
	}
	sort.Slice(callers, func(i, j int) bool { return callers[i].Name < callers[j].Name })
	return statusView{Summary: s.pool.Summarize(), PinModels: s.cfg.PinModels,
		ModelAliases: s.cfg.ModelAliases, Callers: callers}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return false
	}
	return true
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
