package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDecodeStorageEvent_DirectJSON(t *testing.T) {
	t.Parallel()
	body := []byte(`{"bucket":"b","name":"n.jpg","contentType":"image/jpeg","generation":"123"}`)
	ev, err := DecodeStorageEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Bucket != "b" || ev.Name != "n.jpg" || ev.ContentType != "image/jpeg" || ev.Generation != "123" {
		t.Fatalf("%+v", ev)
	}
}

func TestDecodeStorageEvent_PubSub(t *testing.T) {
	t.Parallel()
	inner := []byte(`{"bucket":"bb","name":"x.png"}`)
	env := map[string]interface{}{
		"message": map[string]interface{}{
			"data": base64.StdEncoding.EncodeToString(inner),
			"attributes": map[string]string{
				"contentType":      "image/png",
				"objectGeneration": "456",
			},
		},
	}
	body, _ := json.Marshal(env)
	ev, err := DecodeStorageEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Bucket != "bb" || ev.Name != "x.png" || ev.ContentType != "image/png" || ev.Generation != "456" {
		t.Fatalf("%+v", ev)
	}
}

func TestDecodeStorageEvent_Errors(t *testing.T) {
	t.Parallel()
	if _, err := DecodeStorageEvent([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty event")
	}
	if _, err := DecodeStorageEvent([]byte(`{"message":{}}`)); err == nil {
		t.Fatal("expected missing data")
	}
	badB64, _ := json.Marshal(map[string]interface{}{
		"message": map[string]string{"data": "@@@"},
	})
	if _, err := DecodeStorageEvent(badB64); err == nil {
		t.Fatal("expected b64 error")
	}
	inner, _ := json.Marshal(map[string]string{"bucket": "", "name": ""})
	env, _ := json.Marshal(map[string]interface{}{
		"message": map[string]string{"data": base64.StdEncoding.EncodeToString(inner)},
	})
	if _, err := DecodeStorageEvent(env); err == nil {
		t.Fatal("expected missing bucket/name")
	}
}

func TestContentTypeFromExt(t *testing.T) {
	t.Parallel()
	if contentTypeFromExt(".JPG") != "image/jpeg" {
		t.Fatal()
	}
	if contentTypeFromExt(".JPEG") != "image/jpeg" || contentTypeFromExt(".Jpg") != "image/jpeg" {
		t.Fatal()
	}
	if contentTypeFromExt(".PNG") != "image/png" || contentTypeFromExt(".Gif") != "image/gif" {
		t.Fatal()
	}
	if contentTypeFromExt(".tif") != "image/tiff" || contentTypeFromExt(".TIF") != "image/tiff" {
		t.Fatal()
	}
	if contentTypeFromExt(".webp") != "image/webp" || contentTypeFromExt(".WEBP") != "image/webp" {
		t.Fatal()
	}
	if contentTypeFromExt(".unknown") != "application/octet-stream" {
		t.Fatal()
	}
}

func TestEnvOrDefault(t *testing.T) {
	key := "IMAGE_PROCESSOR_TEST_ENV_OR_DEFAULT_XYZ"
	_ = os.Unsetenv(key)
	if envOrDefault(key, "fb") != "fb" {
		t.Fatal()
	}
	t.Setenv(key, "  set  ")
	if envOrDefault(key, "fb") != "set" {
		t.Fatal()
	}
	_ = os.Unsetenv(key)
}

func TestHealthHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	healthHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestEventHandler_MethodNotAllowed(t *testing.T) {
	h := eventHandler(&Processor{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
}

type errReadCloser struct{}

func (errReadCloser) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func (errReadCloser) Close() error { return nil }

func TestEventHandler_ReadBodyError(t *testing.T) {
	h := eventHandler(&Processor{})
	req := httptest.NewRequest(http.MethodPost, "/", errReadCloser{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
}

func TestEventHandler_DecodeError(t *testing.T) {
	h := eventHandler(&Processor{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString("not-json"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "decode") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
