DSN ?= postgres://tender:tender@localhost:5432/tender?sslmode=disable

.PHONY: up down migrate build test lint tidy psql

## up: start Postgres and wait for it
up:
	docker compose up -d --wait
	@echo "TENDER_DSN=$(DSN)"

## down: stop Postgres and keep the data
down:
	docker compose down

## migrate: apply pending migrations
migrate: build
	TENDER_DSN="$(DSN)" ./bin/tenderctl migrate

## build: compile the CLI
build:
	go build -o bin/ ./cmd/...

## test: run everything, including the Postgres-backed tests
test:
	TENDER_TEST_DSN="$(DSN)" go test -race -count=1 ./...

## lint: vet and golangci-lint
lint:
	go vet ./...
	golangci-lint run

## tidy: sync go.mod and go.sum
tidy:
	go mod tidy

## psql: open a shell on the dev database
psql:
	docker compose exec postgres psql -U tender -d tender
