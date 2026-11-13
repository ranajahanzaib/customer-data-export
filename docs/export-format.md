# Export format v1

The ZIP contains one `data/<dataset>.jsonl` file for each registered dataset, a binary file under `attachments/<attachment UUID>/<safe filename>`, and `manifest.json`.

The JSON manifest contains `version` (currently 1), `tenant_id`, `created_at`, `record_counts` keyed by dataset, and `files` mapping every non-manifest path to its SHA-256 hex digest. Each JSONL line is one UTF-8 JSON object. UUIDs and exact PostgreSQL numeric values are serialized as strings; timestamps use Go's RFC 3339 JSON representation. Attachment metadata includes the archive-relative `file` path, source size and SHA-256; internal storage keys are excluded.

Importers should reject unsupported versions, undeclared or missing files, duplicate paths, unsafe paths, digest mismatches, cross-tenant rows, invalid JSONL, or count mismatches. The `exportctl verify` command performs those structural, checksum, tenant, count, and registered relationship checks. It does not restore or import data.
