.PHONY: demo api worker migrate test test-console integration fmt vet build setup-compose compose-check up down observe observe-down load
demo:
	go run ./cmd/forge -mode demo
api:
	go run ./cmd/forge -mode api
worker:
	go run ./cmd/forge -mode worker
migrate:
	go run ./cmd/forge -mode migrate
test:
	go test -race ./...
test-console:
	node --test internal/console/model_test.mjs
integration:
	go test -race -tags integration ./internal/store
fmt:
	gofmt -w cmd internal
vet:
	go vet ./...
build:
	go build -o bin/forge ./cmd/forge
	go build -o bin/forge-load ./cmd/forge-load
setup-compose:
	python3 scripts/configure-compose.py
compose-check:
	docker compose config --quiet
	docker compose -f compose.yaml -f compose.observability.yaml config --quiet
up:
	docker compose up --build -d --scale worker=10
down:
	docker compose down
observe:
	docker compose -f compose.yaml -f compose.observability.yaml up --build -d --scale worker=10
observe-down:
	docker compose -f compose.yaml -f compose.observability.yaml down
load:
	go run ./cmd/forge-load -tasks 2200
