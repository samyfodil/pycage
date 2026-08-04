package pycage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DefaultServerPort matches the port E2B's code interpreter listens on, so a
// client pointed at pycage needs no configuration beyond the host.
const DefaultServerPort = 49999

const (
	defaultMaxContexts = 32
	defaultIdleTimeout = 10 * time.Minute
)

// ServerConfig tunes the HTTP server wrapped around an Engine.
type ServerConfig struct {
	// AccessToken, when set, must be presented as X-Access-Token. Leave empty
	// only when the listener is not reachable from another host.
	AccessToken string
	// MaxContexts caps simultaneous Python contexts. Zero uses 32. Each context
	// is a live CPython instance, so this bounds the server's memory.
	MaxContexts int
	// IdleTimeout closes a context that has gone unused for this long. Zero uses
	// ten minutes. Clients that forget to delete a context cannot leak one.
	IdleTimeout time.Duration
}

// Server exposes an Engine over E2B's code-interpreter HTTP API: /execute plus
// the /contexts lifecycle. One E2B context is one pycage Sandbox, so contexts
// keep independent Python globals and an independent filesystem.
//
// Output is not incremental. pycage's guest returns a cell's effects when the
// cell finishes, so every NDJSON frame for one execution is written at once.
// Clients that accumulate frames see identical results; clients that render
// partial output as it arrives see it all land together.
type Server struct {
	engine *Engine
	config ServerConfig
	mux    *http.ServeMux

	mu        sync.Mutex
	contexts  map[string]*serverContext
	defaultID string
	closed    bool

	stop chan struct{}
	done chan struct{}
}

type serverContext struct {
	ID         string `json:"id"`
	Language   string `json:"language"`
	CWD        string `json:"cwd"`
	sandbox    *Sandbox
	executions int
	lastUsed   time.Time
}

