// Package pycage executes stateful CPython cells inside Wazy WebAssembly
// Component Model instances.
package pycage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	Timeout             time.Duration
	MemoryLimitBytes    uint64
	RuntimeMode         RuntimeMode
	CompilationCacheDir string
	AllowNetwork        bool
	// HTTPClient handles wasi:http outgoing requests when network access is
	// enabled. Nil uses Go's default client and certificate verification.
	HTTPClient *http.Client
	// FileSystem creates the Afero mounts exposed to each sandbox. Nil uses a
	// private in-memory COW layer backed by a temporary host directory.
	FileSystem FileSystemFactory
}

// RuntimeMode selects Wazy's execution backend. The zero value uses the native
// compiler. Interpreter mode trades execution speed for lower cold-start cost.
type RuntimeMode string

const (
	RuntimeModeCompiler    RuntimeMode = "compiler"
	RuntimeModeInterpreter RuntimeMode = "interpreter"
)

// Engine owns a Wazy runtime and compiled-component cache. Reuse an Engine to
// avoid decoding and compiling the embedded CPython component for every
// sandbox. Each sandbox still gets independent Python state and a separate
// in-memory filesystem.
type Engine struct {
	mu      sync.Mutex
	runtime wazy.Runtime
	cache   *component.CompileCache
	native  wazy.CompilationCache
	pypi    *pypiDownloader
	config  Config
	closed  bool
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
	mu     sync.Mutex
	inst   *component.Instance
	fs     *sandboxFilesystem
	config Config
	engine *Engine
	owned  bool
	closed bool
}

// New creates a sandbox backed by the embedded componentized CPython guest.
func New(ctx context.Context, config Config) (*Sandbox, error) {
	engine, err := NewEngine(ctx, config)
	if err != nil {
		return nil, err
	}
	sandbox, err := engine.NewSandbox(ctx)
	if err != nil {
		_ = engine.Close(ctx)
		return nil, err
	}
	sandbox.owned = true
	return sandbox, nil
}

// NewEngine creates a reusable Wazy runtime. Component compilation happens
// lazily when the first sandbox is created and is reused by later sandboxes.
func NewEngine(ctx context.Context, config Config) (*Engine, error) {
	config, pages, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}

	var runtimeConfig wazy.RuntimeConfig
	switch config.RuntimeMode {
	case "", RuntimeModeCompiler:
		runtimeConfig = wazy.NewRuntimeConfig()
	case RuntimeModeInterpreter:
		runtimeConfig = wazy.NewRuntimeConfigInterpreter()
	default:
		return nil, fmt.Errorf("pycage: unknown runtime mode %q", config.RuntimeMode)
	}
	runtimeConfig = runtimeConfig.
		WithCoreFeatures(api.CoreFeaturesV2 | api.CoreFeatureExtendedConst).
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(pages)
	var nativeCache wazy.CompilationCache
	if config.CompilationCacheDir != "" {
		nativeCache, err = wazy.NewCompilationCacheWithDir(config.CompilationCacheDir)
		if err != nil {
			return nil, fmt.Errorf("pycage: create native compilation cache: %w", err)
		}
		runtimeConfig = runtimeConfig.WithCompilationCache(nativeCache)
	}
	var pypi *pypiDownloader
	if config.AllowNetwork {
		pypi = newPyPIDownloader()
	}
	return &Engine{
		runtime: wazy.NewRuntimeWithConfig(ctx, runtimeConfig),
		cache:   component.NewCompileCache(),
		native:  nativeCache,
		pypi:    pypi,
		config:  config,
	}, nil
}

func normalizeConfig(config Config) (Config, uint32, error) {
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
		return Config{}, 0, fmt.Errorf("pycage: memory limit is too large")
	}
	return config, uint32(pages), nil
}

