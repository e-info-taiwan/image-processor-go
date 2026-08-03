-- Apply this only to the isolated vector-lab database, never to the CMS prod DB.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS vector_lab_images (
  id BIGSERIAL PRIMARY KEY,
  image_file_id TEXT NOT NULL UNIQUE,
  object_name TEXT NOT NULL UNIQUE,
  source_format TEXT NOT NULL,
  generation BIGINT NOT NULL,
  embedding vector(512),
  model_version TEXT,
  vector_status TEXT NOT NULL DEFAULT 'pending',
  vector_attempts INTEGER NOT NULL DEFAULT 0,
  lease_expires_at TIMESTAMPTZ,
  last_error TEXT,
  candidate_status TEXT NOT NULL DEFAULT 'pending',
  candidate_attempts INTEGER NOT NULL DEFAULT 0,
  candidate_lease_expires_at TIMESTAMPTZ,
  candidate_error TEXT,
  candidate_run TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS vector_lab_images_vector_claim_idx
  ON vector_lab_images (vector_status, id);
CREATE INDEX IF NOT EXISTS vector_lab_images_candidate_claim_idx
  ON vector_lab_images (candidate_status, id)
  WHERE vector_status = 'succeeded';

CREATE TABLE IF NOT EXISTS vector_lab_similarity_candidates (
  run_name TEXT NOT NULL,
  image_a_id BIGINT NOT NULL REFERENCES vector_lab_images(id),
  image_b_id BIGINT NOT NULL REFERENCES vector_lab_images(id),
  distance DOUBLE PRECISION NOT NULL,
  model_version TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (run_name, image_a_id, image_b_id),
  CHECK (image_a_id < image_b_id)
);

-- Run this only after the vector backfill is complete. CREATE INDEX
-- CONCURRENTLY must be executed outside a transaction.
-- CREATE INDEX CONCURRENTLY vector_lab_images_embedding_hnsw_idx
--   ON vector_lab_images USING hnsw (embedding vector_cosine_ops);
