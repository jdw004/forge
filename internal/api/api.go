// Package api exposes Forge's queue through a Chi HTTP router.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"forge/internal/model"
	"forge/internal/queue"

	"github.com/go-chi/chi/v5"
)

const MaxBodyBytes = 4 << 20

type Config struct {
	Token          string
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

type server struct {
	store queue.Store
	cfg   Config
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// WorkflowSummary keeps list responses small even for 5,000-task workflows.
type WorkflowSummary struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	State     model.State         `json:"state"`
	CreatedAt time.Time           `json:"created_at"`
	TaskCount int                 `json:"task_count"`
	Counts    map[model.State]int `json:"counts"`
}

// New returns a router without binding a port. An empty token is intended for
// loopback demo mode; the application must reject unsafe remote configurations.
func New(store queue.Store, cfg Config) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("API requires a store")
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.RequestTimeout < 0 {
		return nil, errors.New("API timeout must be positive")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &server{store: store, cfg: cfg}
	r := chi.NewRouter()
	r.Use(s.requestContext)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.ready)
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.authenticate)
		r.Get("/workflows", s.list)
		r.Post("/workflows", s.create)
		r.Get("/workflows/{id}", s.get)
		r.Post("/workflows/{id}/cancel", s.cancel)
		r.Get("/nodes", s.nodes)
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Route not found")
	})
	return r, nil
}

func (s *server) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", model.ID())
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				s.cfg.Logger.Error("HTTP handler panic", "request_id", w.Header().Get("X-Request-ID"), "panic", recovered)
				writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error")
			}
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *server) authenticate(next http.Handler) http.Handler {
	// Fixed-length hashes make the token comparison independent of token length.
	expected := sha256.Sum256([]byte(s.cfg.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			actual := sha256.Sum256([]byte(token))
			if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="forge"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer token is required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Clients may disconnect while reading. No further response can be written
	// after headers are sent; values here contain only JSON-encodable model data.
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	body.RequestID = w.Header().Get("X-Request-ID")
	writeJSON(w, status, body)
}

func (s *server) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Workflow not found")
	case errors.Is(err, model.ErrConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "Idempotency key was used for a different workflow")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "request_timeout", "Request deadline exceeded")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "request_cancelled", "Request cancelled")
	default:
		s.cfg.Logger.Error("queue operation failed", "request_id", w.Header().Get("X-Request-ID"), "error", err)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Task store is unavailable")
	}
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "Task store is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be at most 200 bytes")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var spec model.Spec
	if err := decoder.Decode(&spec); err != nil {
		decodeError(w, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		decodeError(w, err)
		return
	}
	if err := model.Validate(spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_workflow", err.Error())
		return
	}
	workflow, fresh, err := s.store.Create(r.Context(), spec, key)
	if err != nil {
		s.storeError(w, err)
		return
	}
	status := http.StatusOK
	if fresh {
		status = http.StatusCreated
	}
	w.Header().Set("Location", "/v1/workflows/"+workflow.ID)
	writeJSON(w, status, workflow)
}

func decodeError(w http.ResponseWriter, err error) {
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds 4 MiB")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_json", "Body must contain one JSON object with supported fields")
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if values, ok := r.URL.Query()["limit"]; ok {
		var err error
		if len(values) == 1 {
			limit, err = strconv.Atoi(values[0])
		}
		if len(values) != 1 || err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 to 100")
			return
		}
	}
	workflows, err := s.store.List(r.Context(), limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	summaries := make([]WorkflowSummary, 0, len(workflows))
	for _, workflow := range workflows {
		counts := map[model.State]int{model.Pending: 0, model.Running: 0, model.Succeeded: 0, model.Failed: 0, model.Cancelled: 0, model.Skipped: 0}
		for _, task := range workflow.Tasks {
			counts[task.State]++
		}
		summaries = append(summaries, WorkflowSummary{workflow.ID, workflow.Name, workflow.State, workflow.CreatedAt, len(workflow.Tasks), counts})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": summaries})
}

func workflowID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 || strings.ToLower(id) != id {
		writeError(w, http.StatusBadRequest, "invalid_workflow_id", "Workflow ID must be 32 lowercase hexadecimal characters")
		return "", false
	}
	return id, true
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	workflow, err := s.store.Get(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflow)
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	if err := s.store.Cancel(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) nodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.Nodes(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}
