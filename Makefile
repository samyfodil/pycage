.PHONY: all build bindings guest test bench clean

all: build

.venv/bin/componentize-py:
	python3 -m venv .venv
	.venv/bin/pip install componentize-py==0.19.1

bindings: .venv/bin/componentize-py
	set -e; \
	bindings_dir=$$(mktemp -d /tmp/pycage-bindings.XXXXXX); \
	trap 'rm -rf -- "$$bindings_dir"' EXIT; \
	.venv/bin/componentize-py -d wit -w python-sandbox bindings "$$bindings_dir"; \
	cp -R "$$bindings_dir"/. guest/
	sed -i '$${/^$$/d;}' guest/wit_world/exports/__init__.py

guest: bindings
	.venv/bin/componentize-py -d wit -w python-sandbox componentize -p guest app -o guest/app.wasm

build: guest
	mkdir -p bin
	go build -o bin/pycage ./cmd/pycage

test: guest
	go test ./...

bench: guest
	go test -run '^$$' -bench . -benchtime=1x -benchmem .

clean:
	go clean
