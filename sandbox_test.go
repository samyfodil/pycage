package pycage

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestEngineSandboxesAreIsolated(t *testing.T) {
	ctx := context.Background()
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	engine, err := NewEngine(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(ctx)

	first, err := engine.NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(ctx)
	second, err := engine.NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(ctx)

	if _, err := first.RunCode(ctx, "secret = 42"); err != nil {
		t.Fatal(err)
	}
	result, err := second.RunCode(ctx, `globals().get("secret", "missing")`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text() != "'missing'" {
		t.Fatalf("second sandbox observed first sandbox state: %q", result.Text())
	}
}

func TestNewEngineRejectsUnknownRuntimeMode(t *testing.T) {
	config := DefaultConfig()
	config.RuntimeMode = "turbo"
	if _, err := NewEngine(context.Background(), config); err == nil {
		t.Fatal("expected unknown runtime mode error")
	}
}

func TestSandboxStateResetAndErrors(t *testing.T) {
	ctx := context.Background()
	sandbox, err := New(ctx, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	first, err := sandbox.RunCode(ctx, "x = 40")
	if err != nil || first.Error != nil {
		t.Fatalf("set state: execution=%+v err=%v", first, err)
	}
	second, err := sandbox.RunCode(ctx, "x + 2")
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Text(); got != "42" {
		t.Fatalf("text = %q, want 42", got)
	}

	failed, err := sandbox.RunCode(ctx, "1 / 0")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Error == nil || failed.Error.Name != "ZeroDivisionError" {
		t.Fatalf("error = %+v, want ZeroDivisionError", failed.Error)
	}

	if err := sandbox.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	reset, err := sandbox.RunCode(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if reset.Error == nil || reset.Error.Name != "NameError" {
		t.Fatalf("error after reset = %+v, want NameError", reset.Error)
	}
}

func TestSandboxFilesAndPureWheel(t *testing.T) {
	ctx := context.Background()
	sandbox, err := New(ctx, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	if err := sandbox.WriteFile("input.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	read, err := sandbox.RunCode(ctx, `open("/input.txt").read().upper()`)
	if err != nil || read.Error != nil {
		t.Fatalf("read file: execution=%+v err=%v", read, err)
	}
	if got := read.Text(); got != "'HELLO'" {
		t.Fatalf("text = %q, want 'HELLO'", got)
	}

	info, err := sandbox.InstallWheel(testWheel(t))
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "greeter" || info.Version != "1.0.0" {
		t.Fatalf("wheel info = %+v", info)
	}
	imported, err := sandbox.RunCode(ctx, "import greeter; greeter.hello('Wazy')")
	if err != nil || imported.Error != nil {
		t.Fatalf("import wheel: execution=%+v python-error=%+v err=%v", imported, imported.Error, err)
	}
	if got := imported.Text(); got != "'Hello, Wazy!'" {
		t.Fatalf("text = %q, want greeting", got)
	}
}

func TestEmbeddedPipInstallsWheelAtRuntime(t *testing.T) {
	ctx := context.Background()
	config := DefaultConfig()
	config.RuntimeMode = RuntimeModeInterpreter
	config.Timeout = 30 * time.Second
	sandbox, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	const wheelPath = "/packages/greeter-1.0.0-py3-none-any.whl"
	if err := sandbox.WriteFile(wheelPath, testWheel(t)); err != nil {
		t.Fatal(err)
	}
	installed, err := sandbox.PipInstall(ctx, "--no-index", "--no-deps", wheelPath)
	if err != nil {
		t.Fatal(err)
	}
	if installed.ExitCode != 0 || installed.Error != "" {
		t.Fatalf("pip result = %+v", installed)
	}
	result, err := sandbox.RunCode(ctx, "import greeter; greeter.hello('pip')")
	if err != nil || result.Error != nil {
		t.Fatalf("import installed wheel: python error=%+v err=%v", result.Error, err)
	}
	if result.Text() != "'Hello, pip!'" {
		t.Fatalf("installed package result = %q", result.Text())
	}
	resource, err := sandbox.RunCode(ctx, "greeter.resource()")
	if err != nil || resource.Error != nil {
		t.Fatalf("read installed package resource: python error=%+v err=%v", resource.Error, err)
	}
	if resource.Text() != "'package data works'" {
		t.Fatalf("installed package resource = %q", resource.Text())
	}
}

func TestSandboxTimeoutRetiresInstance(t *testing.T) {
	ctx := context.Background()
	config := DefaultConfig()
	config.Timeout = 25 * time.Millisecond
	sandbox, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.Close(ctx) })

	_, err = sandbox.RunCode(ctx, "while True: pass")
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timeout error = %v", err)
	}
	if _, err := sandbox.RunCode(ctx, "1 + 1"); err != ErrClosed {
		t.Fatalf("reuse error = %v, want ErrClosed", err)
	}
}

func testWheel(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	files := map[string]string{
		"greeter/__init__.py":              "from importlib.resources import files\ndef hello(name):\n    return f'Hello, {name}!'\ndef resource():\n    return files(__package__).joinpath('message.txt').read_text().strip()\n",
		"greeter/message.txt":              "package data works\n",
		"greeter-1.0.0.dist-info/WHEEL":    "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"greeter-1.0.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: greeter\nVersion: 1.0.0\n",
		"greeter-1.0.0.dist-info/RECORD":   "",
	}
	for name, contents := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
