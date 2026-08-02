package pycage

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	maxWheelFiles = 10_000
	maxWheelBytes = 64 << 20
)

// WheelInfo describes one pure-Python wheel installed in a sandbox.
type WheelInfo struct {
	Name    string
	Version string
	Files   int
}

type wheelModule struct {
	Source  string `json:"source"`
	Package bool   `json:"package"`
}

// InstallWheel validates and extracts a pure-Python wheel into the sandbox's
// ephemeral /site-packages directory. Native wheels and source distributions
// are deliberately unsupported.
func (s *Sandbox) InstallWheel(wheel []byte) (WheelInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return WheelInfo{}, ErrClosed
	}

	reader, err := zip.NewReader(bytes.NewReader(wheel), int64(len(wheel)))
	if err != nil {
		return WheelInfo{}, fmt.Errorf("pycage: invalid wheel: %w", err)
	}
	if len(reader.File) > maxWheelFiles {
		return WheelInfo{}, fmt.Errorf("pycage: wheel contains %d files, limit is %d", len(reader.File), maxWheelFiles)
	}

	var wheelMetadata, packageMetadata string
	var total uint64
	for _, file := range reader.File {
		name, err := cleanWheelPath(file.Name)
		if err != nil {
			return WheelInfo{}, err
		}
		if name == "" {
			continue
		}
		total += file.UncompressedSize64
		if total > maxWheelBytes {
			return WheelInfo{}, fmt.Errorf("pycage: expanded wheel exceeds %d bytes", maxWheelBytes)
		}
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".so") || strings.HasSuffix(lower, ".dll") || strings.HasSuffix(lower, ".dylib") || strings.HasSuffix(lower, ".wasm") {
			return WheelInfo{}, fmt.Errorf("pycage: wheel contains native file %q", name)
		}

		contents, err := readZipFile(file)
		if err != nil {
			return WheelInfo{}, fmt.Errorf("pycage: read wheel file %q: %w", name, err)
		}
		switch {
		case strings.HasSuffix(name, ".dist-info/WHEEL"):
			wheelMetadata = string(contents)
		case strings.HasSuffix(name, ".dist-info/METADATA"):
			packageMetadata = string(contents)
		}
	}

	if wheelMetadata == "" {
		return WheelInfo{}, fmt.Errorf("pycage: wheel has no .dist-info/WHEEL metadata")
	}
	if !metadataHasValue(wheelMetadata, "Root-Is-Purelib", "true") {
		return WheelInfo{}, fmt.Errorf("pycage: wheel is not marked as pure Python")
	}
	if !metadataValueHasSuffix(wheelMetadata, "Tag", "-none-any") {
		return WheelInfo{}, fmt.Errorf("pycage: wheel has no platform-independent tag")
	}

	info := WheelInfo{
		Name:    metadataValue(packageMetadata, "Name"),
		Version: metadataValue(packageMetadata, "Version"),
	}
	modules := map[string]wheelModule{}
	for _, file := range reader.File {
		name, err := cleanWheelPath(file.Name)
		if err != nil || name == "" {
			continue
		}
		contents, err := readZipFile(file)
		if err != nil {
			return WheelInfo{}, fmt.Errorf("pycage: read wheel file %q: %w", name, err)
		}
		s.fs["/site-packages/"+name] = contents
		if moduleName, isPackage, ok := wheelModuleName(name); ok {
			if !utf8.Valid(contents) {
				return WheelInfo{}, fmt.Errorf("pycage: Python source %q is not UTF-8", name)
			}
			modules[moduleName] = wheelModule{Source: string(contents), Package: isPackage}
		}
		info.Files++
	}
	if len(modules) == 0 {
		return WheelInfo{}, fmt.Errorf("pycage: wheel contains no importable Python modules")
	}
	payload, err := json.Marshal(modules)
	if err != nil {
		return WheelInfo{}, fmt.Errorf("pycage: encode wheel modules: %w", err)
	}
	callCtx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
	defer cancel()
	if err := s.installModulesLocked(callCtx, payload); err != nil {
		return WheelInfo{}, err
	}
	return info, nil
}

func (s *Sandbox) installModulesLocked(ctx context.Context, payload []byte) error {
	values, err := s.inst.CallExport(ctx, componentInterface, "install-modules", string(payload))
	if err != nil {
		_ = s.closeLocked(context.Background())
		return fmt.Errorf("pycage: install wheel modules: %w", err)
	}
	if len(values) != 1 {
		return fmt.Errorf("pycage: install wheel modules returned %d values, want 1", len(values))
	}
	response, ok := values[0].(string)
	if !ok {
		return fmt.Errorf("pycage: install wheel modules returned %T, want string", values[0])
	}
	var status struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(response), &status); err != nil {
		return fmt.Errorf("pycage: decode module installation result: %w", err)
	}
	if status.Error != "" {
		return fmt.Errorf("pycage: guest rejected modules: %s", status.Error)
	}
	return nil
}

func wheelModuleName(name string) (module string, isPackage, ok bool) {
	if marker := strings.Index(name, ".data/purelib/"); marker >= 0 {
		name = name[marker+len(".data/purelib/"):]
	}
	if strings.Contains(name, ".dist-info/") || !strings.HasSuffix(name, ".py") {
		return "", false, false
	}
	name = strings.TrimSuffix(name, ".py")
	if strings.HasSuffix(name, "/__init__") {
		name = strings.TrimSuffix(name, "/__init__")
		isPackage = true
	}
	if name == "" {
		return "", false, false
	}
	return strings.ReplaceAll(name, "/", "."), isPackage, true
}

func cleanWheelPath(name string) (string, error) {
	if strings.HasSuffix(name, "/") {
		return "", nil
	}
	cleaned := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") || strings.Contains(cleaned, "\x00") {
		return "", fmt.Errorf("pycage: unsafe wheel path %q", name)
	}
	return cleaned, nil
}

func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, maxWheelBytes+1))
}

func metadataValue(metadata, key string) string {
	prefix := strings.ToLower(key) + ":"
	for _, line := range strings.Split(metadata, "\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

func metadataHasValue(metadata, key, expected string) bool {
	return strings.EqualFold(metadataValue(metadata, key), expected)
}

func metadataValueHasSuffix(metadata, key, suffix string) bool {
	prefix := strings.ToLower(key) + ":"
	for _, line := range strings.Split(metadata, "\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) && strings.HasSuffix(strings.ToLower(strings.TrimSpace(line[len(prefix):])), suffix) {
			return true
		}
	}
	return false
}
