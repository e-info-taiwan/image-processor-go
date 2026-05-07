package main

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fsouza/fake-gcs-server/fakestorage"
)

func jpegFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcess_WithFakeGCS(t *testing.T) {
	jpg := jpegFixture(t)
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		InitialObjects: []fakestorage.Object{
			{
				ObjectAttrs: fakestorage.ObjectAttrs{
					BucketName: "test-bucket",
					Name:       "images/pipe.jpg",
				},
				Content: jpg,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	client := srv.Client()
	cfg := Config{
		ResizeTargets:     []ResizeTarget{{Label: "w480", Width: 24}},
		EnableWatermark:   false,
		CacheControl:      "public, max-age=1",
		EnableImageVector: false,
	}
	p, err := NewProcessor(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx); err != nil {
		t.Fatal("expected resized output:", err)
	}
}

func TestProcess_SkipsDuplicateSourceGeneration(t *testing.T) {
	jpg := jpegFixture(t)
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		InitialObjects: []fakestorage.Object{
			{
				ObjectAttrs: fakestorage.ObjectAttrs{
					BucketName: "test-bucket",
					Name:       "images/pipe.jpg",
				},
				Content: jpg,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	client := srv.Client()
	cfg := Config{
		ResizeTargets:     []ResizeTarget{{Label: "w480", Width: 24}},
		EnableWatermark:   false,
		CacheControl:      "public, max-age=1",
		EnableImageVector: false,
	}
	p, err := NewProcessor(cfg, client)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "111"}); err != nil {
		t.Fatal(err)
	}

	firstAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if firstAttrs.Metadata["sourceGeneration"] != "111" {
		t.Fatalf("expected sourceGeneration metadata, got %+v", firstAttrs.Metadata)
	}

	if err := client.Bucket("test-bucket").Object("images/pipe-w480.webP").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "111"}); err != nil {
		t.Fatal(err)
	}
	recoveredAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredAttrs.Generation == firstAttrs.Generation {
		t.Fatal("missing completion sentinel should allow retry to rebuild outputs")
	}

	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "111"}); err != nil {
		t.Fatal(err)
	}
	duplicateAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if duplicateAttrs.Generation != recoveredAttrs.Generation {
		t.Fatalf("duplicate source generation should not rewrite output: recovered=%d duplicate=%d", recoveredAttrs.Generation, duplicateAttrs.Generation)
	}

	if err := p.Process(ctx, storageEvent{Bucket: "test-bucket", Name: "images/pipe.jpg", Generation: "222"}); err != nil {
		t.Fatal(err)
	}
	newAttrs, err := client.Bucket("test-bucket").Object("images/pipe-w480.jpg").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newAttrs.Generation == duplicateAttrs.Generation {
		t.Fatal("new source generation should rewrite output")
	}
	if newAttrs.Metadata["sourceGeneration"] != "222" {
		t.Fatalf("expected updated sourceGeneration metadata, got %+v", newAttrs.Metadata)
	}
}

func TestBackfillImageVectorFromObject_WithFakeGCS(t *testing.T) {
	jpg := jpegFixture(t)
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		InitialObjects: []fakestorage.Object{
			{
				ObjectAttrs: fakestorage.ObjectAttrs{
					BucketName: "bk",
					Name:       "images/abc-w480.jpg",
				},
				Content: jpg,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	vecSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vectorize" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(VectorResponse{Vector: []float64{0.1, 0.2, 0.3}})
	}))
	defer vecSrv.Close()
	host := strings.TrimPrefix(vecSrv.URL, "http://")
	t.Setenv("VECTOR_PORT", host[strings.LastIndex(host, ":")+1:])
	t.Cleanup(func() { _ = os.Unsetenv("VECTOR_PORT") })

	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(sqlmock.AnyArg(), driver.Value("abc")).
			WillReturnResult(sqlmock.NewResult(0, 1))

		client := srv.Client()
		p := &Processor{
			cfg:     Config{QueriedDbTable: "Photo"},
			storage: client,
		}
		ctx := context.Background()
		err := p.BackfillImageVectorFromObject(ctx, "bk", "images/abc-w480.jpg", "abc")
		if err != nil {
			t.Fatal(err)
		}
	})
}
