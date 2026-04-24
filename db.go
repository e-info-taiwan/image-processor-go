package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	_ "github.com/lib/pq"
)

type Duplicate struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func getDBConnection(cfg Config) (*sql.DB, error) {
	connStr := fmt.Sprintf("host=%s dbname=%s user=%s password=%s sslmode=disable",
		cfg.DbHost, cfg.DbName, cfg.DbUser, cfg.DbPassword)
	return sql.Open("postgres", connStr)
}

func UpdateImageMetadata(cfg Config, imageFileID, phashStr, bucketName string, exifData map[string]interface{}, imageVector []float64) error {
	db, err := getDBConnection(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to db: %w", err)
	}
	defer db.Close()

	tableName := cfg.QueriedDbTable

	// 1. Find similar images with Hamming distance <= 5
	query := fmt.Sprintf(`
		SELECT id, "imageFile_id", "imageFile_extension" FROM "%s"
		WHERE phash IS NOT NULL 
		  AND phash != ''
		  AND "imageFile_id" != $1
		  AND bit_count(('x' || phash)::bit(64) # ('x' || $2)::bit(64)) <= 5
	`, tableName)

	rows, err := db.Query(query, imageFileID, phashStr)
	if err != nil {
		return fmt.Errorf("failed to query similar images: %w", err)
	}
	defer rows.Close()

	var duplicates []Duplicate
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

	// 2. Update DB with new phash, exif, possibleDuplicates, and imageVector
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
