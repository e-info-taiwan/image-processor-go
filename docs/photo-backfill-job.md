# Production photo backfill Job

`AI_BACKFILL_JOB=photos` selects a bounded, single-worker batch process. It never
starts the HTTP server and never uploads, replaces, or deletes GCS objects.
`BACKFILL_MODE=check` is the default. Check mode performs read-only DB schema
checks, without starting CLIP or calling Vision. Apply mode requires the Photo
label/vector-status migrations, including nullable failure-reason columns.

The Job fills only missing pHash, image vectors and Vision label suggestions.
Each component is written independently. It does not connect formal Photo tags,
rebuild legacy `possibleDuplicates`, or overwrite editorial timestamps. pHash
is computed from the original using the online EXIF-orientation and w480 resize
algorithm. Vector/labels reuse w480 when pHash is already present, with fallback
to the original. A failed component stays eligible on the next execution.

Configuration:

| Variable | Default |
| --- | --- |
| `BACKFILL_MODE` | `check`; explicitly override to `apply` |
| `DATABASE_URL` | required; Secret Manager reference |
| `BACKFILL_EXPECTED_DATABASE` | `eic-prod` |
| `IMAGE_BUCKET` | required for apply |
| `ENABLE_IMAGE_VECTOR`, `ENABLE_IMAGE_LABEL` | both required for apply |
| `BACKFILL_MAX_ITEMS` | 50 per execution |
| `BACKFILL_BATCH_SIZE` | 25 per DB read |
| `BACKFILL_ATTEMPTS` | 3 per transient download/API failure in this execution |
| `BACKFILL_MAX_SECONDS` | 3000; stops taking work with 310 seconds remaining |
| `BACKFILL_START_ID`, `BACKFILL_END_ID` | 0 exclusive, 2147483647 inclusive |
| `BACKFILL_CREATED_FROM`, `BACKFILL_CREATED_BEFORE` | optional RFC3339 timestamps, inclusive start / exclusive end |

Run one task with parallelism 1, timeout 3600 seconds and task retries 0.
A DB session advisory lock rejects overlapping photo executions. Each row has
a 300-second deadline; network/AI attempts have 60-second deadlines. Missing
objects and permanent API failures are not retried in that execution. Original
downloads are limited to 40 MiB and decoded images to `MAX_SOURCE_PIXELS`.

Database writes check the original Photo ID, file ID and extension, and only
fill components that are still missing. A rename/replacement in the CMS cannot
receive data calculated for the old file ID. SIGTERM cancels new work and exits
nonzero; existing successful updates remain. Failures are logged by Photo ID,
and cause nonzero exit. There is no automatic full-catalog loop: execute again
to process the next missing batch. Persistent failures can be isolated with the
ID bounds; inspect and repair them rather than repeatedly retrying blindly.

`Dockerfile.backfill` pins the tested CLIP runtime image by digest and replaces
the Go binary. `cloudbuild.backfill.yaml` builds only a Job image, never deploys
the online service. Use `.gcloudignore.backfill` to restrict uploaded files.
The CMS repository's `jobs/ai-backfill/deploy.py` creates both production Jobs
with immutable image digests, a dedicated service account, and check mode.

The 2026 production backfill uses Taipei calendar-year bounds:
`BACKFILL_CREATED_FROM=2025-12-31T16:00:00Z` and
`BACKFILL_CREATED_BEFORE=2026-12-31T16:00:00Z`. These filter the Photo
`createdAt` timestamp stored in UTC, not IDs or modification times. Rows with
unknown upload dates are excluded when date bounds are set. The date filter
also applies to check mode.

For this bounded annual run (about 13,134 processable photos), execution
overrides may raise `BACKFILL_MAX_ITEMS` to 20,000 and `BACKFILL_MAX_SECONDS`
to 25,200 (7 hours), with Cloud Run `--task-timeout=8h`. Defaults stay at 50
items / 3,000 seconds. A longer Job keeps the same single worker, retry caps,
advisory lock, per-item deadline, and incremental writes. The task timeout
must always exceed the application time budget.
