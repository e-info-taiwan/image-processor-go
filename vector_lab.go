package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

const (
	vectorLabModeSeed       = "seed"
	vectorLabModeVectorize  = "vectorize"
	vectorLabModeCandidates = "candidates"
	vectorDimension         = 512
)

var w480ObjectPattern = regexp.MustCompile(`(?i)^images/(.+)-w480\.(jpe?g|png|gif|tiff?|webp)$`)

type vectorLabConfig struct {
	Mode               string
	BatchSize          int
	MaxItemsPerTask    int
	MaxAttempts        int
	LeaseDuration      time.Duration
	CandidateLimit     int
	CandidateThreshold float64
	CandidateRun       string
	ModelVersion       string
	ObjectStartOffset  string
	ObjectEndOffset    string
	MaxSeedItems       int
}

type vectorLabImage struct {
	ID          int64
	ImageFileID string
	ObjectName  string
	Generation  int64
	Vector      string
}

func loadVectorLabConfig() (vectorLabConfig, error) {
	cfg := vectorLabConfig{
		Mode:               strings.ToLower(strings.TrimSpace(os.Getenv("VECTOR_LAB_MODE"))),
		BatchSize:          parsePositiveEnv("VECTOR_LAB_BATCH_SIZE", 25, 1, 500),
		MaxItemsPerTask:    parsePositiveEnv("VECTOR_LAB_MAX_ITEMS_PER_TASK", 500, 1, 10000),
		MaxAttempts:        parsePositiveEnv("VECTOR_LAB_MAX_ATTEMPTS", 3, 1, 20),
		LeaseDuration:      time.Duration(parsePositiveEnv("VECTOR_LAB_LEASE_MINUTES", 60, 1, 24*60)) * time.Minute,
		CandidateLimit:     parsePositiveEnv("VECTOR_LAB_CANDIDATE_LIMIT", 20, 1, 100),
		CandidateThreshold: parseFloatEnv("VECTOR_LAB_CANDIDATE_THRESHOLD", 0.22),
		CandidateRun:       envOrDefault("VECTOR_LAB_CANDIDATE_RUN", "initial"),
		ModelVersion:       envOrDefault("VECTOR_LAB_MODEL_VERSION", "clip-ViT-B-32"),
		ObjectStartOffset:  strings.TrimSpace(os.Getenv("VECTOR_LAB_OBJECT_START_OFFSET")),
		ObjectEndOffset:    strings.TrimSpace(os.Getenv("VECTOR_LAB_OBJECT_END_OFFSET")),
		MaxSeedItems:       parseNonNegativeEnv("VECTOR_LAB_MAX_SEED_ITEMS", 0, 10000000),
	}
	if cfg.Mode != vectorLabModeSeed && cfg.Mode != vectorLabModeVectorize && cfg.Mode != vectorLabModeCandidates {
		return vectorLabConfig{}, fmt.Errorf("VECTOR_LAB_MODE must be seed, vectorize, or candidates")
	}
	if cfg.CandidateThreshold <= 0 || cfg.CandidateThreshold >= 2 {
		return vectorLabConfig{}, fmt.Errorf("VECTOR_LAB_CANDIDATE_THRESHOLD must be between 0 and 2")
	}
	return cfg, nil
}

func parsePositiveEnv(key string, fallback, min, max int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value < min || value > max {
		return fallback
	}
	return value
}

func parseNonNegativeEnv(key string, fallback, max int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value < 0 || value > max {
		return fallback
	}
	return value
}

func runVectorLab(ctx context.Context, cfg Config, storageClient *storage.Client) error {
	labCfg, err := loadVectorLabConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ImageBucket) == "" {
		return errors.New("IMAGE_BUCKET is required for VECTOR_LAB_MODE")
	}

	db, err := getDBConnection(cfg)
	if err != nil {
		return fmt.Errorf("connect vector lab database: %w", err)
	}
	if err := ensureVectorLabSchema(ctx, db); err != nil {
		return err
	}

	switch labCfg.Mode {
	case vectorLabModeSeed:
		return seedVectorLabImages(ctx, db, storageClient, cfg.ImageBucket, labCfg)
	case vectorLabModeVectorize:
		return vectorizeVectorLabImages(ctx, db, storageClient, cfg.ImageBucket, labCfg)
	case vectorLabModeCandidates:
		return generateVectorLabCandidates(ctx, db, labCfg)
	default:
		return fmt.Errorf("unsupported vector lab mode: %s", labCfg.Mode)
	}
}

