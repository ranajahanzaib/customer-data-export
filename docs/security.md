# Security notes

## Controls present

- The tenant ID is taken from a verified token and resolved with a membership lookup, not from request JSON.
- Every export endpoint filters by the authenticated tenant. Administrator role is required for list, status, request, cancellation, manifest, and download operations.
- Foreign-key constraints include tenant IDs for core relationships. A trigger validates attachment owner type and tenant. Object keys must be prefixed by the tenant UUID.
- Attachment downloads can pin an S3 version and must match database size and SHA-256. Failed checks prevent publication.
- Presigned downloads last 60 seconds. They are bearer links and can be used by anyone who obtains them until expiry.
- Export ZIPs are not published as available until verification and S3 upload succeed. Per-attempt random lease tokens fence stale workers.
- Development uses a fixed local HS256 secret. Production startup rejects that known secret and requires explicit issuer and audience.

## Gaps and residual risks

The service does not support JWKS rotation, rate limiting, PostgreSQL row-level security, object lock, immutable versioning policy, managed key distribution, comprehensive audit coverage, or disk-isolated staging. The API listener uses plain HTTP; production deployments require a TLS-terminating proxy. HMAC key distribution is the operator's responsibility. The SigV4 client has not been validated against AWS or MinIO in the current test suite. Tenant-isolation and attachment-corruption scenarios require additional PostgreSQL and object-store tests before production use. Export quotas are not enforced. `activity_events.metadata` is exported as supplied and should not contain credentials or secrets.
