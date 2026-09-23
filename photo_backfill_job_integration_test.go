package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

func TestPhotoJobRealPostgres(t *testing.T) {
	dsn := os.Getenv("BACKFILL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires isolated localhost eic_backfill_test database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/eic_backfill_test" {
		t.Fatal("refusing non-test database")
	}
	db, err := sql.Open("postgres", dsn+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `CREATE TABLE "Photo" (id SERIAL PRIMARY KEY,"imageFile_id" TEXT,"imageFile_extension" TEXT,phash TEXT NOT NULL DEFAULT '',"imageVector" vector(512))`)
	if err != nil {
		t.Fatal(err)
	}
	opts := photoJobOptions{ExpectedDatabase: "eic_backfill_test", Mode: "check", Attempts: 1}
	if err = photoJobPreflight(ctx, conn, opts); err == nil || !strings.Contains(err.Error(), "schema is not ready") {
		t.Fatalf("expected missing migration: %v", err)
	}
	_, err = conn.ExecContext(ctx, `ALTER TABLE "Photo"
        ADD "imageVectorStatus" TEXT NOT NULL DEFAULT '', ADD "imageVectorFailReason" TEXT,
        ADD "imageVectorUpdatedAt" TIMESTAMP(3), ADD "imageLabelRawResult" JSONB,
        ADD "imageLabelSuggestions" JSONB, ADD "imageLabelStatus" TEXT NOT NULL DEFAULT '',
        ADD "imageLabelFailReason" TEXT, ADD "imageLabelUpdatedAt" TIMESTAMP(3)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = photoJobPreflight(ctx, conn, opts); err != nil {
		t.Fatal(err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO "Photo" ("imageFile_id","imageFile_extension") VALUES ('test','jpg')`)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http", InitialObjects: []fakestorage.Object{{ObjectAttrs: fakestorage.ObjectAttrs{BucketName: "test", Name: "images/test.jpg"}, Content: jpegFixture(t)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Stop()
	item := photoJobItem{ID: 1, FileID: "test", Extension: "jpg", NeedHash: true}
	cfg := Config{ImageBucket: "test", MaxSourcePixels: 60000000}
	if err = processPhotoJobItem(ctx, conn, storage.Client(), cfg, opts, item); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err = conn.QueryRowContext(ctx, `SELECT phash FROM "Photo" WHERE id=1`).Scan(&hash); err != nil || len(hash) != 16 {
		t.Fatalf("hash=%s err=%v", hash, err)
	}
	_, err = conn.ExecContext(ctx, `UPDATE "Photo" SET "imageFile_id"='replacement',phash='' WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if err = processPhotoJobItem(ctx, conn, storage.Client(), cfg, opts, item); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRowContext(ctx, `SELECT phash FROM "Photo" WHERE id=1`).Scan(&hash); err != nil || hash != "" {
		t.Fatalf("stale source was written: hash=%s err=%v", hash, err)
	}
	rows, err := conn.QueryContext(ctx, photoJobSelect, 0, 10, 25)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("missing row not eligible for resume")
	}
}