// ensureVectorLabSchema creates only lab-owned tables and ordinary claim
// indexes. The HNSW index is intentionally excluded: build it after the bulk
// vector load completes.
func ensureVectorLabSchema(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE TABLE IF NOT EXISTS vector_lab_images (
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
		)`,
		`CREATE INDEX IF NOT EXISTS vector_lab_images_vector_claim_idx
			ON vector_lab_images (vector_status, id)`,
		`CREATE INDEX IF NOT EXISTS vector_lab_images_candidate_claim_idx
			ON vector_lab_images (candidate_status, id)
			WHERE vector_status = 'succeeded'`,
		`CREATE TABLE IF NOT EXISTS vector_lab_similarity_candidates (
			run_name TEXT NOT NULL,
			image_a_id BIGINT NOT NULL REFERENCES vector_lab_images(id),
			image_b_id BIGINT NOT NULL REFERENCES vector_lab_images(id),
			distance DOUBLE PRECISION NOT NULL,
			model_version TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (run_name, image_a_id, image_b_id),
			CHECK (image_a_id < image_b_id)
		)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize vector lab schema: %w", err)
		}
	}
	return nil
}

// seedVectorLabImages is deliberately a single-task operation. Run it once
// before the parallel vectorize Job; it only reads the configured GCS bucket.
func seedVectorLabImages(ctx context.Context, db *sql.DB, storageClient *storage.Client, bucketName string, cfg vectorLabConfig) error {
	if taskIndex := strings.TrimSpace(os.Getenv("CLOUD_RUN_TASK_INDEX")); taskIndex != "" && taskIndex != "0" {
		log.Printf("seed mode skipped by task %s", taskIndex)
		return nil
	}

	iter := storageClient.Bucket(bucketName).Objects(ctx, &storage.Query{
		Prefix:      "images/",
		StartOffset: cfg.ObjectStartOffset,
		EndOffset:   cfg.ObjectEndOffset,
	})
	seeded := 0
	seenFileIDs := make(map[string]struct{})
	for {
		attrs, err := iter.Next()
		if err == iteratorDone {
			break
		}
		if err != nil {
			return fmt.Errorf("list objects in bucket %q: %w", bucketName, err)
		}
		imageFileID, sourceFormat, ok := parseW480Object(attrs.Name)
		if !ok {
			continue
		}
		_, alreadySeen := seenFileIDs[imageFileID]
		seenFileIDs[imageFileID] = struct{}{}

		_, err = db.ExecContext(ctx, `
			INSERT INTO vector_lab_images (image_file_id, object_name, source_format, generation, vector_status)
			VALUES ($1, $2, $3, $4, 'pending')
			ON CONFLICT (image_file_id) DO UPDATE
			SET object_name = CASE
			      WHEN vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp')
			      THEN EXCLUDED.object_name ELSE vector_lab_images.object_name END,
			    source_format = CASE
			      WHEN vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp')
			      THEN EXCLUDED.source_format ELSE vector_lab_images.source_format END,
			    generation = CASE
			      WHEN vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp')
			      THEN EXCLUDED.generation ELSE vector_lab_images.generation END,
			    vector_status = CASE
			      WHEN (vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp'))
			        AND (vector_lab_images.object_name IS DISTINCT FROM EXCLUDED.object_name
			          OR vector_lab_images.generation IS DISTINCT FROM EXCLUDED.generation)
			      THEN 'pending'
			      ELSE vector_lab_images.vector_status
			    END,
			    embedding = CASE
			      WHEN (vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp'))
			        AND (vector_lab_images.object_name IS DISTINCT FROM EXCLUDED.object_name
			          OR vector_lab_images.generation IS DISTINCT FROM EXCLUDED.generation)
			      THEN NULL
			      ELSE vector_lab_images.embedding
			    END,
			    vector_attempts = CASE
			      WHEN (vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp'))
			        AND (vector_lab_images.object_name IS DISTINCT FROM EXCLUDED.object_name
			          OR vector_lab_images.generation IS DISTINCT FROM EXCLUDED.generation)
			      THEN 0
			      ELSE vector_lab_images.vector_attempts
			    END,
			    candidate_status = CASE
			      WHEN (vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp'))
			        AND (vector_lab_images.object_name IS DISTINCT FROM EXCLUDED.object_name
			          OR vector_lab_images.generation IS DISTINCT FROM EXCLUDED.generation)
			      THEN 'pending'
			      ELSE vector_lab_images.candidate_status
			    END,
			    candidate_attempts = CASE
			      WHEN (vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp'))
			        AND (vector_lab_images.object_name IS DISTINCT FROM EXCLUDED.object_name
			          OR vector_lab_images.generation IS DISTINCT FROM EXCLUDED.generation)
			      THEN 0
			      ELSE vector_lab_images.candidate_attempts
			    END,
			    candidate_run = CASE
			      WHEN (vector_lab_images.object_name = EXCLUDED.object_name
			        OR (vector_lab_images.source_format = 'webp' AND EXCLUDED.source_format <> 'webp'))
			        AND (vector_lab_images.object_name IS DISTINCT FROM EXCLUDED.object_name
			          OR vector_lab_images.generation IS DISTINCT FROM EXCLUDED.generation)
			      THEN NULL
			      ELSE vector_lab_images.candidate_run
			    END,
			    updated_at = NOW()`, imageFileID, attrs.Name, sourceFormat, attrs.Generation)
		if err != nil {
			return fmt.Errorf("upsert manifest object %q: %w", attrs.Name, err)
		}
		if !alreadySeen {
			seeded++
		}
		if seeded%10000 == 0 {
			log.Printf("seeded %d w480 objects", seeded)
		}
		if cfg.MaxSeedItems > 0 && seeded >= cfg.MaxSeedItems {
			log.Printf("seed limit reached: %d unique images", seeded)
			return nil
		}
	}
	log.Printf("seed completed: %d w480 objects start=%q end=%q", seeded, cfg.ObjectStartOffset, cfg.ObjectEndOffset)
	return nil
}