// NewServer wraps an Engine. The Engine is not closed by Server.Close; the
// caller keeps ownership of it.
func NewServer(engine *Engine, config ServerConfig) *Server {
	if config.MaxContexts <= 0 {
		config.MaxContexts = defaultMaxContexts
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	server := &Server{
		engine:   engine,
		config:   config,
		mux:      http.NewServeMux(),
		contexts: map[string]*serverContext{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	server.mux.HandleFunc("GET /health", server.handleHealth)
	server.mux.HandleFunc("POST /contexts", server.handleCreateContext)
	server.mux.HandleFunc("GET /contexts", server.handleListContexts)
	server.mux.HandleFunc("DELETE /contexts/{id}", server.handleDeleteContext)
	server.mux.HandleFunc("POST /execute", server.handleExecute)
	go server.reapIdle()
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.config.AccessToken != "" && r.URL.Path != "/health" {
		if r.Header.Get("X-Access-Token") != s.config.AccessToken {
			writeJSONError(w, http.StatusUnauthorized, "invalid access token")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// Close releases every live context and stops the idle reaper.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	sandboxes := make([]*Sandbox, 0, len(s.contexts))
	for _, entry := range s.contexts {
		sandboxes = append(sandboxes, entry.sandbox)
	}
	s.contexts = map[string]*serverContext{}
	s.mu.Unlock()

	close(s.stop)
	<-s.done
	var err error
	for _, sandbox := range sandboxes {
		if closeErr := sandbox.Close(ctx); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

func (s *Server) reapIdle() {
	defer close(s.done)
	ticker := time.NewTicker(s.config.IdleTimeout / 2)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-s.config.IdleTimeout)
			s.mu.Lock()
			var expired []*Sandbox
			for id, entry := range s.contexts {
				if entry.lastUsed.Before(cutoff) {
					expired = append(expired, entry.sandbox)
					delete(s.contexts, id)
					if s.defaultID == id {
						s.defaultID = ""
					}
				}
			}
			s.mu.Unlock()
			for _, sandbox := range expired {
				sandbox.Close(context.Background())
			}
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createContextRequest struct {
	Language string `json:"language"`
	CWD      string `json:"cwd"`
}

func (s *Server) handleCreateContext(w http.ResponseWriter, r *http.Request) {
	var request createContextRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
			return
		}
	}
	if !isPython(request.Language) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unsupported language %q; pycage runs Python only", request.Language))
		return
	}
	entry, err := s.newContext(r.Context(), request.CWD)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handleListContexts(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listed := make([]*serverContext, 0, len(s.contexts))
	for _, entry := range s.contexts {
		listed = append(listed, entry)
	}
	writeJSON(w, http.StatusOK, listed)
}

func (s *Server) handleDeleteContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	entry, ok := s.contexts[id]
	if ok {
		delete(s.contexts, id)
		if s.defaultID == id {
			s.defaultID = ""
		}
	}
	s.mu.Unlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("context %q not found", id))
		return
	}
	if err := entry.sandbox.Close(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type executeRequest struct {
	Code      string            `json:"code"`
	ContextID string            `json:"context_id"`
	Language  string            `json:"language"`
	EnvVars   map[string]string `json:"env_vars"`
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var request executeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if request.ContextID != "" && request.Language != "" {
		writeJSONError(w, http.StatusBadRequest, "provide context_id or language, not both")
		return
	}
	if !isPython(request.Language) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unsupported language %q; pycage runs Python only", request.Language))
		return
	}

	entry, err := s.contextFor(r.Context(), request.ContextID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}

	execution, err := entry.sandbox.RunCode(r.Context(), request.Code)
	if err != nil {
		// The sandbox is retired on a trap or timeout, so drop it rather than
		// hand the next request a dead context.
		s.forget(entry.ID)
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	entry.executions++
	entry.lastUsed = time.Now()
	count := entry.executions
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	stream := newNDJSONWriter(w)
	timestamp := time.Now().UnixNano()
	for _, line := range splitLines(execution.Stdout) {
		stream.write(map[string]any{"type": "stdout", "text": line, "timestamp": timestamp})
	}
	for _, line := range splitLines(execution.Stderr) {
		stream.write(map[string]any{"type": "stderr", "text": line, "timestamp": timestamp})
	}
	for index, output := range execution.Outputs {
		frame := map[string]any{"type": "result", "is_main_result": index == len(execution.Outputs)-1}
		switch output.Type {
		case "text", "html", "markdown", "svg", "png", "jpeg", "pdf", "latex", "javascript", "json":
			frame[output.Type] = output.Data
		default:
			frame["extra"] = map[string]any{output.Type: output.Data}
		}
		stream.write(frame)
	}
	if execution.Error != nil {
		stream.write(map[string]any{
			"type":      "error",
			"name":      execution.Error.Name,
			"value":     execution.Error.Message,
			"traceback": execution.Error.Traceback,
		})
	}
	stream.write(map[string]any{"type": "number_of_executions", "execution_count": count})
}

func (s *Server) newContext(ctx context.Context, cwd string) (*serverContext, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if len(s.contexts) >= s.config.MaxContexts {
		s.mu.Unlock()
		return nil, fmt.Errorf("pycage: context limit of %d reached", s.config.MaxContexts)
	}
	s.mu.Unlock()

	sandbox, err := s.engine.NewSandbox(ctx)
	if err != nil {
		return nil, err
	}
	if cwd == "" {
		cwd = "/"
	}
	entry := &serverContext{
		ID:       newContextID(),
		Language: "python",
		CWD:      cwd,
		sandbox:  sandbox,
		lastUsed: time.Now(),
	}

	s.mu.Lock()
	// Re-check under the lock: NewSandbox is slow and races could overshoot.
	if s.closed || len(s.contexts) >= s.config.MaxContexts {
		s.mu.Unlock()
		sandbox.Close(ctx)
		return nil, fmt.Errorf("pycage: context limit of %d reached", s.config.MaxContexts)
	}
	s.contexts[entry.ID] = entry
	s.mu.Unlock()
	return entry, nil
}

// contextFor resolves an explicit id, or lazily creates the default context the
// E2B clients use when they send no context_id.
func (s *Server) contextFor(ctx context.Context, id string) (*serverContext, error) {
	s.mu.Lock()
	if id != "" {
		entry, ok := s.contexts[id]
		s.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("context %q not found", id)
		}
		return entry, nil
	}
	if entry, ok := s.contexts[s.defaultID]; s.defaultID != "" && ok {
		s.mu.Unlock()
		return entry, nil
	}
	s.mu.Unlock()

	entry, err := s.newContext(ctx, "/")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.defaultID == "" {
		s.defaultID = entry.ID
	}
	s.mu.Unlock()
	return entry, nil
}

func (s *Server) forget(id string) {
	s.mu.Lock()
	delete(s.contexts, id)
	if s.defaultID == id {
		s.defaultID = ""
	}
	s.mu.Unlock()
}

// isPython accepts the values an E2B client sends for a Python context. pycage
// has no other runtime, so anything else is rejected rather than silently run.
func isPython(language string) bool {
	switch language {
	case "", "python", "python3", "py":
		return true
	}
	return false
}

// splitLines keeps the trailing newline on each line, matching how a Jupyter
// kernel reports stream output.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			lines = append(lines, text[start:index+1])
			start = index + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

type ndjsonWriter struct {
	encoder *json.Encoder
	flusher http.Flusher
}

func newNDJSONWriter(w http.ResponseWriter) *ndjsonWriter {
	stream := &ndjsonWriter{encoder: json.NewEncoder(w)}
	if flusher, ok := w.(http.Flusher); ok {
		stream.flusher = flusher
	}
	return stream
}

func (n *ndjsonWriter) write(frame map[string]any) {
	if n.encoder.Encode(frame) != nil {
		return
	}
	if n.flusher != nil {
		n.flusher.Flush()
	}
}

func newContextID() string {
	buffer := make([]byte, 16)
	rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"code": status, "message": message})
}