// NewSandbox creates an isolated CPython instance. The Engine's decoded and
// compiled component is reused, but no Python globals or files are shared.
func (e *Engine) NewSandbox(ctx context.Context) (*Sandbox, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}

	filesystem, fsConfig, err := newSandboxFilesystem(e.config.FileSystem)
	if err != nil {
		return nil, err
	}
	inst, err := e.instantiateLocked(ctx, fsConfig)
	if err != nil {
		_ = filesystem.close()
		return nil, err
	}

	return &Sandbox{inst: inst, fs: filesystem, config: e.config, engine: e}, nil
}

func (e *Engine) instantiateLocked(ctx context.Context, fsConfig wazy.FSConfig) (*component.Instance, error) {
	options := component.WithWASI(component.WASIConfig{
		FS:         fsConfig,
		AllowTCP:   e.config.AllowNetwork,
		EnableHTTP: e.config.AllowNetwork,
		HTTPClient: e.config.HTTPClient,
	})
	options = append(options, component.WithCompileCache(e.cache))
	inst, err := component.Instantiate(ctx, e.runtime, embeddedGuest, options...)
	if err != nil {
		return nil, fmt.Errorf("pycage: instantiate CPython component: %w", err)
	}
	return inst, nil
}

func (s *Sandbox) restartLocked(ctx context.Context) error {
	s.engine.mu.Lock()
	defer s.engine.mu.Unlock()
	if s.engine.closed {
		return ErrClosed
	}
	if err := s.inst.Close(ctx); err != nil {
		return fmt.Errorf("pycage: close retired component: %w", err)
	}
	inst, err := s.engine.instantiateLocked(ctx, s.fs.config)
	if err != nil {
		return err
	}
	s.inst = inst
	return nil
}

// Close releases the Engine's compiled component and Wazy runtime. All
// sandboxes created by this Engine must be closed first.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	componentErr := e.cache.Close(ctx)
	runtimeErr := e.runtime.Close(ctx)
	var nativeErr error
	if e.native != nil {
		nativeErr = e.native.Close(ctx)
	}
	return errors.Join(componentErr, runtimeErr, nativeErr)
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
	_ = s.fs.remove("/.pycage-run-result.json")
	values, err := s.inst.CallExport(callCtx, componentInterface, "run-code", code)
	var payload string
	if mirrored, readErr := s.fs.readFile("/.pycage-run-result.json"); readErr == nil {
		payload = string(mirrored)
		_ = s.fs.remove("/.pycage-run-result.json")
	}
	if err != nil {
		if payload != "" {
			var execution Execution
			if decodeErr := json.Unmarshal([]byte(payload), &execution); decodeErr != nil {
				return Execution{}, fmt.Errorf("pycage: decode mirrored guest result: %w", decodeErr)
			}
			return execution, nil
		}
		// A trap or cancellation can close an underlying core module. Retire the
		// whole context so a caller never accidentally reuses partial state.
		_ = s.closeLocked(context.Background())
		return Execution{}, fmt.Errorf("pycage: execute: %w", err)
	}
	if len(values) != 1 {
		return Execution{}, fmt.Errorf("pycage: guest returned %d values, want 1", len(values))
	}
	if payload == "" {
		var ok bool
		payload, ok = values[0].(string)
		if !ok {
			return Execution{}, fmt.Errorf("pycage: guest returned %T, want string", values[0])
		}
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
	if err := s.fs.writeFile(name, data); err != nil {
		return fmt.Errorf("pycage: write file %q: %w", name, err)
	}
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
	data, err := s.fs.readFile(name)
	if err != nil {
		return nil, fmt.Errorf("pycage: read file %q: %w", name, err)
	}
	return data, nil
}

// Close releases the component. A sandbox made with New also releases its
// private Engine; sandboxes made by Engine.NewSandbox leave the Engine alive.
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
	filesystemErr := s.fs.close()
	if s.owned {
		return errors.Join(instanceErr, filesystemErr, s.engine.Close(ctx))
	}
	return errors.Join(instanceErr, filesystemErr)
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
