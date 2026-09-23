package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/corona10/goimagehash"
	"google.golang.org/api/googleapi"
)

type photoJobOptions struct {
	Mode, ExpectedDatabase                    string
	MaxItems, BatchSize, Attempts, MaxSeconds int
	StartID, EndID                            int64
	CreatedFrom, CreatedBefore                *time.Time
	Fields                                    string
	ShardIndex, ShardCount                    int
}

func photoJobInt(name string, fallback, min, max int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return n, nil
}

func loadPhotoJobOptions() (photoJobOptions, error) {
	cfg := photoJobOptions{Mode: envOrDefault("BACKFILL_MODE", "check"), ExpectedDatabase: envOrDefault("BACKFILL_EXPECTED_DATABASE", "eic-prod")}
	if cfg.Mode != "check" && cfg.Mode != "apply" {
		return cfg, errors.New("BACKFILL_MODE must be check or apply")
	}
	values := []struct {
		name               string
		fallback, min, max int64
		target             *int
	}{
		{"BACKFILL_MAX_ITEMS", 50, 1, 200000, &cfg.MaxItems},
		{"BACKFILL_BATCH_SIZE", 25, 1, 100, &cfg.BatchSize},
		{"BACKFILL_ATTEMPTS", 3, 1, 5, &cfg.Attempts},
		{"BACKFILL_MAX_SECONDS", 3000, 360, 172800, &cfg.MaxSeconds},
	}
	for _, v := range values {
		n, err := photoJobInt(v.name, v.fallback, v.min, v.max)
		if err != nil {
			return cfg, err
		}
		*v.target = int(n)
	}
	var err error
	cfg.StartID, err = photoJobInt("BACKFILL_START_ID", 0, 0, math.MaxInt32)
	if err != nil {
		return cfg, err
	}
	cfg.EndID, err = photoJobInt("BACKFILL_END_ID", math.MaxInt32, 1, math.MaxInt32)
	if err != nil {
		return cfg, err
	}
	if cfg.StartID >= cfg.EndID {
		return cfg, errors.New("BACKFILL_START_ID must be less than BACKFILL_END_ID")
	}
	for _, date := range []struct {
		name   string
		target **time.Time
	}{
		{"BACKFILL_CREATED_FROM", &cfg.CreatedFrom}, {"BACKFILL_CREATED_BEFORE", &cfg.CreatedBefore},
	} {
		if raw := strings.TrimSpace(os.Getenv(date.name)); raw != "" {
			value, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return cfg, fmt.Errorf("%s must be RFC3339 with a timezone", date.name)
			}
			value = value.UTC()
			*date.target = &value
		}
	}
	if cfg.CreatedFrom != nil && cfg.CreatedBefore != nil && !cfg.CreatedFrom.Before(*cfg.CreatedBefore) {
		return cfg, errors.New("BACKFILL_CREATED_FROM must be earlier than BACKFILL_CREATED_BEFORE")
	}
	cfg.Fields = envOrDefault("BACKFILL_FIELDS", "all")
	if cfg.Fields != "all" && cfg.Fields != "phash" && cfg.Fields != "ai" && cfg.Fields != "vector" {
		return cfg, errors.New("BACKFILL_FIELDS must be all, phash, ai, or vector")
	}
	cfg.ShardCount = 1
	requiredTasks := int64(1)
	if cfg.Fields == "phash" {
		requiredTasks = 8
		cfg.ShardCount = 8
	}
	tasks, err := photoJobInt("CLOUD_RUN_TASK_COUNT", 1, requiredTasks, requiredTasks)
	if err != nil || tasks != requiredTasks {
		return cfg, fmt.Errorf("photo job fields=%s requires tasks=%d", cfg.Fields, requiredTasks)
	}
	index, err := photoJobInt("CLOUD_RUN_TASK_INDEX", 0, 0, requiredTasks-1)
	if err != nil {
		return cfg, err
	}
	cfg.ShardIndex = int(index)
	return cfg, nil
}

type photoJobItem struct {
	ID                               int64
	FileID, Extension                string
	NeedHash, NeedVector, NeedLabels bool
}

