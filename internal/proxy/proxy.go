// Package proxy serves the Anthropic and OpenAI APIs to the team and sends
// each request to the first pool upstream that can take it, failing over
// when one hits a rate limit, runs out of quota or goes down.
package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nizarmah/tokenpool/internal/config"
	"github.com/nizarmah/tokenpool/internal/pool"
	"github.com/nizarmah/tokenpool/internal/translate"
)

// Version is reported in the User-Agent sent upstream.
var Version = "dev"

// kind is the endpoint a client called.
type kind int

const (
	kindMessages    kind = iota // POST /v1/messages
	kindCountTokens             // POST /v1/messages/count_tokens
	kindResponses               // POST /v1/responses
)

// format is the API the client speaks on this endpoint.
func (k kind) format() config.Format {
	if k == kindResponses {
		return config.OpenAI
	}
	return config.Anthropic
}

// Server is the tokenpool HTTP server.
type Server struct {
	cfg    *config.Config
	pool   *pool.Pool
	client *http.Client
	log    *slog.Logger
	keys   []clientKey
}

type clientKey struct {
	name string
	key  []byte
}

// New builds a server for cfg that routes through p.
func New(cfg *config.Config, p *pool.Pool, log *slog.Logger) *Server {
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	transport.ResponseHeaderTimeout = cfg.HeaderTimeout
	transport.MaxIdleConnsPerHost = 32
	s := &Server{
		cfg:  cfg,
		pool: p,
		log:  log,
		client: &http.Client{
			Transport: transport,
			// Never follow redirects: they would carry the upstream token elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	for name, key := range cfg.ClientKeys {
		s.keys = append(s.keys, clientKey{name: name, key: []byte(key)})
	}
	return s
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /v1/messages", s.withClient(config.Anthropic, s.serve(kindMessages)))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.withClient(config.Anthropic, s.countTokens))
	mux.HandleFunc("POST /v1/responses", s.withClient(config.OpenAI, s.serve(kindResponses)))
	mux.HandleFunc("POST /responses", s.withClient(config.OpenAI, s.serve(kindResponses)))
	mux.HandleFunc("GET /v1/models", s.withClient(config.OpenAI, s.models))
	if s.cfg.AdminKey != "" {
		s.adminRoutes(mux)
	}
	return mux
}

type clientCtxKey struct{}

// withClient rejects requests without a valid client key.
func (s *Server) withClient(in config.Format, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, ok := s.authenticate(r)
		if !ok {
			writeError(w, in, http.StatusUnauthorized, "tokenpool: missing or invalid API key")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), clientCtxKey{}, name)))
	}
}

func (s *Server) authenticate(r *http.Request) (string, bool) {
	if key := presentedKey(r); key != "" {
		for _, k := range s.keys {
			if subtle.ConstantTimeCompare([]byte(key), k.key) == 1 {
				return k.name, true
			}
		}
	}
	if s.cfg.AllowAnonymous {
		return "anonymous", true
	}
	return "", false
}

// presentedKey reads x-api-key (Anthropic SDKs) or a bearer token (OpenAI
// SDKs, ANTHROPIC_AUTH_TOKEN).
func presentedKey(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func clientName(ctx context.Context) string {
	name, _ := ctx.Value(clientCtxKey{}).(string)
	return name
}

// requestHead is the part of a request body the proxy itself needs.
type requestHead struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) readRequest(w http.ResponseWriter, r *http.Request, in config.Format) ([]byte, requestHead, bool) {
	var head requestHead
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		status := http.StatusBadRequest
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, in, status, "tokenpool: could not read request body: "+err.Error())
		return nil, head, false
	}
	if err := json.Unmarshal(body, &head); err != nil {
		writeError(w, in, http.StatusBadRequest, "tokenpool: request body must be a JSON object")
		return nil, head, false
	}
	return body, head, true
}

