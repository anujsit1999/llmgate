// Package server implements llmgate's HTTP API and middleware.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anujsit1999/llmgate/auth"
	"github.com/anujsit1999/llmgate/config"
	"github.com/anujsit1999/llmgate/internal/metrics"
	"github.com/anujsit1999/llmgate/provider"
	"github.com/anujsit1999/llmgate/ratelimit"
	"github.com/anujsit1999/llmgate/redact"
	"github.com/anujsit1999/llmgate/router"
)

// Server wires the gateway together.
type Server struct {
	cfg      config.Server
	auth     *auth.Authenticator
	limiter  *ratelimit.Limiter
	redactor *redact.Redactor // nil when redaction is disabled
	router   *router.Router
	metrics  *metrics.Metrics
	log      *slog.Logger
	usageLog bool
}

// Deps are the collaborators a Server needs.
type Deps struct {
	Config   config.Server
	Auth     *auth.Authenticator // nil disables authentication
	Limiter  *ratelimit.Limiter  // nil disables rate limiting
	Redactor *redact.Redactor
	Router   *router.Router
	Metrics  *metrics.Metrics
	Logger   *slog.Logger
	UsageLog bool
}

// New builds a Server.
func New(d Deps) *Server {
	return &Server{cfg: d.Config, auth: d.Auth, limiter: d.Limiter, redactor: d.Redactor,
		router: d.Router, metrics: d.Metrics, log: d.Logger, usageLog: d.UsageLog}
}

// Handler returns the public API handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /v1/models", s.authenticated(s.handleModels))
	mux.Handle("POST /v1/chat/completions", s.authenticated(s.handleChat))
	return s.requestID(s.recoverer(securityHeaders(mux)))
}

// ---- middleware ----

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxPrincipal
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._\-]{1,64}$`)

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID.MatchString(id) { // never trust/echo arbitrary client input
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "request_id", reqID(r), "panic", fmt.Sprint(v))
				writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.Anonymous()
		if s.auth != nil {
			var err error
			if p, err = s.auth.Authenticate(r); err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="llmgate"`)
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", err.Error())
				return
			}
		}
		if s.limiter != nil {
			ok, wait, err := s.limiter.Allow(r.Context(), p.KeyID, p.RPM)
			if err != nil {
				s.log.Warn("rate-limit store unavailable; allowing request", "request_id", reqID(r), "error", err.Error())
			} else if !ok {
				s.metrics.RateLimited.WithLabelValues(p.Tenant).Inc()
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limited", "rate limit exceeded")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
	})
}

func reqID(r *http.Request) string { id, _ := r.Context().Value(ctxRequestID).(string); return id }

func principal(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(ctxPrincipal).(auth.Principal)
	return p
}

// ---- handlers ----

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := []model{}
	for _, m := range s.router.Models() {
		if p.CanUse(m) {
			data = append(data, model{ID: m, Object: "model", OwnedBy: "llmgate"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

var validEfforts = map[string]bool{"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p := principal(r)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body too large")
		return
	}
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "invalid JSON body: "+err.Error())
		return
	}
	extra, apiErr := extraParams(body)
	if apiErr != nil {
		writeError(w, http.StatusBadRequest, apiErr.Type, apiErr.Code, apiErr.Message)
		return
	}

	preq, redactions, apiErr := s.translate(req)
	if apiErr != nil {
		writeError(w, http.StatusBadRequest, apiErr.Type, apiErr.Code, apiErr.Message)
		return
	}
	if !s.router.Has(req.Model) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model not found: "+req.Model)
		return
	}
	if !p.CanUse(req.Model) {
		writeError(w, http.StatusForbidden, "permission_error", "model_not_allowed", "this key may not use model "+req.Model)
		return
	}
	preq.Extra = extra
	total := 0
	for t, n := range redactions {
		s.metrics.Redactions.WithLabelValues(string(t)).Add(float64(n))
		total += n
	}
	w.Header().Set("X-LLMGate-Redactions", strconv.Itoa(total))

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	if req.Stream {
		s.streamChat(ctx, w, r, req.Model, preq, start, total)
		return
	}

	resp, target, attempts, err := s.router.Complete(ctx, req.Model, preq)
	s.observe(r, req.Model, target, attempts, usageOf(resp), err, start, total, false)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ChatResponse{
		ID:      "chatcmpl-" + reqID(r),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant", Content: Content(resp.Content)},
			FinishReason: resp.FinishReason,
		}},
		Usage: toUsage(resp.Usage),
	})
}

