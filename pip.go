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
		if err := s.fs.removeAll("/pycage-wheels"); err != nil {
			return PipResult{}, fmt.Errorf("pycage: clear wheelhouse: %w", err)
		}
		if err := s.engine.pypi.Prefetch(ctx, requirements, s.fs.writeFile); err != nil {
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
		"--prefix", "/pycage-install",
	}
	if s.engine.pypi != nil {
		arguments = append(arguments, "--no-index", "--no-deps")
		wheelFiles, err := s.fs.listFiles("/pycage-wheels")
		if err != nil {
			return PipResult{}, fmt.Errorf("pycage: list wheelhouse: %w", err)
		}
		var wheelPaths []string
		for name := range wheelFiles {
			wheelPaths = append(wheelPaths, name)
		}
		sort.Strings(wheelPaths)
		arguments = append(arguments, wheelPaths...)
	} else {
		arguments = append(arguments, requirements...)
	}
	payload, err := json.Marshal(map[string]any{"pycage_pip_arguments": arguments})
	if err != nil {
		return PipResult{}, fmt.Errorf("pycage: encode pip arguments: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()
	_ = s.fs.remove("/.pycage-pip-result.json")
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
	if mirrored, readErr := s.fs.readFile("/.pycage-pip-result.json"); readErr == nil {
		response = string(mirrored)
		_ = s.fs.remove("/.pycage-pip-result.json")
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
	installed, err := s.fs.listFiles("/site-packages")
	if err != nil {
		return fmt.Errorf("pycage: inspect installed packages: %w", err)
	}
	staged := make(map[string][]byte)
	for _, root := range []string{"/pycage-install", "/tmp"} {
		files, err := s.fs.listFiles(root)
		if err != nil {
			return fmt.Errorf("pycage: inspect pip target: %w", err)
		}
		for name, contents := range files {
			staged[name] = contents
		}
	}
	moved := len(installed) > 0
	for name, contents := range staged {
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
		destination := "/site-packages/" + relative
		if err := s.fs.writeFile(destination, contents); err != nil {
			return fmt.Errorf("pycage: finalize package file %q: %w", destination, err)
		}
		if err := s.fs.remove(name); err != nil {
			return fmt.Errorf("pycage: remove staged package file %q: %w", name, err)
		}
		moved = true
	}
	if !moved {
		return fmt.Errorf("pycage: pip succeeded but produced no installable files")
	}

	modules := map[string]wheelModule{}
	installed, err = s.fs.listFiles("/site-packages")
	if err != nil {
		return fmt.Errorf("pycage: inspect installed packages: %w", err)
	}
	for name, contents := range installed {
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
