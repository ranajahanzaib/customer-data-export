.PHONY: setup up down migrate seed build test test-integration test-security test-e2e demo clean
setup:
	cp -n .env.example .env || true
	go mod download
up:
	docker compose up -d --build
down:
	docker compose down
migrate:
	@db="$${DATABASE_URL:-postgres://export:export_dev_only@localhost:5432/customer_export?sslmode=disable}"; psql "$$db" -v ON_ERROR_STOP=1 -c 'CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())'; if ! psql "$$db" -Atqc "SELECT 1 FROM schema_migrations WHERE version='001_init'" | grep -q 1; then psql "$$db" -v ON_ERROR_STOP=1 -f migrations/001_init.sql && psql "$$db" -v ON_ERROR_STOP=1 -c "INSERT INTO schema_migrations(version) VALUES ('001_init')"; else echo 'migrations are current'; fi
seed:
	psql "$${DATABASE_URL:-postgres://export:export_dev_only@localhost:5432/customer_export?sslmode=disable}" -v ON_ERROR_STOP=1 -f fixtures/seed.sql
build:
	go build ./cmd/api ./cmd/worker ./cmd/exportctl
test:
	go test ./...
test-integration:
	RUN_INTEGRATION=1 go test ./tests/integration/...
test-security: test
	RUN_INTEGRATION=1 go test ./tests/integration/...
test-e2e: demo
demo: build
	./scripts/demo.sh
clean:
	rm -rf artifacts
