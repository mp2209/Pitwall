.PHONY: build test lint run mock smoke up down bench-check bench-dry bench-report

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

# Benchmark (see bench/README.md). bench-dry needs a gateway with a database
# and PITWALL_ADMIN_TOKEN, plus the mock provider.
bench-check:
	python3 bench/check_tasks.py

bench-dry: build
	python3 bench/run.py --plan bench/plans/dry-run.json --out bench/results/dry-run --jobs 4
	python3 bench/analyze.py bench/results/dry-run

# make bench-report DIR=bench/results/<plan>
bench-report:
	python3 bench/analyze.py $(DIR)