// storage.IteratorDone is kept behind a variable so the package can be tested
// without wrapping the Cloud Storage iterator.
var iteratorDone = iterator.Done

func isW480Object(name string) bool {
	_, _, ok := parseW480Object(name)
	return ok
}

func parseW480Object(name string) (imageFileID, sourceFormat string, ok bool) {
	matches := w480ObjectPattern.FindStringSubmatch(name)
	if len(matches) != 3 {
		return "", "", false
	}
	return matches[1], strings.ToLower(matches[2]), true
}

func vectorizeVectorLabImages(ctx context.Context, db *sql.DB, storageClient *storage.Client, bucketName string, cfg vectorLabConfig) error {
	processed := 0
	for {
		images, err := claimVectorLabImages(ctx, db, cfg)
		if err != nil {
			return err
		}
		if len(images) == 0 {
			log.Printf("vectorize completed: processed=%d", processed)
			return nil
		}

		for _, image := range images {
			if err := vectorizeVectorLabImage(ctx, db, storageClient, bucketName, image, cfg.ModelVersion); err != nil {
				log.Printf("vectorize failed: id=%d object=%s err=%v", image.ID, image.ObjectName, err)
			}
			processed++
			if processed >= cfg.MaxItemsPerTask {
				log.Printf("vectorize task limit reached: processed=%d; start another Job execution", processed)
				return nil
			}
		}
	}
}