func (s *Server) streamChat(ctx context.Context, w http.ResponseWriter, r *http.Request, model string, preq provider.Request, start time.Time, redactions int) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.RequestTimeout + 5*time.Second))

	id := "chatcmpl-" + reqID(r)
	created := time.Now().Unix()
	headerSent := false
	var usage provider.Usage

	send := func(v any) error {
		if !headerSent {
			h := w.Header()
			h.Set("Content-Type", "text/event-stream")
			h.Set("Connection", "keep-alive")
			h.Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			headerSent = true
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	}

	first := true
	target, attempts, err := s.router.Stream(ctx, model, preq, func(c provider.Chunk) error {
		chunk := ChatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ChunkChoice{{Delta: Delta{Content: c.Delta}}}}
		if first {
			chunk.Choices[0].Delta.Role = "assistant"
			first = false
		}
		if c.FinishReason != "" {
			fr := c.FinishReason
			chunk.Choices[0].FinishReason = &fr
		}
		if c.Usage != nil {
			usage = *c.Usage
			u := toUsage(usage)
			chunk.Usage = &u
		}
		return send(chunk)
	})
	s.observe(r, model, target, attempts, &usage, err, start, redactions, true)

	if err != nil {
		if !headerSent {
			s.writeUpstreamError(w, err)
			return
		}
		// Mid-stream failure: emit an error event the client can detect.
		_ = send(ErrorBody{Error: APIError{Message: "upstream stream interrupted", Type: "upstream_error"}})
	}
	if headerSent {
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		_ = rc.Flush()
	}
}

// knownFields are the request fields the gateway understands itself; every
// other top-level field is treated as a model-specific extra parameter.
var knownFields = map[string]bool{
	"model": true, "messages": true, "max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "reasoning_effort": true, "stream": true, "stream_options": true,
}

const maxExtraParams = 32

// extraParams collects non-standard top-level fields for pass-through.
func extraParams(body []byte) (map[string]any, *APIError) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, &APIError{Type: "invalid_request_error", Code: "invalid_json", Message: "body must be a JSON object"}
	}
	extra := map[string]any{}
	for k, raw := range all {
		if knownFields[k] {
			continue
		}
		if err := config.CheckParamKey(k); err != nil {
			return nil, &APIError{Type: "invalid_request_error", Code: "invalid_parameter", Message: err.Error()}
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, &APIError{Type: "invalid_request_error", Code: "invalid_parameter", Message: "invalid value for " + k}
		}
		extra[k] = v
	}
	if len(extra) > maxExtraParams {
		return nil, &APIError{Type: "invalid_request_error", Code: "too_many_parameters",
			Message: fmt.Sprintf("at most %d extra parameters are allowed", maxExtraParams)}
	}
	return extra, nil
}

// translate validates an OpenAI request and converts it to a provider request,
// applying redaction to message content.
func (s *Server) translate(req ChatRequest) (provider.Request, map[redact.Type]int, *APIError) {
	bad := func(code, msg string) (provider.Request, map[redact.Type]int, *APIError) {
		return provider.Request{}, nil, &APIError{Type: "invalid_request_error", Code: code, Message: msg}
	}
	if req.Model == "" {
		return bad("missing_model", "model is required")
	}
	if len(req.Messages) == 0 {
		return bad("missing_messages", "messages must not be empty")
	}
	if len(req.Messages) > s.cfg.MaxMessages {
		return bad("too_many_messages", fmt.Sprintf("at most %d messages are allowed", s.cfg.MaxMessages))
	}
	if !validEfforts[req.ReasoningEffort] {
		return bad("invalid_reasoning_effort", "reasoning_effort must be one of low, medium, high, xhigh, max")
	}

	maxTokens := s.cfg.DefaultMaxTokens
	for _, v := range []*int{req.MaxCompletionTokens, req.MaxTokens} {
		if v != nil {
			maxTokens = *v
			break
		}
	}
	if maxTokens < 1 || maxTokens > s.cfg.MaxTokensCap {
		return bad("invalid_max_tokens", fmt.Sprintf("max_tokens must be between 1 and %d", s.cfg.MaxTokensCap))
	}

	counts := map[redact.Type]int{}
	scrub := func(t string) string {
		if s.redactor == nil {
			return t
		}
		return s.redactor.Redact(t, counts)
	}

	out := provider.Request{MaxTokens: maxTokens, Temperature: req.Temperature, Effort: req.ReasoningEffort}
	var system []string
	for _, m := range req.Messages {
		text := scrub(string(m.Content))
		switch m.Role {
		case "system", "developer":
			system = append(system, text)
		case "user", "assistant":
			out.Messages = append(out.Messages, provider.Message{Role: m.Role, Content: text})
		default:
			return bad("unsupported_role", "unsupported message role: "+m.Role)
		}
	}
	if len(out.Messages) == 0 || out.Messages[0].Role != "user" {
		return bad("invalid_messages", "the first non-system message must have role user")
	}
	out.System = strings.Join(system, "\n\n")
	return out, counts, nil
}

