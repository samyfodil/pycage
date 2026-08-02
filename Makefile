.PHONY: all build guest test bench clean

GOCACHE := $(CURDIR)/.cache/go-build

all: build

.venv/bin/componentize-py:
	python3 -m venv .venv
	.venv/bin/pip install componentize-py==0.19.1

guest: .venv/bin/componentize-py
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