// serve handles a completion request: try each candidate upstream in pool
// order until one answers.
func (s *Server) serve(k kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		in := k.format()
		body, head, ok := s.readRequest(w, r, in)
		if !ok {
			return
		}
		log := s.log.With("client", clientName(r.Context()), "api", in, "model", head.Model)
		sel := s.pool.Pick(head.Model)
		if len(sel.Candidates)+len(sel.Unavailable) == 0 {
			writeError(w, in, http.StatusNotFound,
				fmt.Sprintf("tokenpool: no upstream in the pool serves model %q", head.Model))
			return
		}

		failures := append([]string{}, sel.Unavailable...)
		attempted, allLimits := 0, true
		for _, c := range sel.Candidates {
			attempted++
			res, err := s.send(r, k, c, body)
			if err != nil {
				if r.Context().Err() != nil {
					log.Info("client went away", "upstream", c.Name)
					return
				}
				var bad badRequestError
				if errors.As(err, &bad) {
					writeError(w, in, http.StatusBadRequest, "tokenpool: "+bad.Error())
					return
				}
				if errors.Is(err, translate.ErrStateful) {
					log.Info("skipping", "upstream", c.Name, "reason", err.Error())
					failures = append(failures, c.Name+": "+err.Error())
					allLimits = false
					continue
				}
				reason := "unreachable: " + err.Error()
				var tokErr tokenError
				if errors.As(err, &tokErr) {
					reason = tokErr.Error()
				}
				s.pool.Fail(c.Upstream, 0, s.cfg.Cooldowns.Error, reason)
				log.Warn("failing over", "upstream", c.Name, "reason", reason, "cooldown", s.cfg.Cooldowns.Error.String())
				failures = append(failures, c.Name+": "+reason)
				allLimits = false
				continue
			}

			if res.StatusCode >= 200 && res.StatusCode < 300 {
				s.pool.Success(c.Upstream, res.StatusCode)
				err := s.writeSuccess(w, res, k, c, head)
				attrs := []any{"upstream", c.Name, "upstream_model", c.UpstreamModel,
					"fallback", c.Fallback, "status", res.StatusCode, "failovers", attempted - 1,
					"duration", time.Since(start).Round(time.Millisecond)}
				if err != nil {
					log.Warn("served with errors", append(attrs, "error", err)...)
				} else {
					log.Info("served", attrs...)
				}
				return
			}

			errBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			v := classify(res.StatusCode, res.Header, errBody, s.cfg.Cooldowns, time.Now())
			if res.StatusCode >= 300 && res.StatusCode < 400 {
				v = verdict{action: skip, reason: fmt.Sprintf("redirected (%d) to %q: fix the url",
					res.StatusCode, res.Header.Get("Location"))}
			}
			switch v.action {
			case relay:
				s.pool.Record(c.Upstream, res.StatusCode)
				log.Info("upstream rejected the request", "upstream", c.Name, "status", res.StatusCode,
					"duration", time.Since(start).Round(time.Millisecond))
				relayError(w, res, errBody, k, c)
				return
			case skip:
				s.pool.Record(c.Upstream, res.StatusCode)
			case bench:
				s.pool.Fail(c.Upstream, res.StatusCode, v.cooldown, v.reason)
			}
			log.Warn("failing over", "upstream", c.Name, "status", res.StatusCode,
				"reason", v.reason, "cooldown", v.cooldown.String())
			failures = append(failures, c.Name+": "+v.reason)
			allLimits = allLimits && v.limit
		}

		status := http.StatusServiceUnavailable
		if wait := s.pool.Wait(head.Model); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
			if allLimits {
				status = http.StatusTooManyRequests
			}
		}
		log.Warn("no upstream could serve", "status", status, "reasons", failures)
		writeError(w, in, status, "tokenpool: no upstream could serve this request: "+strings.Join(failures, "; "))
	}
}

// countTokens asks an Anthropic upstream to count, or estimates when none
// can. Count limits are separate from message limits, so nothing is benched.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	body, head, ok := s.readRequest(w, r, config.Anthropic)
	if !ok {
		return
	}
	log := s.log.With("client", clientName(r.Context()), "api", "count_tokens", "model", head.Model)
	for _, c := range s.pool.Pick(head.Model).Candidates {
		if c.Format != config.Anthropic {
			continue
		}
		res, err := s.send(r, kindCountTokens, c, body)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			log.Warn("count failed, trying the next upstream", "upstream", c.Name, "error", err)
			continue
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			_ = s.writeSuccess(w, res, kindCountTokens, c, head)
			return
		}
		errBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		v := classify(res.StatusCode, res.Header, errBody, s.cfg.Cooldowns, time.Now())
		if v.action == relay {
			relayError(w, res, errBody, kindCountTokens, c)
			return
		}
		log.Warn("count failed, trying the next upstream", "upstream", c.Name, "status", res.StatusCode, "reason", v.reason)
	}
	log.Info("no upstream could count; estimating")
	// About four bytes of JSON per token: rough, but enough for budgeting.
	w.Header().Set("X-Tokenpool-Estimated", "true")
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": max(len(body)/4, 1)})
}

// tokenError means the upstream's token_file couldn't be read.
type tokenError struct{ err error }

func (e tokenError) Error() string { return e.err.Error() }
func (e tokenError) Unwrap() error { return e.err }