const photoJobSelect = `SELECT id, "imageFile_id", "imageFile_extension",
    ($6 NOT IN ('ai', 'vector') AND COALESCE(phash, '') = ''),
    ($6 <> 'phash' AND "imageVector" IS NULL),
    ($6 IN ('all', 'ai') AND COALESCE("imageLabelStatus", '') <> 'success')
    FROM "Photo" WHERE id > $1 AND id <= $2
    AND "imageFile_id" IS NOT NULL AND btrim("imageFile_id") <> ''
    AND "imageFile_extension" IS NOT NULL AND btrim("imageFile_extension") <> ''
    AND (($6 NOT IN ('ai', 'vector') AND COALESCE(phash, '') = '') OR
         ($6 <> 'phash' AND "imageVector" IS NULL) OR
         ($6 IN ('all', 'ai') AND COALESCE("imageLabelStatus", '') <> 'success'))
    AND ($4::timestamp IS NULL OR "createdAt" >= $4::timestamp)
    AND ($5::timestamp IS NULL OR "createdAt" < $5::timestamp)
    AND id % $8::integer = $7::integer
    ORDER BY id LIMIT $3`

func photoJobQueryArgs(opts photoJobOptions, cursor int64, limit int) []any {
	fields := opts.Fields
	if fields == "" {
		fields = "all"
	}
	shards := opts.ShardCount
	if shards == 0 {
		shards = 1
	}
	return []any{cursor, opts.EndID, limit, opts.CreatedFrom, opts.CreatedBefore, fields, opts.ShardIndex, shards}
}

func lockPhotoJob(ctx context.Context, conn *sql.Conn, opts photoJobOptions) error {
	queries := []string{}
	if opts.Fields != "phash" {
		queries = append(queries, `SELECT pg_try_advisory_lock(62130923, 2)`)
	}
	if opts.Fields == "phash" {
		queries = append(queries, `SELECT pg_try_advisory_lock_shared(62130923, 3)`,
			fmt.Sprintf(`SELECT pg_try_advisory_lock(62130924, %d)`, opts.ShardIndex))
	} else if opts.Fields != "ai" && opts.Fields != "vector" {
		queries = append(queries, `SELECT pg_try_advisory_lock(62130923, 3)`)
	}
	for _, query := range queries {
		var acquired bool
		if err := conn.QueryRowContext(ctx, query).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			return errors.New("another photo backfill execution is running for these fields/shard")
		}
	}
	return nil
}

