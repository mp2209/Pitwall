.PHONY: build test lint run mock smoke up down

build:
	go build -o bin/ ./cmd/...

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

# Run the gateway and mock provider locally (two terminals, or `make mock &`).
run: build
	bin/gateway

mock: build
	bin/mockprovider

# Needs `pip install openai anthropic` and a running gateway + mock.
smoke:
	python3 scripts/sdk_smoke.py

up:
	docker compose up -d --build

down:
	docker compose down