type badRequestError struct{ err error }

func (e badRequestError) Error() string { return e.err.Error() }
func (e badRequestError) Unwrap() error { return e.err }

// send makes one attempt against one upstream.
func (s *Server) send(r *http.Request, k kind, c pool.Candidate, body []byte) (*http.Response, error) {
	upBody, err := s.upstreamBody(k, c, body)
	if errors.Is(err, translate.ErrStateful) {
		return nil, err
	}
	if err != nil {
		return nil, badRequestError{err}
	}
	cred, err := c.Credential()
	if err != nil {
		return nil, tokenError{err}
	}
	query := ""
	if k.format() == c.Format {
		query = r.URL.RawQuery // e.g. Claude Code's ?beta=true
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		upstreamURL(c.Upstream.Upstream, k, query), bytes.NewReader(upBody))
	if err != nil {
		return nil, err
	}
	setUpstreamHeaders(req.Header, r.Header, c, k, cred)
	return s.client.Do(req)
}

func (s *Server) upstreamBody(k kind, c pool.Candidate, body []byte) ([]byte, error) {
	var err error
	switch in := k.format(); {
	case in == config.Anthropic && c.Format == config.OpenAI:
		body, err = translate.AnthropicToResponses(body)
	case in == config.OpenAI && c.Format == config.Anthropic:
		body, err = translate.ResponsesToAnthropic(body, s.cfg.DefaultMaxTokens)
	}
	if err != nil {
		return nil, err
	}
	return translate.Rewrite(body, c.UpstreamModel, c.MaxTokens)
}

// upstreamURL builds the endpoint URL. Anthropic bases follow the Anthropic
// SDK (no /v1), OpenAI bases follow the OpenAI SDK (with /v1); a URL that
// already ends in the endpoint path is used as is.
func upstreamURL(u config.Upstream, k kind, query string) string {
	base := strings.TrimRight(u.URL, "/")
	var path string
	if u.Format == config.Anthropic {
		base = strings.TrimSuffix(base, "/v1/messages")
		base = strings.TrimSuffix(base, "/v1")
		path = "/v1/messages"
		if k == kindCountTokens {
			path += "/count_tokens"
		}
	} else {
		base = strings.TrimSuffix(base, "/responses")
		if parsed, err := url.Parse(base); err == nil && parsed.Path == "" {
			base += "/v1"
		}
		path = "/responses"
	}
	if query != "" {
		path += "?" + query
	}
	return base + path
}

// GrokClientVersion is the Grok CLI version sent with a Grok login session.
// The CLI chat proxy answers 426 ("Your Grok CLI version (none) is
// outdated") without one; an upstream's headers can override it when the
// proxy raises its floor.
var GrokClientVersion = "1.0.46"

func setUpstreamHeaders(h, in http.Header, c pool.Candidate, k kind, cred pool.Credential) {
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "tokenpool/"+Version)
	auth := c.AuthFor(cred.Token)
	if c.Auth == "" && c.Format == config.OpenAI && cred.GrokSession {
		auth = "grok"
	}
	if c.Format == config.Anthropic {
		version := "2023-06-01"
		if k.format() == config.Anthropic {
			if v := in.Get("Anthropic-Version"); v != "" {
				version = v
			}
			for _, beta := range in.Values("Anthropic-Beta") {
				h.Add("Anthropic-Beta", beta)
			}
		}
		h.Set("Anthropic-Version", version)
	}
	switch {
	case auth == "bearer" || auth == "setup-token" || auth == "grok":
		h.Set("Authorization", "Bearer "+cred.Token)
	case auth == "x-api-key":
		h.Set("X-Api-Key", cred.Token)
	case strings.HasPrefix(auth, "header:"):
		h.Set(strings.TrimPrefix(auth, "header:"), cred.Token)
	}
	if auth == "setup-token" {
		// The caller is Claude Code. Keep the identity it actually sent
		// and attach the OAuth beta the setup-token requires.
		forwardClientIdentity(h, in)
		mergeBeta(h, config.OAuthBeta())
	}
	if auth == "grok" {
		// The CLI chat proxy validates a login session with this header,
		// and routes on x-grok-model-override rather than the body model.
		h.Set("X-XAI-Token-Auth", "xai-grok-cli")
		h.Set("X-Grok-Client-Version", GrokClientVersion)
		if c.UpstreamModel != "" {
			h.Set("X-Grok-Model-Override", c.UpstreamModel)
		}
	}
	for name, value := range c.Headers {
		h.Set(name, value)
	}
	if auth == "setup-token" {
		mergeBeta(h, config.OAuthBeta())
	}
}

