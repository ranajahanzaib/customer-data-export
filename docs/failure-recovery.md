# Failure and recovery

- A worker claims a queued job or a running job with an expired lease. It increments the attempt count and gets a unique lease token. A heartbeat extends the lease once a minute.
- On an ordinary processing error, only the current lease owner can return the job to queued (while attempts remain) or mark it failed. A stale worker cannot publish because the final update requires the same lease token.
- A process crash leaves a running lease. Another worker can reclaim it after five minutes. Jobs at their attempt limit are marked failed when the worker loop next runs.
- PostgreSQL failures abort collection and leave no available job. S3 GET failures, missing objects, size mismatches, and hash mismatches fail the attempt without publishing.
- The archive is locally staged, verified, uploaded, then published in PostgreSQL. If publication loses its lease, the S3 artifact is deleted. A process crash between upload and publication can leave an unreferenced object; orphan-object cleanup is not supported.
- Cancellation clears the lease and prevents final publication. The worker notices cancellation at dataset boundaries; a currently running object read may finish first.
- Expiration deletes up to 25 expired artifacts per worker loop and changes their jobs to expired only after successful deletion. If deletion fails, the job remains available in the database until a later retry.
