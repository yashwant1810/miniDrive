include .env
export

.PHONY: build run test migrate seed reset clean up down logs ps

# Build both binaries
build:
	go build -o bin/minidrive-server ./cmd/server
	go build -o bin/minidrive ./cmd/cli

# Run API server
run:
	go run ./cmd/server

# Run tests
test:
	go test ./...

# Run migrations
migrate:
	goose -dir migrations postgres "$(DATABASE_URL)" up

# Seed test data
seed:
	psql "$(DATABASE_URL)" -f scripts/seed.sql

# Reset DB (drop + migrate + seed)
reset:
	psql "$(DATABASE_URL)" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
	$(MAKE) migrate
	$(MAKE) seed

# Docker
up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f

ps:
	docker compose ps

# Clean build artifacts
clean:
	rm -rf bin/
