.PHONY: demo api worker test integration fmt vet build up down load
demo:
	go run ./cmd/forge -mode demo
api:
	go run ./cmd/forge -mode api
worker:
	go run ./cmd/forge -mode worker
test:
	go test -race ./...
integration:
	go test -race -tags integration ./internal/store
fmt:
	gofmt -w cmd internal
vet:
	go vet ./...
build:
	go build -o bin/forge ./cmd/forge
	go build -o bin/forge-load ./cmd/forge-load
up:
	docker compose up --build -d --scale worker=10
down:
	docker compose down
load:
	go run ./cmd/forge-load -tasks 2200
