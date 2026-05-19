package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type Duplicate struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Keep "exact duplicate" pHash matching strict; this only tolerates tiny
// differences from resizing or re-encoding.
const phashDuplicateHammingThreshold = 2

type ImageVectorBackfillCandidate struct {
	ID                 string
	ImageFileID        string
	ImageFileExtension string
	ImageBucket        string
}

type ListImageVectorBackfillCandidatesInput struct {
	Mode              string
	Limit             int
	Cursor            string
	MaxRetries        int
	OnlyOlderThanMins int
}

var ErrInvalidCursor = errors.New("invalid cursor")

// dbOpen opens the application database. Overridden in tests (e.g. sqlmock).
var dbOpen = func(cfg Config) (*sql.DB, error) {
	connStr := fmt.Sprintf("host=%s dbname=%s user=%s password=%s sslmode=disable",
		cfg.DbHost, cfg.DbName, cfg.DbUser, cfg.DbPassword)
	return sql.Open("postgres", connStr)
}

func getDBConnection(cfg Config) (*sql.DB, error) {
	return dbOpen(cfg)
}

func UpdateImageMetadata(cfg Config, imageFileID, phashStr, bucketName string, exifData map[string]interface{}, imageVector []float64) error {
	db, err := getDBConnection(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to db: %w", err)
	}
	defer db.Close()

	tableName := cfg.QueriedDbTable

	// possibleDuplicates powers the CMS pHash duplicate UI, so keep it pHash-only.
	query := fmt.Sprintf(`
		SELECT id, "imageFile_id", "imageFile_extension" FROM "%s"
		WHERE phash IS NOT NULL
		  AND phash != ''
		  AND "imageFile_id" != $1
		  AND bit_count(('x' || phash)::bit(64) # ('x' || $2)::bit(64)) <= %d
	`, tableName, phashDuplicateHammingThreshold)
	rows, err := db.Query(query, imageFileID, phashStr)

	if err != nil {
		return fmt.Errorf("failed to query similar images: %w", err)
	}
	defer rows.Close()

	duplicates := []Duplicate{}
	for rows.Next() {
		var id, recFileID sql.NullString
		var recExt sql.NullString
		if err := rows.Scan(&id, &recFileID, &recExt); err != nil {
			log.Printf("error scanning similar image row: %v", err)
			continue
		}
		ext := ""
		if recExt.Valid && recExt.String != "" {
			ext = "." + recExt.String
		}
		url := fmt.Sprintf("https://storage.googleapis.com/%s/images/%s-w480%s", bucketName, recFileID.String, ext)
		duplicates = append(duplicates, Duplicate{
			ID:  id.String,
			URL: url,
		})
	}

	duplicatesJSON, err := json.Marshal(duplicates)
	if err != nil {
		return fmt.Errorf("failed to marshal duplicates: %w", err)
	}

	exifJSON, err := json.Marshal(exifData)
	if err != nil {
		return fmt.Errorf("failed to marshal exif: %w", err)
	}

	// 2. Update DB with new phash, exif, possibleDuplicates, and optional imageVector.
	if len(imageVector) > 0 {
		vectorBytes, _ := json.Marshal(imageVector)
		vectorStr := string(vectorBytes)
		updateQuery := fmt.Sprintf(`
			UPDATE "%s"
			SET phash = $1, exif = $2, "possibleDuplicates" = $3, "imageVector" = $4::vector
			WHERE "imageFile_id" = $5
		`, tableName)
		_, err = db.Exec(updateQuery, phashStr, string(exifJSON), string(duplicatesJSON), vectorStr, imageFileID)
	} else {
		updateQuery := fmt.Sprintf(`
			UPDATE "%s"
			SET phash = $1, exif = $2, "possibleDuplicates" = $3
			WHERE "imageFile_id" = $4
		`, tableName)
		_, err = db.Exec(updateQuery, phashStr, string(exifJSON), string(duplicatesJSON), imageFileID)
	}

	if err != nil {
		return fmt.Errorf("failed to update image metadata: %w", err)
	}

	log.Printf("pHash and Metadata updated for %s: phash=%s, similar count=%d", imageFileID, phashStr, len(duplicates))
	return nil
}

func UpdateImageVectorOnly(cfg Config, imageFileID string, imageVector []float64) error {
	db, err := getDBConnection(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to db: %w", err)
	}
	defer db.Close()

	tableName := cfg.QueriedDbTable
	vectorBytes, _ := json.Marshal(imageVector)
	vectorStr := string(vectorBytes)

	updateQuery := fmt.Sprintf(`
		UPDATE "%s"
		SET "imageVector" = $1::vector,
		    "imageVectorUpdatedAt" = NOW(),
		    "imageVectorStatus" = 'success',
		    "imageVectorFailReason" = NULL
		WHERE "imageFile_id" = $2
	`, tableName)
	if _, err := db.Exec(updateQuery, vectorStr, imageFileID); err != nil {
		return fmt.Errorf("failed to update image vector: %w", err)
	}
	return nil
}

