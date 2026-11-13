# Adding a dataset

1. Add an explicit typed `SELECT` to the `datasets` registry in `internal/app/app.go`.
2. Include a tenant predicate and stable ordering. Cast UUID and exact numeric columns to text so JSON preserves their canonical values.
3. Add any required tenant-aware foreign keys and indexes in a new migration.
4. Add export-format and verifier coverage for record counts, ownership, and relationship expectations.
5. Update documentation and fixtures to demonstrate the dataset.

Dataset queries are registered in application code and reviewed with their ownership and relationship rules. API requests must never provide table or column names.
