.PHONY: all build test lint sec vuln check run validate image

# -race needs cgo (a C compiler); fall back to plain tests when unavailable.
RACE ?= $(if $(shell command -v gcc),-race,)
VERSION ?= dev
IMAGE ?= courier:$(VERSION)

all: check

build:
	go build ./...

test:
	go test $(RACE) -cover ./...

lint:
	golangci-lint run

sec:
	gosec ./...

vuln:
	govulncheck ./...

check: build lint test sec vuln

validate:
	go run ./cmd/courier validate -c config.example.yaml

run:
	go run ./cmd/courier serve -c config.example.yaml

image:
	podman build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE) .
