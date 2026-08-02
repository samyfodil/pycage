// Package pycage executes stateful CPython cells inside Wazy WebAssembly
// Component Model instances.
package pycage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/component"
)

const (
	componentInterface = "pycage:sandbox/code-interpreter@0.1.0"
	wasmPageBytes      = 64 * 1024
)

var ErrClosed = errors.New("pycage: sandbox is closed")

// Config controls host-enforced sandbox limits. Network access and host
// filesystem access are always denied by default.
type Config struct {
	Timeout          time.Duration
	MemoryLimitBytes uint64
}

func DefaultConfig() Config {
	return Config{
		Timeout:          5 * time.Second,
		MemoryLimitBytes: 256 << 20,
	}
}

// Sandbox is one persistent Python context and isolated in-memory filesystem.
// Calls are serialized because a Python context is stateful.
type Sandbox struct {
	mu      sync.Mutex
	runtime wazy.Runtime
	inst    *component.Instance
	fs      map[string][]byte
	config  Config
	closed  bool
}

// New creates a sandbox backed by the embedded componentized CPython guest.
func New(ctx context.Context, config Config) (*Sandbox, error) {
	if config.Timeout <= 0 {
		config.Timeout = DefaultConfig().Timeout
	}
	if config.MemoryLimitBytes == 0 {
		config.MemoryLimitBytes = DefaultConfig().MemoryLimitBytes
	}

	pages := config.MemoryLimitBytes / wasmPageBytes
	if config.MemoryLimitBytes%wasmPageBytes != 0 {
		pages++
	}
	if pages > uint64(^uint32(0)) {
		return nil, fmt.Errorf("pycage: memory limit is too large")
	}

	runtimeConfig := wazy.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | api.CoreFeatureExtendedConst).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(uint32(pages))
	r := wazy.NewRuntimeWithConfig(ctx, runtimeConfig)
	fs := map[string][]byte{}
	options := component.WithWASI(component.WASIConfig{FS: fs})
	inst, err := component.Instantiate(ctx, r, embeddedGuest, options...)
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("pycage: instantiate CPython component: %w", err)
	}

	return &Sandbox{runtime: r, inst: inst, fs: fs, config: config}, nil
}

// RunCode evaluates one cell. Variables and imports remain available to later
// calls. A Python exception is returned in Execution.Error; host/runtime
// failures are returned as Go errors.
func (s *Sandbox) RunCode(ctx context.Context, code string) (Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Execution{}, ErrClosed
	}

	callCtx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()
	values, err := s.inst.CallExport(callCtx, componentInterface, "run-code", code)
	if err != nil {
		// A trap or cancellation can close an underlying core module. Retire the
		// whole context so a caller never accidentally reuses partial state.
		_ = s.closeLocked(context.Background())
		return Execution{}, fmt.Errorf("pycage: execute: %w", err)
	}
	if len(values) != 1 {
		return Execution{}, fmt.Errorf("pycage: guest returned %d values, want 1", len(values))
	}
	payload, ok := values[0].(string)
	if !ok {
		return Execution{}, fmt.Errorf("pycage: guest returned %T, want string", values[0])
	}

	var execution Execution
	if err := json.Unmarshal([]byte(payload), &execution); err != nil {
		return Execution{}, fmt.Errorf("pycage: decode guest result: %w", err)
	}
	return execution, nil
}

// Reset clears all user-defined Python globals while keeping the component and
// filesystem alive.
func (s *Sandbox) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	_, err := s.inst.CallExport(ctx, componentInterface, "reset")
	if err != nil {
		return fmt.Errorf("pycage: reset: %w", err)
	}
	return nil
}

// WriteFile adds or replaces a file in the sandbox's isolated filesystem.
func (s *Sandbox) WriteFile(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	name, err := cleanGuestPath(name)
	if err != nil {
		return err
	}
	s.fs[name] = append([]byte(nil), data...)
	return nil
}

// ReadFile copies a file out of the sandbox's isolated filesystem.
func (s *Sandbox) ReadFile(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	name, err := cleanGuestPath(name)
	if err != nil {
		return nil, err
	}
	data, ok := s.fs[name]
	if !ok {
		return nil, fmt.Errorf("pycage: file %q does not exist", name)
	}
	return append([]byte(nil), data...), nil
}

// Close releases the component and its Wazy runtime.
func (s *Sandbox) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked(ctx)
}

func (s *Sandbox) closeLocked(ctx context.Context) error {
	if s.closed {
		return nil
	}
	s.closed = true
	instanceErr := s.inst.Close(ctx)
	runtimeErr := s.runtime.Close(ctx)
	return errors.Join(instanceErr, runtimeErr)
}

func cleanGuestPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("pycage: empty file path")
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if cleaned == "/" || strings.Contains(cleaned, "\x00") {
		return "", fmt.Errorf("pycage: invalid file path %q", name)
	}
	return cleaned, nil
}