func buildListImageVectorBackfillQuery(tableName string, input ListImageVectorBackfillCandidatesInput) (string, []interface{}, error) {
	conds := []string{}
	args := []interface{}{}
	argN := 1
	conds = append(conds, `"imageFile_id" IS NOT NULL AND btrim("imageFile_id") != ''`)

	switch strings.ToLower(strings.TrimSpace(input.Mode)) {
	case "missing":
		conds = append(conds, `"imageVector" IS NULL`)
	case "failed":
		conds = append(conds, `"imageVectorStatus" = 'failed'`)
	case "all":
	default:
		return "", nil, fmt.Errorf("invalid mode: %s", input.Mode)
	}

	if input.MaxRetries > 0 {
		conds = append(conds, fmt.Sprintf(`COALESCE("imageVectorRetryCount", 0) < $%d`, argN))
		args = append(args, input.MaxRetries)
		argN++
	}

	if input.OnlyOlderThanMins > 0 {
		conds = append(conds, fmt.Sprintf(`("imageVectorFailedAt" IS NULL OR "imageVectorFailedAt" <= NOW() - ($%d || ' minutes')::interval)`, argN))
		args = append(args, input.OnlyOlderThanMins)
		argN++
	}

	if strings.TrimSpace(input.Cursor) != "" {
		cursorID, parseErr := strconv.ParseInt(strings.TrimSpace(input.Cursor), 10, 64)
		if parseErr != nil {
			return "", nil, fmt.Errorf("%w: must be integer", ErrInvalidCursor)
		}
		conds = append(conds, fmt.Sprintf(`id > $%d`, argN))
		args = append(args, cursorID)
		argN++
	}

	whereClause := ""
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT
			id,
			"imageFile_id",
			COALESCE("imageFile_extension", '')
		FROM "%s"
		%s
		ORDER BY id ASC
		LIMIT $%d
	`, tableName, whereClause, argN)
	args = append(args, input.Limit)
	return query, args, nil
}

func ListImageVectorBackfillCandidates(cfg Config, input ListImageVectorBackfillCandidatesInput) ([]ImageVectorBackfillCandidate, error) {
	db, err := getDBConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to db: %w", err)
	}
	defer db.Close()

	tableName := cfg.QueriedDbTable
	query, args, err := buildListImageVectorBackfillQuery(tableName, input)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query backfill candidates: %w", err)
	}
	defer rows.Close()

	items := make([]ImageVectorBackfillCandidate, 0, input.Limit)
	for rows.Next() {
		var item ImageVectorBackfillCandidate
		var id int64
		if err := rows.Scan(&id, &item.ImageFileID, &item.ImageFileExtension); err != nil {
			return nil, fmt.Errorf("scan backfill candidate: %w", err)
		}
		item.ID = strconv.FormatInt(id, 10)
		item.ImageBucket = cfg.ImageBucket
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backfill candidates: %w", err)
	}

	return items, nil
}

func MarkImageVectorBackfillAttempt(cfg Config, imageFileID string) error {
	db, err := getDBConnection(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to db: %w", err)
	}
	defer db.Close()

	tableName := cfg.QueriedDbTable
	query := fmt.Sprintf(`
		UPDATE "%s"
		SET "imageVectorStatus" = 'pending',
		    "imageVectorRetryCount" = COALESCE("imageVectorRetryCount", 0) + 1
		WHERE "imageFile_id" = $1
	`, tableName)
	if _, err := db.Exec(query, imageFileID); err != nil {
		return fmt.Errorf("mark backfill attempt: %w", err)
	}
	return nil
}

func MarkImageVectorBackfillFailed(cfg Config, imageFileID, reason string, failedAt time.Time) error {
	db, err := getDBConnection(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to db: %w", err)
	}
	defer db.Close()

	tableName := cfg.QueriedDbTable
	query := fmt.Sprintf(`
		UPDATE "%s"
		SET "imageVectorStatus" = 'failed',
		    "imageVectorFailedAt" = $1,
		    "imageVectorFailReason" = $2
		WHERE "imageFile_id" = $3
	`, tableName)
	if _, err := db.Exec(query, failedAt, reason, imageFileID); err != nil {
		return fmt.Errorf("mark backfill failed: %w", err)
	}
	return nil
}
