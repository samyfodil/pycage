package pycage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// PipResult is the captured result of an embedded pip invocation.
type PipResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// PipInstall installs requirements into this sandbox's ephemeral
// /site-packages using the pip embedded in the CPython component. Pure-Python
// wheels work without subprocess support. Network access is available only
// when Config.AllowNetwork is explicitly enabled.
func (s *Sandbox) PipInstall(ctx context.Context, requirements ...string) (PipResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PipResult{}, ErrClosed
	}
	if len(requirements) == 0 {
		return PipResult{}, fmt.Errorf("pycage: pip install requires at least one requirement")
	}
	if s.engine.pypi != nil {
		for name := range s.fs {
			if strings.HasPrefix(name, "/pycage-wheels/") {
				delete(s.fs, name)
			}
		}
		if err := s.engine.pypi.Prefetch(ctx, requirements, s.fs); err != nil {
			return PipResult{}, err
		}
	}

	arguments := []string{
		"install",
		"--disable-pip-version-check",
		"--no-input",
		"--progress-bar", "off",
		"--no-compile",
		"--no-warn-conflicts",
		"--root-user-action", "ignore",
		"--only-binary", ":all:",
	}
	if s.engine.pypi != nil {
		arguments = append(arguments, "--prefix", "/pycage-install", "--no-index", "--no-deps")
		var wheelPaths []string
		for name := range s.fs {
			if strings.HasPrefix(name, "/pycage-wheels/") {
				wheelPaths = append(wheelPaths, name)
			}
		}
		sort.Strings(wheelPaths)
		arguments = append(arguments, wheelPaths...)
	} else {
		arguments = append(arguments, "--target", "/site-packages")
		arguments = append(arguments, requirements...)
	}
	payload, err := json.Marshal(map[string]any{"pycage_pip_arguments": arguments})
	if err != nil {
		return PipResult{}, fmt.Errorf("pycage: encode pip arguments: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()
	delete(s.fs, "/.pycage-pip-result.json")
	values, err := s.inst.CallExport(callCtx, componentInterface, "install-modules", string(payload))
	if err != nil {
		_ = s.closeLocked(context.Background())
		return PipResult{}, fmt.Errorf("pycage: pip install: %w", err)
	}
	if len(values) != 1 {
		return PipResult{}, fmt.Errorf("pycage: pip install returned %d values, want 1", len(values))
	}
	response, ok := values[0].(string)
	if !ok {
		return PipResult{}, fmt.Errorf("pycage: pip install returned %T, want string", values[0])
	}
	if mirrored, exists := s.fs["/.pycage-pip-result.json"]; exists {
		response = string(mirrored)
		delete(s.fs, "/.pycage-pip-result.json")
	}
	var result PipResult
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		return PipResult{}, fmt.Errorf("pycage: decode pip result %q: %w", response, err)
	}
	if result.ExitCode == 0 && result.Error == "" {
		if err := s.finalizePipTargetLocked(callCtx); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Sandbox) finalizePipTargetLocked(ctx context.Context) error {
	moved := false
	for name, contents := range s.fs {
		var relative string
		if strings.HasPrefix(name, "/pycage-install/") {
			const marker = "/site-packages/"
			index := strings.Index(name, marker)
			if index < 0 {
				continue
			}
			relative = name[index+len(marker):]
		} else if strings.HasPrefix(name, "/tmp/pip-target-") {
			const marker = "/lib/python/"
			index := strings.Index(name, marker)
			if index < 0 {
				continue
			}
			relative = name[index+len(marker):]
		} else {
			continue
		}
		if relative == "" {
			continue
		}
		lower := strings.ToLower(relative)
		if strings.HasSuffix(lower, ".so") || strings.HasSuffix(lower, ".dll") ||
			strings.HasSuffix(lower, ".dylib") || strings.HasSuffix(lower, ".wasm") {
			return fmt.Errorf("pycage: pip package contains native file %q", relative)
		}
		s.fs["/site-packages/"+relative] = append([]byte(nil), contents...)
		delete(s.fs, name)
		moved = true
	}
	if !moved {
		return fmt.Errorf("pycage: pip succeeded but produced no installable files")
	}

	modules := map[string]wheelModule{}
	for name, contents := range s.fs {
		if !strings.HasPrefix(name, "/site-packages/") {
			continue
		}
		relative := strings.TrimPrefix(name, "/site-packages/")
		moduleName, isPackage, ok := wheelModuleName(relative)
		if !ok {
			continue
		}
		if !utf8.Valid(contents) {
			return fmt.Errorf("pycage: installed Python source %q is not UTF-8", relative)
		}
		modules[moduleName] = wheelModule{Source: string(contents), Package: isPackage}
	}
	for moduleName := range modules {
		parts := strings.Split(moduleName, ".")
		for index := 1; index < len(parts); index++ {
			parent := strings.Join(parts[:index], ".")
			if _, exists := modules[parent]; !exists {
				modules[parent] = wheelModule{Source: "", Package: true}
			}
		}
	}
	if len(modules) == 0 {
		return fmt.Errorf("pycage: pip installed no importable Python modules")
	}
	payload, err := json.Marshal(modules)
	if err != nil {
		return fmt.Errorf("pycage: encode pip-installed modules: %w", err)
	}
	return s.installModulesLocked(ctx, payload)
}
