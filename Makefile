.PHONY: build run clean vet test docker-up docker-down

build:
	@go build -o bin/api ./cmd/api

run: build
	@./bin/api

test:
	@go test -v -race ./...

clean:
	@rm -rf bin

vet:
	@go vet ./...

docker-up:
	@docker compose up -d

docker-down:
	@docker compose down

migrate-up:
	@go run ./cmd/migrate up

migrate-down:
	@go run ./cmd/migrate down

migrate-reset:
	@go run ./cmd/migrate reset

migrate-create:
	@migrate create -ext sql -dir migrations -seq $(name)

migrate-force:
	@go run ./cmd/migrate force $(version)