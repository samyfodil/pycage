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

guest: bindings
	# site-packages is on the module path explicitly: without it componentize-py
	# resolves the guest's `import pip._internal` only when the ambient
	# environment happens to expose pip, which fails on a clean machine.
	.venv/bin/componentize-py -d wit -w python-sandbox componentize \
		-p guest \
		-p "$$(.venv/bin/python -c 'import sysconfig; print(sysconfig.get_paths()["purelib"])')" \
		app -o guest/python.wasm
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
