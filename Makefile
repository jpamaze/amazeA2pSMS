SERVICES := smpp-server http-api admin-api router rater dlr-processor connector migrate
COMPOSE  := docker compose -f deploy/docker-compose.yml

.PHONY: build test test-short lint fmt migrate up down logs images

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