// forwardClientIdentity copies the Claude Code identity headers the client
// sent. It does not invent any. Authorization and x-api-key stay out: those
// carry the caller's tokenpool key.
func forwardClientIdentity(h, in http.Header) {
	if ua := in.Get("User-Agent"); ua != "" {
		h.Set("User-Agent", ua)
	}
	for _, name := range []string{"X-App", "Anthropic-Dangerous-Direct-Browser-Access"} {
		if v := in.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	for name, values := range in {
		if strings.HasPrefix(strings.ToLower(name), "x-stainless-") && len(values) > 0 {
			h.Set(name, values[0])
		}
	}
}

// mergeBeta adds beta to anthropic-beta if it is not already listed.
func mergeBeta(h http.Header, beta string) {
	var parts []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		parts = append(parts, s)
	}
	for _, v := range h.Values("Anthropic-Beta") {
		for _, p := range strings.Split(v, ",") {
			add(p)
		}
	}
	add(beta)
	h.Del("Anthropic-Beta")
	if len(parts) > 0 {
		h.Set("Anthropic-Beta", strings.Join(parts, ","))
	}
}

// writeSuccess relays a 2xx answer, translating it when the upstream
// speaks a different API than the client.
func (s *Server) writeSuccess(w http.ResponseWriter, res *http.Response, k kind, c pool.Candidate, head requestHead) error {
	defer res.Body.Close()
	in := k.format()
	w.Header().Set("X-Tokenpool-Upstream", c.Name)
	if in == c.Format {
		copyHeaders(w.Header(), res.Header)
		w.WriteHeader(res.StatusCode)
		return copyBody(w, res.Body)
	}
	if head.Stream {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if in == config.Anthropic {
			return translate.ResponsesStreamToAnthropic(w, flusher(w), res.Body, c.UpstreamModel)
		}
		return translate.AnthropicStreamToResponses(w, flusher(w), res.Body)
	}
	upstream, err := io.ReadAll(res.Body)
	if err != nil {
		writeError(w, in, http.StatusBadGateway, fmt.Sprintf("tokenpool: reading %s: %v", c.Name, err))
		return err
	}
	convert := translate.AnthropicResponseToResponses
	if in == config.Anthropic {
		convert = translate.ResponsesResponseToAnthropic
	}
	out, err := convert(upstream)
	if err != nil {
		writeError(w, in, http.StatusBadGateway, fmt.Sprintf("tokenpool: translating %s: %v", c.Name, err))
		return err
	}
	writeJSON(w, res.StatusCode, json.RawMessage(out))
	return nil
}

// relayError passes an upstream's rejection to the client in its API shape.
func relayError(w http.ResponseWriter, res *http.Response, body []byte, k kind, c pool.Candidate) {
	w.Header().Set("X-Tokenpool-Upstream", c.Name)
	if k.format() == c.Format {
		copyHeaders(w.Header(), res.Header)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(body)
		return
	}
	writeError(w, k.format(), res.StatusCode, c.Name+": "+translate.ErrorMessage(body))
}

var skipHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Set-Cookie": true,
}

func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		if !skipHeaders[name] {
			dst[name] = append([]string(nil), values...)
		}
	}
}

// copyBody streams the body, flushing as it goes so SSE arrives live.
func copyBody(w http.ResponseWriter, r io.Reader) error {
	flush := flusher(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			flush()
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func flusher(w http.ResponseWriter) func() {
	rc := http.NewResponseController(w)
	return func() { _ = rc.Flush() }
}

func writeError(w http.ResponseWriter, in config.Format, status int, msg string) {
	body := translate.AnthropicError(status, msg)
	if in == config.OpenAI {
		body = translate.OpenAIError(status, msg)
	}
	writeJSON(w, status, json.RawMessage(body))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":"encoding response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// models lists the model names the pool config mentions, in a shape both
// the OpenAI and Anthropic SDKs accept.
func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	owner := map[string]string{}
	for _, u := range s.pool.List() {
		for pattern, target := range u.Models {
			for _, name := range []string{pattern, target} {
				if name != "" && !strings.Contains(name, "*") && owner[name] == "" {
					owner[name] = u.Name
				}
			}
		}
		if u.Model != "" && owner[u.Model] == "" {
			owner[u.Model] = u.Name
		}
	}
	names := make([]string, 0, len(owner))
	for name := range owner {
		names = append(names, name)
	}
	sort.Strings(names)
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{
			"id": name, "object": "model", "type": "model", "display_name": name,
			"owned_by": owner[name], "created": 0, "created_at": "1970-01-01T00:00:00Z",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
}
