package pycage

import (
	"context"
	"os"
	"testing"
	"time"
)

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
