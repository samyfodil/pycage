# pycage

`pycage` is an embedded, E2B-like Python sandbox for AI-generated code. It
runs real CPython as a WebAssembly Component inside
[Wazy](https://github.com/samyfodil/wazy): no Python process, container, CGO,
or remote sandbox service is needed at runtime.

```console
$ pycage run 'print("hello from CPython"); sum(i*i for i in range(10))'
hello from CPython
285

$ pycage run -timeout 50ms 'while True: pass'
pycage: execute: ... context deadline exceeded
```

## What works

- Stateful cells: variables and imports survive between `RunCode` calls.
- Captured stdout, stderr, rich text/HTML/SVG/JSON results, and tracebacks.
- Host-enforced execution deadlines and WebAssembly memory limits.
- An isolated in-memory filesystem with no host paths exposed.
- Pure-Python wheel installation with native files, path traversal, oversized
  archives, and non-`none-any` wheels rejected.
- Embedded pip 25.1.1 with runtime PyPI downloads, dependency prefetching, and
  package-resource support for pure-Python wheels.
- Network access denied by default.
- A typed WIT boundary between the Go host and CPython guest.

The project deliberately does not provide Bash, subprocesses, native wheels,
source distributions, `apt`, or console-script execution.

## Build

Building the guest requires Python 3.10+ and creates a project-local virtual
environment. Running the resulting binary does not require Python.

```console
make build
./bin/pycage run '6 * 7'
```

The CLI runs Wazy's native compiler by default and persists compiled core Wasm
modules under `${TMPDIR:-/tmp}/pycage/wazy-native`. The cache is content- and
Wazy-version-keyed, so subsequent processes skip native compilation. Override
its location with `-cache-dir`.

Interpreter mode remains an explicit opt-in when an empty-cache cold start
matters more than warm execution speed:

```console
./bin/pycage run -runtime interpreter -timing 'print("hello"); 6 * 7'
```

Use compiler mode (the default) in a long-running Go process and reuse an
`Engine`, as shown below. See [the benchmark notes](docs/benchmarks.md) for the
measured tradeoffs and profiling results.

The componentized CPython guest is about 56 MB and is embedded into the Go
binary. `guest/app.wasm`, `.venv`, and `bin` are generated locally and ignored
by Git.

## Go API

For a service or agent runtime, keep one Engine alive so component decoding and
native compilation happen once:

```go
engine, err := pycage.NewEngine(ctx, pycage.DefaultConfig())
if err != nil {
    log.Fatal(err)
}
defer engine.Close(ctx)

sandbox, err := engine.NewSandbox(ctx)
defer sandbox.Close(ctx)
```

Each Engine-created sandbox has independent Python globals and files. Only the
immutable decoded component and compiled native code are shared.

For a single sandbox, the convenience API owns its private Engine:

```go
ctx := context.Background()
sandbox, err := pycage.New(ctx, pycage.DefaultConfig())
if err != nil {
    log.Fatal(err)
}
defer sandbox.Close(ctx)

first, _ := sandbox.RunCode(ctx, "x = 40")
second, _ := sandbox.RunCode(ctx, "x + 2")
fmt.Println(second.Text()) // 42
```

Files are isolated from the host:

```go
sandbox.WriteFile("input.txt", []byte("hello"))
result, _ := sandbox.RunCode(ctx, `open("/input.txt").read()`)
```

Install a downloaded, pinned wheel after verifying its hash in your
application:

```go
wheel, _ := os.ReadFile("greeter-1.0.0-py3-none-any.whl")
info, err := sandbox.InstallWheel(wheel)
result, _ := sandbox.RunCode(ctx, "import greeter; greeter.hello()")
```

`InstallWheel` validates wheel metadata and loads `.py` modules through an
in-memory importer inside the guest.

Embedded pip can also install a pinned package and its pure-Python dependencies
from PyPI at runtime. Network access is an explicit capability:

```go
config := pycage.DefaultConfig()
config.AllowNetwork = true
sandbox, _ := pycage.New(ctx, config)
installed, err := sandbox.PipInstall(ctx, "requests==2.32.4")
result, _ := sandbox.RunCode(ctx, "import requests; requests.__version__")
```

The CLI equivalent is:

```console
./bin/pycage run -timeout 60s -network -pip 'requests==2.32.4' \
  'import requests; requests.__version__'
```

Because WASI CPython has no TLS module, the trusted Go host fetches metadata
from `pypi.org` and wheels from `files.pythonhosted.org`, verifies PyPI's
SHA-256 digest, and places them in an isolated wheelhouse. Embedded pip performs
the wheel installation. Downloads remain disabled unless `AllowNetwork` or
`-network` is set.

Only pure `*-none-any.whl` artifacts are accepted. Source builds, native files,
and console entry points are rejected or stripped because the sandbox has no
subprocess or native extension support. Exact version pins are strongly
recommended; the lightweight dependency prefetcher selects the latest release
for non-exact ranges before pip installs the resulting wheel set.

## WIT interface

The guest currently exports three operations:

```wit
interface code-interpreter {
    run-code: func(code: string) -> string;
    install-modules: func(modules: string) -> string;
    reset: func() -> string;
}
```

The Go layer provides the E2B-like lifecycle, files, package validation,
timeouts, and memory policy. Keeping those controls outside the guest prevents
untrusted Python from weakening them.

## Wazy compatibility

The repository vendors its pinned Wazy revision. Running a real
`componentize-py` component uncovered several Component Model graph shapes not
handled by that revision. The focused compatibility changes are documented in
[`docs/wazy-compatibility.md`](docs/wazy-compatibility.md) and live in the
vendored package so this checkout builds reproducibly.

## Security status

This is an early prototype, not yet a hardened security boundary. Before
running hostile workloads in production, fuzz the WIT inputs, audit every WASI
capability, enforce process-level resource limits around the Go host, and move
the vendored compatibility changes into upstream Wazy with conformance tests.
