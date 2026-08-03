# Vector lab Cloud Run Jobs

These commands use the same container image as `image-processor`, but set
`VECTOR_LAB_MODE`; the binary exits after its batch and does not start the HTTP
image-processing service. Replace all angle-bracketed values before use.

Both Jobs must use a dedicated lab service account with only these permissions:

- `storage.objects.list` and `storage.objects.get` on the production image bucket.
- Cloud SQL Client and the lab database credential secret in the development project.

Do not grant this service account GCS write/delete permissions, Eventarc roles,
Pub/Sub subscriber roles, or any production database credential.

## 1. Photo vector backfill job

Create one Job resource for both manifest seeding and vectorization. First run
it once with `VECTOR_LAB_MODE=seed` and one task. Then update the mode to
`vectorize` and run it repeatedly until no pending rows remain.

```bash
gcloud run jobs deploy photo-vector-backfill-lab \
  --image=<IMAGE_URI> \
  --region=<DEV_REGION> \
  --service-account=<LAB_SERVICE_ACCOUNT> \
  --set-env-vars=VECTOR_LAB_MODE=seed,IMAGE_BUCKET=<PROD_IMAGE_BUCKET>,DB_HOST=<DEV_DB_HOST>,DB_NAME=<LAB_DB_NAME>,DB_USER=<LAB_DB_USER>,VECTOR_LAB_MAX_SEED_ITEMS=1000 \
  --set-secrets=DB_PASSWORD=<LAB_DB_PASSWORD_SECRET>:latest \
  --tasks=1 --parallelism=1 --task-timeout=6h --max-retries=1
```

For a very large bucket, run `seed` multiple times with non-overlapping
`VECTOR_LAB_OBJECT_START_OFFSET` / `VECTOR_LAB_OBJECT_END_OFFSET` ranges.

After the manifest is complete, update and execute the same Job:

```bash
gcloud run jobs update photo-vector-backfill-lab \
  --region=<DEV_REGION> \
  --update-env-vars=VECTOR_LAB_MODE=vectorize,ENABLE_IMAGE_VECTOR=true,VECTOR_LAB_BATCH_SIZE=25,VECTOR_LAB_MAX_ITEMS_PER_TASK=500,VECTOR_LAB_MAX_ATTEMPTS=3

gcloud run jobs execute photo-vector-backfill-lab --region=<DEV_REGION> --tasks=20 --parallelism=5 --wait
```

Repeat the execution while this query returns rows:

```sql
SELECT vector_status, count(*)
FROM vector_lab_images
GROUP BY vector_status
ORDER BY vector_status;
```

After vectorization has completed, create the HNSW index from
`vector-lab-schema.sql` in the lab database. Do not start candidate generation
without it.

## 2. Similarity candidate job

```bash
gcloud run jobs deploy photo-similarity-candidates-lab \
  --image=<IMAGE_URI> \
  --region=<DEV_REGION> \
  --service-account=<LAB_SERVICE_ACCOUNT> \
  --set-env-vars=VECTOR_LAB_MODE=candidates,IMAGE_BUCKET=<PROD_IMAGE_BUCKET>,DB_HOST=<DEV_DB_HOST>,DB_NAME=<LAB_DB_NAME>,DB_USER=<LAB_DB_USER>,VECTOR_LAB_CANDIDATE_RUN=initial,VECTOR_LAB_CANDIDATE_LIMIT=20,VECTOR_LAB_CANDIDATE_THRESHOLD=0.22,VECTOR_LAB_MAX_ITEMS_PER_TASK=500 \
  --set-secrets=DB_PASSWORD=<LAB_DB_PASSWORD_SECRET>:latest \
  --tasks=20 --parallelism=5 --task-timeout=6h --max-retries=1
```

Repeat executions until:

```sql
SELECT candidate_status, count(*)
FROM vector_lab_images
WHERE vector_status = 'succeeded'
GROUP BY candidate_status
ORDER BY candidate_status;
```

Candidate pairs are deduplicated as unordered pairs in
`vector_lab_similarity_candidates`. Use a new `VECTOR_LAB_CANDIDATE_RUN` after
changing the threshold, candidate limit, or model version.
