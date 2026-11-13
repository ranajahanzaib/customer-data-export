# Contributing

Contributions that improve correctness, tenant isolation, verification, reliability, documentation, or the local development experience are welcome.

## Development setup

```sh
git clone https://github.com/ranajahanzaib/customer-data-export.git
cd customer-data-export
make setup
make up
make migrate
make seed
```

Run `make test` before submitting a change. Changes to PostgreSQL or MinIO behavior should also run `make test-integration` against the Compose services. `make down` stops the services while preserving their development volumes.

## Pull requests

- Keep changes focused and describe the behavior they change.
- Add or update tests for behavior changes.
- Document API, schema, configuration, and archive-format changes.
- Do not commit credentials, local `.env` files, exported customer data, or generated archives.
- Report which checks ran and identify any checks that require unavailable infrastructure.

Pull requests are reviewed for correctness, tenant isolation, safe failure behavior, and clear documentation. A pull request may receive requests for changes before it is merged.

## Reporting issues

Use the repository issue forms for reproducible bugs and feature proposals. Do not include customer data, credentials, or private attachment contents in an issue.
