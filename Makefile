IMAGE ?= ghcr.io/autonoma-ai/gatekeeper:latest

.PHONY: build test vet fmt fmt-check tidy docker run all

all: fmt-check vet test build

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy

docker:
	docker build -t $(IMAGE) .

# Build the binary into ./bin for local inspection.
run:
	go build -o bin/gatekeeper ./cmd/gatekeeper
