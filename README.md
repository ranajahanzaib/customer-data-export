# Customer Data Export

Export, verify, and securely deliver tenant-owned SaaS records and attachments without exposing another customer’s data.

Customer Data Export is a Go reference service for an explicitly declared PostgreSQL schema. Tenant administrators request asynchronous exports. Workers collect records from a repeatable-read snapshot, retrieve and checksum attachment objects, create a ZIP archive, verify its contents, and publish it to S3-compatible storage.

## Features

- Tenant-scoped export APIs with administrator authorization and idempotent requests.
- Durable PostgreSQL jobs, worker leases, heartbeat renewal, retries, and cancellation.
- Tenant-aware database constraints and attachment ownership checks.
- JSONL datasets, binary attachments, a versioned manifest, and SHA-256 integrity checks.
- Offline archive verification for tenant ownership, record counts, known relationships, archive paths, and checksums.
- S3-compatible storage, optional version-pinned attachment reads, short-lived presigned downloads, and artifact expiration.
- A small dashboard and CLI backed by the API.

## Quick start

Requirements: Go 1.27 or newer, Docker Compose, and `psql`.

```sh
git clone https://github.com/ranajahanzaib/customer-data-export.git
cd customer-data-export
cp .env.example .env
docker compose up -d --build
export DATABASE_URL='postgres://export:export_dev_only@localhost:5432/customer_export?sslmode=disable'
make migrate
make seed
make demo
```

The local stack includes PostgreSQL, MinIO, the API, and a worker. The dashboard is at [http://localhost:8080/dashboard](http://localhost:8080/dashboard). Seed data contains three fictional tenants and sample attachment objects.

Create a one-hour development token for the seeded Acme administrator:

```sh
export EXPORT_TOKEN="$(JWT_HMAC_SECRET=local-development-signing-secret-not-for-production go run ./cmd/exportctl token 20000000-0000-4000-8000-000000000001 10000000-0000-4000-8000-000000000001)"
go run ./cmd/exportctl request
go run ./cmd/exportctl list
go run ./cmd/exportctl status EXPORT_ID
go run ./cmd/exportctl download EXPORT_ID
go run ./cmd/exportctl verify EXPORT_ID.zip
```

The fixed credentials in `.env.example` and Docker Compose are for local development only. The application rejects the default signing secret when `APP_ENV=production`.

## API

All export routes use a signed bearer token whose subject and tenant are resolved against active PostgreSQL membership:

- `POST /api/v1/exports` — queue an export; accepts `Idempotency-Key`.
- `GET /api/v1/exports` — list the current tenant’s jobs with limit and offset pagination.
- `GET /api/v1/exports/{id}` — inspect job status and verification details.
- `POST /api/v1/exports/{id}/cancel` — cancel a queued or active job.
- `GET /api/v1/exports/{id}/manifest` — retrieve the verified manifest.
- `GET /api/v1/exports/{id}/download` — receive a short-lived presigned download URL.

Tenant administrators can use export operations. Tenant members cannot request or access full-tenant exports. Requests do not accept a tenant ID; the authenticated membership determines the tenant.

## Archive format

The ZIP contains a `data/<dataset>.jsonl` file for each registered dataset, attachment binaries under `attachments/<attachment-id>/`, and `manifest.json`. The manifest records the format version, tenant ID, per-dataset counts, and SHA-256 hashes for all data files. UUID and exact numeric values are serialized as strings. See [the export format specification](docs/export-format.md) for details.

Run `exportctl verify ARCHIVE.zip` to check archive safety, file hashes, JSONL validity, tenant IDs, record counts, and supported relational references. Verification does not restore or import data.

## Development and tests

```sh
make setup
make up
make migrate
make seed
make test
```

With PostgreSQL and MinIO running and seeded, `make test-integration` checks tenant-aware database constraints, API authorization, cross-tenant status access, and the seeded object checksum. `make test-e2e` runs the export demonstration. `make down` stops the local services without deleting persistent volumes.

GitHub Actions is configured to run formatting checks, `go vet`, race-enabled tests, builds, and PostgreSQL/MinIO integration checks.

## Project documentation

- [Architecture](docs/architecture.md)
- [Security model and limitations](docs/security.md)
- [Export format](docs/export-format.md)
- [Failure recovery](docs/failure-recovery.md)
- [Adding a dataset](docs/adding-a-dataset.md)

## Limitations

- Authentication supports configured HS256 signing keys; JWKS rotation and OIDC discovery are not implemented.
- The S3-compatible client uses an in-repository SigV4 implementation. MinIO and AWS compatibility must be validated in an environment with the object store available.
- Export staging is memory-backed and limited to 128 MiB, with a 128 MiB archive limit. Large exports require disk-based streaming before production use.
- Rate limiting, export quotas, Prometheus metrics, PostgreSQL row-level security, object lock, orphan-object cleanup, and configurable retention are not implemented.
- Integration tests are present, but worker crash injection, concurrent snapshot mutation, attachment corruption scenarios, large-resource tests, and full end-to-end execution require further coverage.
- The API listener uses plain HTTP; production deployments need a TLS-terminating proxy.

The current implementation is suitable for local evaluation and development. Production deployment requires the additional controls and verification described in [the security documentation](docs/security.md).
