# Contributing

Thanks for helping improve pycage.

## Development setup

Install Go 1.26+, Python 3.10+, and GNU Make, then run:

```console
make test
```

The build creates a local `.venv`, generates Python WIT bindings, builds the
componentized CPython guest, and runs the Go test suite. Use `make bench` for
the startup and warm-call benchmarks.

## Pull requests

- Keep networking and host filesystem access denied by default.
- Add regression tests for behavior changes.
- Run `gofmt` on Go changes and `make test` before submitting.
- Do not vendor Go modules or patch Wazy inside this repository. Required Wazy
  changes should be proposed upstream first.
- Keep generated `.venv`, `guest/app.wasm`, and `bin/` artifacts out of commits.

For security-sensitive reports, follow [SECURITY.md](SECURITY.md) instead of
opening a public issue.
