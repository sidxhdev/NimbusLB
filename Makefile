.PHONY: build test run compose compose-down vet

build:
	go build -o bin/nimbuslb ./cmd/nimbuslb
	go build -o bin/backend ./cmd/backend

test:
	go test -race ./...

vet:
	go vet ./...

run:
	go run ./cmd/nimbuslb -config config.yaml

compose:
	docker compose -f deployments/docker-compose.yml up --build

compose-down:
	docker compose -f deployments/docker-compose.yml down
