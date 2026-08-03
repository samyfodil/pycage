package pycage

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxPackageDownloadBytes = 128 << 20
	maxPackageDownloadTotal = 256 << 20
	maxResolvedPackages     = 100
)

var requirementNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*`)

type pypiDownloader struct {
	client *http.Client
}

func newPyPIDownloader() *pypiDownloader {
	return &pypiDownloader{client: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			host := request.URL.Hostname()
			if request.URL.Scheme != "https" || (host != "pypi.org" && host != "files.pythonhosted.org") {
				return fmt.Errorf("redirect to disallowed package host %q", host)
			}
			return nil
		},
	}}
}

func (d *pypiDownloader) Prefetch(ctx context.Context, requirements []string, writeFile func(string, []byte) error) error {
	state := &pypiDownloadState{downloader: d, writeFile: writeFile, seen: map[string]bool{}}
	for _, requirement := range requirements {
		if strings.HasPrefix(requirement, "-") || strings.HasPrefix(requirement, "/") {
			continue
		}
		if err := state.fetchRequirement(ctx, requirement); err != nil {
			return err
		}
	}
	return nil
}

type pypiDownloadState struct {
	downloader *pypiDownloader
	writeFile  func(string, []byte) error
	seen       map[string]bool
	total      int64
}

func (s *pypiDownloadState) fetchRequirement(ctx context.Context, requirement string) error {
	name, version, ok := parsePyPIRequirement(requirement)
	if !ok {
		return fmt.Errorf("pycage: unsupported network requirement %q; use a package name or exact == version", requirement)
	}
	normalized := strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(name))
	if s.seen[normalized] {
		return nil
	}
	if len(s.seen) >= maxResolvedPackages {
		return fmt.Errorf("pycage: dependency graph exceeds %d packages", maxResolvedPackages)
	}
	s.seen[normalized] = true

	endpoint := "https://pypi.org/pypi/" + url.PathEscape(name)
	if version != "" {
		endpoint += "/" + url.PathEscape(version)
	}
	endpoint += "/json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := s.downloader.client.Do(request)
	if err != nil {
		return fmt.Errorf("pycage: fetch PyPI metadata for %q: %w", name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pycage: PyPI metadata for %q returned %s", name, response.Status)
	}
	var project struct {
		Info struct {
			RequiresDist []string `json:"requires_dist"`
		} `json:"info"`
		URLs []struct {
			Filename    string `json:"filename"`
			PackageType string `json:"packagetype"`
			URL         string `json:"url"`
			Size        int64  `json:"size"`
			Digests     struct {
				SHA256 string `json:"sha256"`
			} `json:"digests"`
		} `json:"urls"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&project); err != nil {
		return fmt.Errorf("pycage: decode PyPI metadata for %q: %w", name, err)
	}
	var wheel struct {
		Filename string
		URL      string
		Size     int64
		SHA256   string
	}
	for _, candidate := range project.URLs {
		if candidate.PackageType == "bdist_wheel" && strings.HasSuffix(strings.ToLower(candidate.Filename), "-none-any.whl") {
			wheel.Filename, wheel.URL, wheel.Size, wheel.SHA256 = candidate.Filename, candidate.URL, candidate.Size, candidate.Digests.SHA256
			if strings.Contains(strings.ToLower(candidate.Filename), "-py3-none-any.whl") {
				break
			}
		}
	}
	if wheel.URL == "" {
		return fmt.Errorf("pycage: %q has no pure-Python wheel for the selected version", requirement)
	}
	if wheel.Size > maxPackageDownloadBytes || s.total+wheel.Size > maxPackageDownloadTotal {
		return fmt.Errorf("pycage: package download limit exceeded by %q", wheel.Filename)
	}
	contents, err := s.downloadWheel(ctx, wheel.URL)
	if err != nil {
		return fmt.Errorf("pycage: download %q: %w", wheel.Filename, err)
	}
	if wheel.SHA256 == "" {
		return fmt.Errorf("pycage: PyPI supplied no SHA-256 digest for %q", wheel.Filename)
	}
	digest := sha256.Sum256(contents)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), wheel.SHA256) {
		return fmt.Errorf("pycage: SHA-256 mismatch for %q", wheel.Filename)
	}
	contents, err = stripWheelEntryPoints(contents)
	if err != nil {
		return fmt.Errorf("pycage: sanitize %q: %w", wheel.Filename, err)
	}
	s.total += int64(len(contents))
	if s.total > maxPackageDownloadTotal {
		return fmt.Errorf("pycage: package downloads exceed %d bytes", maxPackageDownloadTotal)
	}
	if err := s.writeFile("/pycage-wheels/"+filepath.Base(wheel.Filename), contents); err != nil {
		return fmt.Errorf("pycage: write wheelhouse: %w", err)
	}

	for _, dependency := range project.Info.RequiresDist {
		if marker := strings.SplitN(dependency, ";", 2); len(marker) == 2 && strings.Contains(strings.ToLower(marker[1]), "extra") {
			continue
		}
		if err := s.fetchRequirement(ctx, dependency); err != nil {
			// Marker-only and extra dependencies are allowed to be absent from
			// the wheelhouse; embedded pip evaluates whether they apply.
			if strings.Contains(dependency, ";") {
				continue
			}
			return fmt.Errorf("pycage: dependency of %q: %w", name, err)
		}
	}
	return nil
}

func stripWheelEntryPoints(wheel []byte) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(wheel), int64(len(wheel)))
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, file := range reader.File {
		if strings.HasSuffix(strings.ToLower(file.Name), ".dist-info/entry_points.txt") {
			continue
		}
		source, err := file.Open()
		if err != nil {
			return nil, err
		}
		header := file.FileHeader
		destination, err := writer.CreateHeader(&header)
		if err == nil {
			_, err = io.Copy(destination, source)
		}
		closeErr := source.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (s *pypiDownloadState) downloadWheel(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "files.pythonhosted.org" {
		return nil, fmt.Errorf("PyPI returned a disallowed package URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := s.downloader.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("package host returned %s", response.Status)
	}
	if response.ContentLength > maxPackageDownloadBytes {
		return nil, fmt.Errorf("package exceeds %d bytes", maxPackageDownloadBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxPackageDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > maxPackageDownloadBytes {
		return nil, fmt.Errorf("package exceeds %d bytes", maxPackageDownloadBytes)
	}
	return contents, nil
}

func parsePyPIRequirement(requirement string) (name, version string, ok bool) {
	requirement = strings.TrimSpace(strings.SplitN(requirement, ";", 2)[0])
	match := requirementNamePattern.FindString(requirement)
	if match == "" {
		return "", "", false
	}
	name = match
	rest := strings.TrimSpace(requirement[len(match):])
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return "", "", false
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	if rest == "" {
		return name, "", true
	}
	if !strings.ContainsAny(rest[:1], "<>=!~") {
		return "", "", false
	}
	if !strings.HasPrefix(rest, "==") {
		// The lightweight prefetcher selects the latest project release for
		// ordinary ranges. Callers should pin top-level requirements exactly.
		return name, "", true
	}
	version = strings.TrimSpace(strings.TrimPrefix(rest, "=="))
	if version == "" || strings.ContainsAny(version, "*, ") {
		return "", "", false
	}
	return name, version, true
}
