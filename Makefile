.PHONY: all build bindings guest test bench clean

COMPONENTIZE_PY_VERSION := 0.19.1
# guest/app.py imports pip._internal at module scope, so componentize-py bundles
# whatever pip it can resolve at build time into python.wasm. That pip is the
# guest's, never the host's, so pin it here instead of inheriting whichever
# version `python3 -m venv` happened to seed.
PIP_VERSION := 24.0

all: build

.venv/bin/componentize-py:
	python3 -m venv .venv
	.venv/bin/pip install "pip==$(PIP_VERSION)" "componentize-py==$(COMPONENTIZE_PY_VERSION)"

bindings: .venv/bin/componentize-py
	set -e; \
	bindings_dir=$$(mktemp -d /tmp/pycage-bindings.XXXXXX); \
	trap 'rm -rf -- "$$bindings_dir"' EXIT; \
	.venv/bin/componentize-py -d wit -w python-sandbox bindings "$$bindings_dir"; \
	cp -R "$$bindings_dir"/. guest/
	sed -i '$${/^$$/d;}' guest/wit_world/exports/__init__.py

# Do not add site-packages as a second -p. It looks like the fix for
# "ModuleNotFoundError: No module named 'pip'" on a machine where pip is not
# ambient, and it does resolve that -- but -p means "bundle this tree
# wholesale", not "search here". The component grows from 57 MB to 64 MB, and
# the oversized result traps inside CPython's garbage collector partway through
# a real multi-package install:
#
#   wasm error: indirect call type mismatch
#   libpython3.14.so.gc_collect_region -> _PyGC_Collect
#
# It passes every test in the suite while doing so, because those install one
# small local wheel. See TestEmbeddedPipInstallsWithDependenciesFromPyPI.
guest: bindings
	.venv/bin/componentize-py -d wit -w python-sandbox componentize -p guest app -o guest/python.wasm
	gzip -9 -n -c guest/python.wasm > guest/python.wasm.gz

build: guest
	mkdir -p bin
	go build -o bin/pycage ./cmd/pycage

test: guest
	go test ./...

bench: guest
	go test -run '^$$' -bench . -benchtime=1x -benchmem .

clean:
	go clean