func runPhotoBackfillJob(cfg Config) error {
	opts, err := loadPhotoJobOptions()
	if err != nil {
		return err
	}
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required for photo job")
	}
	if cfg.EnableWatermark {
		return errors.New("photo job does not support watermark; verify production processing rules")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	connectCtx, stopConnect := context.WithTimeout(ctx, 15*time.Second)
	conn, err := db.Conn(connectCtx)
	stopConnect()
	if err != nil {
		return fmt.Errorf("database connection failed: %T", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SET statement_timeout = '30s'`); err != nil {
		return err
	}
	if opts.Mode == "check" {
		if _, err = conn.ExecContext(ctx, `SET default_transaction_read_only = on`); err != nil {
			return err
		}
	}
	if err = photoJobPreflight(ctx, conn, opts); err != nil {
		return err
	}
	if opts.Mode == "check" {
		return nil
	}
	if cfg.ImageBucket == "" {
		return errors.New("IMAGE_BUCKET is required")
	}
	if opts.Fields != "phash" && !cfg.EnableImageVector {
		return errors.New("photo job requires ENABLE_IMAGE_VECTOR for vector fields")
	}
	if (opts.Fields == "all" || opts.Fields == "ai") && !cfg.EnableImageLabel {
		return errors.New("photo job requires ENABLE_IMAGE_LABEL for label fields")
	}
	if err = lockPhotoJob(ctx, conn, opts); err != nil {
		return err
	}
	// Closing this dedicated connection releases the session lock, including on SIGTERM.
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("storage client: %w", err)
	}
	defer client.Close()
	var highID int64
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(max(id), 0) FROM "Photo"`).Scan(&highID); err != nil {
		return err
	}
	if opts.EndID > highID {
		opts.EndID = highID
	}
	stopAt := time.Now().Add(time.Duration(opts.MaxSeconds) * time.Second)
	cursor := opts.StartID
	processed, failed := 0, 0
	for processed < opts.MaxItems && time.Now().Add(310*time.Second).Before(stopAt) && ctx.Err() == nil {
		limit := min(opts.BatchSize, opts.MaxItems-processed)
		rows, err := conn.QueryContext(ctx, photoJobSelect, photoJobQueryArgs(opts, cursor, limit)...)
		if err != nil {
			return fmt.Errorf("list photo candidates: %w", err)
		}
		batch := []photoJobItem{}
		for rows.Next() {
			var item photoJobItem
			if err = rows.Scan(&item.ID, &item.FileID, &item.Extension, &item.NeedHash, &item.NeedVector, &item.NeedLabels); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, item := range batch {
			if ctx.Err() != nil || !time.Now().Add(310*time.Second).Before(stopAt) {
				break
			}
			itemCtx, stop := context.WithTimeout(ctx, 300*time.Second)
			err := processPhotoJobItem(itemCtx, conn, client, cfg, opts, item)
			stop()
			processed++
			cursor = item.ID
			if err != nil {
				failed++
				log.Printf("photo_backfill item_failed id=%d error=%v", item.ID, err)
			}
		}
	}
	log.Printf("photo_backfill summary fields=%s shard=%d/%d processed=%d succeeded=%d failed=%d next_cursor=%d end_id=%d", opts.Fields, opts.ShardIndex, opts.ShardCount, processed, processed-failed, failed, cursor, opts.EndID)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed > 0 {
		return fmt.Errorf("%d photos incomplete; successful components were retained", failed)
	}
	return nil
}

func photoJobPreflight(ctx context.Context, conn *sql.Conn, opts photoJobOptions) error {
	var database string
	if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return err
	}
	if database != opts.ExpectedDatabase {
		return errors.New("database does not match BACKFILL_EXPECTED_DATABASE")
	}
	required := map[string]string{
		"createdAt": "timestamp(3) without time zone",
		"phash":     "text", "imageVector": "vector(512)", "imageVectorStatus": "text",
		"imageVectorFailReason": "text", "imageVectorUpdatedAt": "timestamp(3) without time zone",
		"imageLabelRawResult": "jsonb", "imageLabelSuggestions": "jsonb", "imageLabelStatus": "text",
		"imageLabelFailReason": "text", "imageLabelUpdatedAt": "timestamp(3) without time zone",
	}
	rows, err := conn.QueryContext(ctx, `SELECT attname, format_type(atttypid, atttypmod), attnotnull FROM pg_attribute
        WHERE attrelid = '"Photo"'::regclass AND attnum > 0 AND NOT attisdropped`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, typ string
		var notNull bool
		if err = rows.Scan(&name, &typ, &notNull); err != nil {
			rows.Close()
			return err
		}
		if expected, ok := required[name]; ok && typ == expected {
			if (name == "imageLabelFailReason" || name == "imageVectorFailReason") && notNull {
				continue
			}
			delete(required, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(required) != 0 {
		return fmt.Errorf("photo schema is not ready; missing or incompatible columns: %v", required)
	}
	var sample int
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM ("+photoJobSelect+") p",
		photoJobQueryArgs(opts, opts.StartID, 1000)...).Scan(&sample); err != nil {
		return err
	}
	log.Printf("photo_backfill preflight mode=%s schema=ready missing_sample_up_to_1000=%d created_from=%v created_before=%v", opts.Mode, sample, opts.CreatedFrom, opts.CreatedBefore)
	return nil
}

func photoJobRetry(ctx context.Context, attempts int, fn func(context.Context) error) error {
	var err error
	for n := 0; n < attempts; n++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err = fn(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if !photoJobRetryable(err) {
			return err
		}
		if n+1 < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<n) * time.Second):
			}
		}
	}
	return err
}

func photoJobRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrObjectNotExist) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	var api *googleapi.Error
	if errors.As(err, &api) {
		return api.Code == 429 || api.Code >= 500
	}
	// Existing Vision/vector clients include HTTP status in wrapped errors.
	return strings.Contains(err.Error(), "server error (429)") || strings.Contains(err.Error(), "server error (5")
}

const photoJobMaxBytes = 40 << 20

func photoJobRead(ctx context.Context, client *storage.Client, bucket, object string) ([]byte, error) {
	reader, err := client.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, photoJobMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > photoJobMaxBytes {
		return nil, errors.New("source exceeds 40 MiB job limit")
	}
	return data, nil
}