func claimVectorLabImages(ctx context.Context, db *sql.DB, cfg vectorLabConfig) ([]vectorLabImage, error) {
	rows, err := db.QueryContext(ctx, `
		WITH claimed AS (
		  SELECT id
		  FROM vector_lab_images
		  WHERE (vector_status IN ('pending', 'failed') OR (vector_status = 'running' AND lease_expires_at < NOW()))
		    AND vector_attempts < $1
		  ORDER BY id
		  FOR UPDATE SKIP LOCKED
		  LIMIT $2
		)
		UPDATE vector_lab_images image
		SET vector_status = 'running',
		    vector_attempts = image.vector_attempts + 1,
		    lease_expires_at = NOW() + ($3 * INTERVAL '1 minute'),
		    updated_at = NOW()
		FROM claimed
		WHERE image.id = claimed.id
		RETURNING image.id, image.image_file_id, image.object_name, image.generation`, cfg.MaxAttempts, cfg.BatchSize, int(cfg.LeaseDuration.Minutes()))
	if err != nil {
		return nil, fmt.Errorf("claim vector lab images: %w", err)
	}
	defer rows.Close()

	images := make([]vectorLabImage, 0, cfg.BatchSize)
	for rows.Next() {
		var image vectorLabImage
		if err := rows.Scan(&image.ID, &image.ImageFileID, &image.ObjectName, &image.Generation); err != nil {
			return nil, fmt.Errorf("scan claimed vector lab image: %w", err)
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

func vectorizeVectorLabImage(ctx context.Context, db *sql.DB, storageClient *storage.Client, bucketName string, image vectorLabImage, modelVersion string) error {
	handle := storageClient.Bucket(bucketName).Object(image.ObjectName)
	if image.Generation > 0 {
		handle = handle.Generation(image.Generation)
	}
	reader, err := handle.NewReader(ctx)
	if err != nil {
		return markVectorLabImageFailed(ctx, db, image.ID, fmt.Errorf("read GCS object: %w", err))
	}
	bytes, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return markVectorLabImageFailed(ctx, db, image.ID, fmt.Errorf("read GCS object: %w", readErr))
	}
	if closeErr != nil {
		return markVectorLabImageFailed(ctx, db, image.ID, fmt.Errorf("close GCS object: %w", closeErr))
	}

	vector, err := ComputeImageVector(bytes)
	if err != nil {
		return markVectorLabImageFailed(ctx, db, image.ID, err)
	}
	if len(vector) != vectorDimension || !isFiniteVector(vector) {
		return markVectorLabImageFailed(ctx, db, image.ID, fmt.Errorf("invalid vector with %d dimensions", len(vector)))
	}

	_, err = db.ExecContext(ctx, `
		UPDATE vector_lab_images
		SET embedding = $1::vector,
		    model_version = $2,
		    vector_status = 'succeeded',
		    lease_expires_at = NULL,
		    last_error = NULL,
		    candidate_status = 'pending',
		    candidate_attempts = 0,
		    candidate_run = NULL,
		    updated_at = NOW()
		WHERE id = $3`, vectorLiteral(vector), modelVersion, image.ID)
	if err != nil {
		return fmt.Errorf("persist vector: %w", err)
	}
	return nil
}

func markVectorLabImageFailed(ctx context.Context, db *sql.DB, imageID int64, cause error) error {
	_, err := db.ExecContext(ctx, `
		UPDATE vector_lab_images
		SET vector_status = 'failed', lease_expires_at = NULL, last_error = $1, updated_at = NOW()
		WHERE id = $2`, truncateReason(cause.Error(), 1024), imageID)
	if err != nil {
		return fmt.Errorf("mark vector failure after %v: %w", cause, err)
	}
	return cause
}

func isFiniteVector(vector []float64) bool {
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func vectorLiteral(vector []float64) string {
	parts := make([]string, len(vector))
	for i, value := range vector {
		parts[i] = strconv.FormatFloat(value, 'g', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func generateVectorLabCandidates(ctx context.Context, db *sql.DB, cfg vectorLabConfig) error {
	if err := ensureVectorLabHNSWIndex(ctx, db); err != nil {
		return err
	}
	processed := 0
	for {
		images, err := claimCandidateSources(ctx, db, cfg)
		if err != nil {
			return err
		}
		if len(images) == 0 {
			log.Printf("candidate generation completed: processed=%d run=%s", processed, cfg.CandidateRun)
			return nil
		}
		for _, image := range images {
			if err := generateCandidatesForImage(ctx, db, image, cfg); err != nil {
				log.Printf("candidate generation failed: id=%d err=%v", image.ID, err)
			}
			processed++
			if processed >= cfg.MaxItemsPerTask {
				log.Printf("candidate task limit reached: processed=%d; start another Job execution", processed)
				return nil
			}
		}
	}
}

func claimCandidateSources(ctx context.Context, db *sql.DB, cfg vectorLabConfig) ([]vectorLabImage, error) {
	rows, err := db.QueryContext(ctx, `
		WITH claimed AS (
		  SELECT id
		  FROM vector_lab_images
		  WHERE vector_status = 'succeeded'
		    AND (candidate_run IS DISTINCT FROM $1
		      OR ((candidate_status IN ('pending', 'failed') OR (candidate_status = 'running' AND candidate_lease_expires_at < NOW()))
		        AND candidate_attempts < $2))
		  ORDER BY id
		  FOR UPDATE SKIP LOCKED
		  LIMIT $3
		)
		UPDATE vector_lab_images image
		SET candidate_status = 'running',
		    candidate_attempts = CASE WHEN image.candidate_run IS DISTINCT FROM $1 THEN 1 ELSE image.candidate_attempts + 1 END,
		    candidate_run = $1,
		    candidate_lease_expires_at = NOW() + ($4 * INTERVAL '1 minute'),
		    updated_at = NOW()
		FROM claimed
		WHERE image.id = claimed.id
		RETURNING image.id, image.embedding::text`, cfg.CandidateRun, cfg.MaxAttempts, cfg.BatchSize, int(cfg.LeaseDuration.Minutes()))
	if err != nil {
		return nil, fmt.Errorf("claim candidate sources: %w", err)
	}
	defer rows.Close()

	images := make([]vectorLabImage, 0, cfg.BatchSize)
	for rows.Next() {
		var image vectorLabImage
		if err := rows.Scan(&image.ID, &image.Vector); err != nil {
			return nil, fmt.Errorf("scan candidate source: %w", err)
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

func generateCandidatesForImage(ctx context.Context, db *sql.DB, image vectorLabImage, cfg vectorLabConfig) error {
	rows, err := db.QueryContext(ctx, `
		SELECT id, embedding <=> $1::vector AS distance
		FROM vector_lab_images
		WHERE embedding IS NOT NULL AND id <> $2
		ORDER BY embedding <=> $1::vector
		LIMIT $3`, image.Vector, image.ID, cfg.CandidateLimit)
	if err != nil {
		return markCandidateSourceFailed(ctx, db, image.ID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var neighborID int64
		var distance float64
		if err := rows.Scan(&neighborID, &distance); err != nil {
			return markCandidateSourceFailed(ctx, db, image.ID, err)
		}
		if distance > cfg.CandidateThreshold {
			continue
		}
		firstID, secondID := canonicalPair(image.ID, neighborID)
		_, err = db.ExecContext(ctx, `
			INSERT INTO vector_lab_similarity_candidates
			  (run_name, image_a_id, image_b_id, distance, model_version)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (run_name, image_a_id, image_b_id) DO UPDATE
			SET distance = LEAST(vector_lab_similarity_candidates.distance, EXCLUDED.distance),
			    updated_at = NOW()`, cfg.CandidateRun, firstID, secondID, distance, cfg.ModelVersion)
		if err != nil {
			return markCandidateSourceFailed(ctx, db, image.ID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return markCandidateSourceFailed(ctx, db, image.ID, err)
	}
	_, err = db.ExecContext(ctx, `
		UPDATE vector_lab_images
		SET candidate_status = 'succeeded', candidate_lease_expires_at = NULL, candidate_error = NULL, updated_at = NOW()
		WHERE id = $1`, image.ID)
	return err
}

func ensureVectorLabHNSWIndex(ctx context.Context, db *sql.DB) error {
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_indexes
		  WHERE schemaname = current_schema()
		    AND tablename = 'vector_lab_images'
		    AND indexdef ILIKE '%USING hnsw%'
		    AND indexdef ILIKE '%embedding%'
		)`).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check vector lab HNSW index: %w", err)
	}
	if !exists {
		return errors.New("refusing candidate generation without an HNSW index on vector_lab_images.embedding")
	}
	return nil
}

func markCandidateSourceFailed(ctx context.Context, db *sql.DB, imageID int64, cause error) error {
	_, err := db.ExecContext(ctx, `
		UPDATE vector_lab_images
		SET candidate_status = 'failed', candidate_lease_expires_at = NULL, candidate_error = $1, updated_at = NOW()
		WHERE id = $2`, truncateReason(cause.Error(), 1024), imageID)
	if err != nil {
		return fmt.Errorf("mark candidate failure after %v: %w", cause, err)
	}
	return cause
}

func canonicalPair(first, second int64) (int64, int64) {
	if first < second {
		return first, second
	}
	return second, first
}
