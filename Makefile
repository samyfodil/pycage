.PHONY: all build bindings guest test bench clean

GOCACHE := $(CURDIR)/.cache/go-build

all: build

.venv/bin/componentize-py:
	python3 -m venv .venv
	.venv/bin/pip install componentize-py==0.19.1

bindings: .venv/bin/componentize-py
	rm -rf $(CURDIR)/.cache/componentize-bindings
	.venv/bin/componentize-py -d wit -w python-sandbox bindings .cache/componentize-bindings
	cp -R .cache/componentize-bindings/. guest/
	sed -i '$${/^$$/d;}' guest/wit_world/exports/__init__.py

guest: bindings
	.venv/bin/componentize-py -d wit -w python-sandbox componentize -p guest app -o guest/app.wasm

build: guest
	mkdir -p bin
	GOCACHE=$(GOCACHE) go build -mod=vendor -o bin/pycage ./cmd/pycage

test: guest
	GOCACHE=$(GOCACHE) go test -mod=vendor ./...

bench: guest
	GOCACHE=$(GOCACHE) go test -mod=vendor -run '^$$' -bench . -benchtime=1x -benchmem .

clean:
	go clean
