package pycage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestHTTPSViaWASIHTTP(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("tls works"))
	}))
	defer server.Close()

	ctx := context.Background()
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	config.AllowNetwork = true
	config.HTTPClient = server.Client()
	sandbox, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	code := fmt.Sprintf("import pycage_http; r = pycage_http.get(%q); (r.status_code, r.text)", server.URL)
	result, err := sandbox.RunCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != nil {
		t.Fatalf("HTTPS request: %+v", result.Error)
	}
	if result.Text() != "(200, 'tls works')" {
		t.Fatalf("HTTPS result = %q", result.Text())
	}
}

func TestGuestDoesNotAdvertiseUnusableIPv6(t *testing.T) {
	ctx := context.Background()
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	config.AllowNetwork = true
	sandbox, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	result, err := sandbox.RunCode(ctx, "import socket; socket.has_ipv6")
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != nil {
		t.Fatalf("import socket: %+v", result.Error)
	}
	if result.Text() != "False" {
		t.Fatalf("socket.has_ipv6 = %q", result.Text())
	}
}

func TestEmbeddedPipInstallsFromPyPI(t *testing.T) {
	if os.Getenv("PYCAGE_NETWORK_TESTS") != "1" {
		t.Skip("set PYCAGE_NETWORK_TESTS=1 to run the live PyPI test")
	}
	ctx := context.Background()
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	config.AllowNetwork = true
	config.Timeout = 60 * time.Second
	sandbox, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	installed, err := sandbox.PipInstall(ctx, "six==1.17.0")
	if err != nil {
		t.Fatal(err)
	}
	if installed.ExitCode != 0 || installed.Error != "" {
		t.Fatalf("pip result = %+v", installed)
	}
	result, err := sandbox.RunCode(ctx, "import six; six.__version__")
	if err != nil || result.Error != nil {
		t.Fatalf("import six: python error=%+v err=%v", result.Error, err)
	}
	if result.Text() != "'1.17.0'" {
		t.Fatalf("six version = %q", result.Text())
	}
}

// TestEmbeddedPipInstallsWithDependenciesFromPyPI installs a package that pulls
// real transitive dependencies.
//
// The six test above is not a substitute. six has no dependencies, and a guest
// that handles it can still fault partway through a larger install: a rebuilt
// component once passed every other test here while dying inside CPython's
// garbage collector on exactly this path, with "indirect call type mismatch".
// Install volume, not network access, is what exercises it.
func TestEmbeddedPipInstallsWithDependenciesFromPyPI(t *testing.T) {
	if os.Getenv("PYCAGE_NETWORK_TESTS") != "1" {
		t.Skip("set PYCAGE_NETWORK_TESTS=1 to run the live PyPI test")
	}
	ctx := context.Background()
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	config.AllowNetwork = true
	config.Timeout = 180 * time.Second
	sandbox, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	// requests brings urllib3, idna, certifi, and charset-normalizer with it.
	installed, err := sandbox.PipInstall(ctx, "requests==2.32.4")
	if err != nil {
		t.Fatalf("pip install: %v", err)
	}
	if installed.ExitCode != 0 || installed.Error != "" {
		t.Fatalf("pip result = %+v", installed)
	}

	result, err := sandbox.RunCode(ctx, "import requests, urllib3, certifi, idna\nrequests.__version__")
	if err != nil || result.Error != nil {
		t.Fatalf("import requests and its dependencies: python error=%+v err=%v", result.Error, err)
	}
	if result.Text() != "'2.32.4'" {
		t.Fatalf("requests version = %q", result.Text())
	}
}
