SERVICES := smpp-server http-api admin-api router rater dlr-processor connector migrate smpp-sim devseed
COMPOSE  := docker compose -f deploy/docker-compose.yml

.PHONY: build test lint fmt migrate up down logs images dev-up dev-down load

build:
	go build -o bin/ ./cmd/...

# Integration tests need Redis (REDIS_TEST_ADDR) and Postgres (POSTGRES_TEST_URL);
# they skip when those are not reachable.
test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	golangci-lint run

fmt:
	gofmt -w .

migrate:
	go run ./cmd/migrate

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

images:
	@for s in $(SERVICES); do docker build --build-arg SERVICE=$$s -t amaze-sms/$$s:dev . || exit 1; done

# Without Docker: in-memory Kafka + local Postgres/Redis (see scripts/dev-local.sh).
dev-up:
	scripts/dev-local.sh up

dev-down:
	scripts/dev-local.sh down

load:
	scripts/dev-local.sh load