func photoJobPrepareOriginal(data []byte, ext string, maxPixels int) (string, []byte, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", nil, err
	}
	if err = validateSourceImageSize(config.Width, config.Height, maxPixels); err != nil {
		return "", nil, err
	}
	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", nil, err
	}
	resized := resizeImage(applyEXIFOrientation(src, decodeExif(data)), 480)
	hash, err := goimagehash.PerceptionHash(resized)
	if err != nil {
		return "", nil, err
	}
	var encoded bytes.Buffer
	if err = encodeToWriter(&encoded, resized, "."+strings.TrimPrefix(ext, "."), format == "jpeg"); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%016x", hash.GetHash()), encoded.Bytes(), nil
}

func photoJobVectorValid(vector []float64) bool {
	if len(vector) != 512 {
		return false
	}
	nonzero := false
	for _, n := range vector {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return false
		}
		if n != 0 {
			nonzero = true
		}
	}
	return nonzero
}

func processPhotoJobItem(ctx context.Context, conn *sql.Conn, client *storage.Client, cfg Config, opts photoJobOptions, item photoJobItem) error {
	if strings.ContainsAny(item.FileID, "/\\") || strings.ContainsAny(item.Extension, "/\\") {
		return errors.New("invalid image file identifier")
	}
	var data []byte
	var hash string
	err := photoJobRetry(ctx, opts.Attempts, func(attempt context.Context) error {
		var err error
		if !item.NeedHash {
			data, err = photoJobRead(attempt, client, cfg.ImageBucket, buildBackfillObjectName(item.FileID, item.Extension))
			if err == nil {
				return nil
			}
			if !errors.Is(err, storage.ErrObjectNotExist) {
				return err
			}
		}
		original, err := photoJobRead(attempt, client, cfg.ImageBucket, "images/"+item.FileID+"."+strings.TrimPrefix(item.Extension, "."))
		if err != nil {
			return err
		}
		hash, data, err = photoJobPrepareOriginal(original, item.Extension, cfg.MaxSourcePixels)
		return err
	})
	if err != nil {
		return fmt.Errorf("read/prepare source: %w", err)
	}
	failures := []error{}
	// Every update checks the source identity and preserves values filled by the online processor.
	if item.NeedHash {
		_, err = conn.ExecContext(ctx, `UPDATE "Photo" SET phash=$1 WHERE id=$2 AND "imageFile_id"=$3 AND "imageFile_extension"=$4 AND COALESCE(phash,'')=''`, hash, item.ID, item.FileID, item.Extension)
		if err != nil {
			failures = append(failures, fmt.Errorf("phash: %w", err))
		}
	}
	if item.NeedVector {
		var vector []float64
		err = photoJobRetry(ctx, opts.Attempts, func(attempt context.Context) error {
			var err error
			vector, err = ComputeImageVectorContext(attempt, data)
			if err == nil && !photoJobVectorValid(vector) {
				return errors.New("invalid 512-dimensional CLIP vector")
			}
			return err
		})
		if err == nil {
			encoded, _ := json.Marshal(vector)
			_, err = conn.ExecContext(ctx, `UPDATE "Photo" SET "imageVector"=$1::vector, "imageVectorStatus"='success', "imageVectorFailReason"=NULL, "imageVectorUpdatedAt"=NOW()
                WHERE id=$2 AND "imageFile_id"=$3 AND "imageFile_extension"=$4 AND "imageVector" IS NULL`, string(encoded), item.ID, item.FileID, item.Extension)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("vector: %w", err))
		}
	}
	if item.NeedLabels {
		var labels []ImageLabel
		var person bool
		err = photoJobRetry(ctx, opts.Attempts, func(attempt context.Context) error {
			var err error
			labels, person, err = DetectImageLabelsWithFacesContext(attempt, cfg, data)
			return err
		})
		if err == nil {
			suggestions := AddPersonSuggestion(FilterImageLabelSuggestions(labels, cfg.ImageLabelMinScore), person)
			raw, _ := json.Marshal(labels)
			suggested, _ := json.Marshal(suggestions)
			_, err = conn.ExecContext(ctx, `UPDATE "Photo" SET "imageLabelRawResult"=$1::jsonb, "imageLabelSuggestions"=$2::jsonb,
                "imageLabelStatus"='success', "imageLabelFailReason"=NULL, "imageLabelUpdatedAt"=NOW()
                WHERE id=$3 AND "imageFile_id"=$4 AND "imageFile_extension"=$5 AND COALESCE("imageLabelStatus",'') <> 'success'`, string(raw), string(suggested), item.ID, item.FileID, item.Extension)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("labels: %w", err))
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	log.Printf("photo_backfill item_complete id=%d", item.ID)
	return nil
}
