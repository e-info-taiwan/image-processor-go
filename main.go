package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"cloud.google.com/go/storage"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx := context.Background()
	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("create storage client: %v", err)
	}
	defer storageClient.Close()

	processor, err := NewProcessor(cfg, storageClient)
	if err != nil {
		log.Fatalf("create processor: %v", err)
	}

	server := newHTTPServer(":"+cfg.Port, newRouter(processor))

	log.Printf("image processor listening on :%s", cfg.Port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func eventHandler(processor *Processor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, "read request body failed", http.StatusBadRequest)
			return
		}

		event, err := DecodeStorageEvent(body)
		if err != nil {
			http.Error(w, fmt.Sprintf("decode event failed: %v", err), http.StatusBadRequest)
			return
		}

		if err := processor.Process(r.Context(), event); err != nil {
			log.Printf("process event failed: bucket=%s name=%s err=%v", event.Bucket, event.Name, err)
			http.Error(w, "image processing failed", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

type pubsubEnvelope struct {
	Message struct {
		Data       string            `json:"data"`
		Attributes map[string]string `json:"attributes"`
	} `json:"message"`
}

type storageEvent struct {
	Bucket      string `json:"bucket"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Generation  string `json:"generation"`
}

func DecodeStorageEvent(body []byte) (storageEvent, error) {
	var event storageEvent
	if err := json.Unmarshal(body, &event); err == nil && event.Bucket != "" && event.Name != "" {
		return event, nil
	}

	var envelope pubsubEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return storageEvent{}, err
	}
	if envelope.Message.Data == "" {
		return storageEvent{}, errors.New("missing message.data")
	}

	raw, err := base64.StdEncoding.DecodeString(envelope.Message.Data)
	if err != nil {
		return storageEvent{}, err
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return storageEvent{}, err
	}
	if event.Bucket == "" || event.Name == "" {
		return storageEvent{}, errors.New("storage event missing bucket or name")
	}
	if event.ContentType == "" {
		event.ContentType = envelope.Message.Attributes["contentType"]
	}
	if event.Generation == "" {
		event.Generation = envelope.Message.Attributes["objectGeneration"]
	}
	if event.Generation == "" {
		event.Generation = envelope.Message.Attributes["object_generation"]
	}
	return event, nil
}

func contentTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