// observe records metrics and a structured usage log line. Prompt and
// completion text are deliberately never logged.
func (s *Server) observe(r *http.Request, model string, target router.Target, attempts []router.Attempt,
	usage *provider.Usage, err error, start time.Time, redactions int, stream bool) {
	p := principal(r)
	providerName, upstream := "", ""
	if target.Provider != nil {
		providerName, upstream = target.Provider.Name(), target.Model
	}
	status := "ok"
	switch {
	case errors.Is(err, context.Canceled):
		status = "canceled" // the client went away; not an upstream failure
	case err != nil:
		status = "error"
	}
	called, skipped := 0, 0
	for _, a := range attempts {
		if !a.Called() {
			skipped++
			s.metrics.Skipped.WithLabelValues(model, a.Target.Provider.Name()+"/"+a.Target.Model, a.Skipped).Inc()
			continue
		}
		called++
		s.metrics.Latency.WithLabelValues(model, a.Target.Provider.Name()).Observe(a.Duration.Seconds())
	}
	if called > 1 {
		s.metrics.Fallbacks.WithLabelValues(model).Add(float64(called - 1))
	}
	s.metrics.Requests.WithLabelValues(model, providerName, status).Inc()

	var in, out int
	var cost float64
	if usage != nil {
		in, out = usage.InputTokens, usage.OutputTokens
		cost = target.Pricing.Cost(in, out)
		s.metrics.Tokens.WithLabelValues(p.Tenant, model, "input").Add(float64(in))
		s.metrics.Tokens.WithLabelValues(p.Tenant, model, "output").Add(float64(out))
		s.metrics.CostUSD.WithLabelValues(p.Tenant, model).Add(cost)
	}

	if !s.usageLog {
		return
	}
	attrs := []any{
		"request_id", reqID(r), "tenant", p.Tenant, "key_id", p.KeyID,
		"model", model, "provider", providerName, "upstream_model", upstream,
		"stream", stream, "status", status, "attempts", called, "skipped", skipped,
		"latency_ms", time.Since(start).Milliseconds(),
		"input_tokens", in, "output_tokens", out, "cost_usd", cost, "redactions", redactions,
	}
	if status == "error" {
		s.log.Warn("chat", append(attrs, "error", err.Error())...)
		return
	}
	s.log.Info("chat", attrs...)
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, router.ErrNoHealthyUpstream):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "upstream_error", "no_healthy_upstream",
			"no healthy upstream is available for this model; retry shortly")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "upstream_error", "timeout", "upstream request timed out")
	case errors.Is(err, context.Canceled):
		// Client went away; nothing useful to send.
	default:
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Status == http.StatusTooManyRequests {
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "upstream_rate_limited", "upstream provider is rate limiting")
			return
		}
		if errors.As(err, &pe) && pe.Status == http.StatusBadRequest {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "upstream_rejected",
				"upstream provider rejected the request (check model-specific parameters)")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed", "all upstream targets failed")
	}
}

func usageOf(r *provider.Response) *provider.Usage {
	if r == nil {
		return nil
	}
	return &r.Usage
}

func toUsage(u provider.Usage) Usage {
	return Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.InputTokens + u.OutputTokens}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: APIError{Message: msg, Type: typ, Code: code}})
}
