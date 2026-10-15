VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test cover lint docker up down clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/uptimer ./cmd/uptimer

run: build
	./bin/uptimer -config config.example.json -log-level debug

test:
	go test -race -count=1 ./...

cover:
	go test -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -1

lint:
	gofmt -l . | (! grep .) || (echo "gofmt needed on the files above" && exit 1)
	go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t uptimer:$(VERSION) .

up:
	docker compose up --build

down:
	docker compose down

clean:
	rm -rf bin coverage.out
