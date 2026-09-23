package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/corona10/goimagehash"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"golang.org/x/oauth2"
)

func TestPhotoJobOptionsSafeDefaults(t *testing.T) {
	opts, err := loadPhotoJobOptions()
	if err != nil || opts.Mode != "check" || opts.MaxItems != 50 {
		t.Fatalf("%+v %v", opts, err)
	}
	t.Setenv("CLOUD_RUN_TASK_COUNT", "2")
	if _, err = loadPhotoJobOptions(); err == nil {
		t.Fatal("parallel tasks should be rejected")
	}
}

func TestPhotoJobPHashMatchesOnlineAlgorithm(t *testing.T) {
	data := jpegFixture(t)
	hash, encoded, err := photoJobPrepareOriginal(data, "jpg", 60000000)
	if err != nil {
		t.Fatal(err)
	}
	src, _, _ := image.Decode(strings.NewReader(string(data)))
	want, _ := goimagehash.PerceptionHash(resizeImage(applyEXIFOrientation(src, decodeExif(data)), 480))
	if hash != fmt.Sprintf("%016x", want.GetHash()) || len(encoded) == 0 {
		t.Fatal("inconsistent pHash")
	}
	if _, _, err = photoJobPrepareOriginal(data, "jpg", 1); err == nil {
		t.Fatal("pixel limit bypassed")
	}
}

func TestPhotoJobVectorValidation(t *testing.T) {
	v := make([]float64, 512)
	if photoJobVectorValid(v) {
		t.Fatal("zero vector accepted")
	}
	v[2] = 1
	if !photoJobVectorValid(v) {
		t.Fatal("valid vector rejected")
	}
	v[1] = math.NaN()
	if photoJobVectorValid(v) {
		t.Fatal("NaN accepted")
	}
}

func TestPhotoJobPermanentFailureNotRetried(t *testing.T) {
	calls := 0
	err := photoJobRetry(context.Background(), 3, func(context.Context) error {
		calls++
		return errors.New("vision server error (403): permission denied")
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestPhotoJobPartialFailureKeepsOtherComponentsAndNeverUploads(t *testing.T) {
	storage, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http", InitialObjects: []fakestorage.Object{{ObjectAttrs: fakestorage.ObjectAttrs{BucketName: "test", Name: "images/test.jpg"}, Content: jpegFixture(t)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Stop()
	vector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", 403) }))
	defer vector.Close()
	t.Setenv("VECTOR_PORT", vector.URL[strings.LastIndex(vector.URL, ":")+1:])
	vision := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"responses": []any{map[string]any{"labelAnnotations": []any{map[string]any{"description": "Tree", "score": .95}}}}})
	}))
	defer vision.Close()
	previousEndpoint, previousToken := visionAPIEndpoint, visionTokenSource
	visionAPIEndpoint = vision.URL
	visionTokenSource = func() (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}), nil
	}
	defer func() { visionAPIEndpoint = previousEndpoint; visionTokenSource = previousToken }()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, _ := db.Conn(context.Background())
	defer conn.Close()
	mock.ExpectExec(`UPDATE "Photo" SET phash=\$1.*COALESCE\(phash,''\)=''`).WithArgs(sqlmock.AnyArg(), int64(9), "test", "jpg").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE "Photo" SET "imageLabelRawResult"=.*COALESCE\("imageLabelStatus",''\) <> 'success'`).WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), int64(9), "test", "jpg").WillReturnResult(sqlmock.NewResult(0, 1))
	err = processPhotoJobItem(context.Background(), conn, storage.Client(), Config{ImageBucket: "test", MaxSourcePixels: 60000000, ImageLabelMinScore: .75}, photoJobOptions{Attempts: 1}, photoJobItem{ID: 9, FileID: "test", Extension: "jpg", NeedHash: true, NeedVector: true, NeedLabels: true})
	if err == nil || !strings.Contains(err.Error(), "vector:") {
		t.Fatalf("%v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	objects, _, err := storage.ListObjects("test", "", "", false)
	if err != nil || len(objects) != 1 {
		t.Fatalf("unexpected GCS writes: %d %v", len(objects), err)
	}
}

func TestPhotoJobVectorRequestCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer srv.Close()
	t.Setenv("VECTOR_PORT", srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := ComputeImageVectorContext(ctx, []byte("x"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestPhotoJobDateBounds(t *testing.T) {
	t.Setenv("BACKFILL_CREATED_FROM", "2026-01-01T00:00:00+08:00")
	t.Setenv("BACKFILL_CREATED_BEFORE", "2027-01-01T00:00:00+08:00")
	opts, err := loadPhotoJobOptions()
	if err != nil || opts.CreatedFrom.Format(time.RFC3339) != "2025-12-31T16:00:00Z" {
		t.Fatalf("%+v %v", opts, err)
	}
	t.Setenv("BACKFILL_CREATED_BEFORE", "2025-01-01T00:00:00+08:00")
	if _, err = loadPhotoJobOptions(); err == nil {
		t.Fatal("reversed dates accepted")
	}
	t.Setenv("BACKFILL_CREATED_FROM", "2026-01-01")
	if _, err = loadPhotoJobOptions(); err == nil {
		t.Fatal("timezone-free date accepted")
	}
}

func TestPhotoJobPHashShardsRequireFixedEightTasks(t *testing.T) {
	t.Setenv("BACKFILL_FIELDS", "phash")
	if _, err := loadPhotoJobOptions(); err == nil {
		t.Fatal("missing shard count accepted")
	}
	t.Setenv("CLOUD_RUN_TASK_COUNT", "8")
	t.Setenv("CLOUD_RUN_TASK_INDEX", "7")
	opts, err := loadPhotoJobOptions()
	if err != nil || opts.ShardIndex != 7 || opts.ShardCount != 8 || opts.Fields != "phash" {
		t.Fatalf("%+v %v", opts, err)
	}
	t.Setenv("CLOUD_RUN_TASK_INDEX", "8")
	if _, err := loadPhotoJobOptions(); err == nil {
		t.Fatal("invalid shard index accepted")
	}
	t.Setenv("BACKFILL_FIELDS", "ai")
	if _, err := loadPhotoJobOptions(); err == nil {
		t.Fatal("sharding accepted for AI fields")
	}
}
