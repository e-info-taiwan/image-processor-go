package main

import (
	"errors"
	"image"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsSupportedImage(t *testing.T) {
	if !isSupportedImage("images/demo.JPG") {
		t.Fatal("expected JPG to be supported")
	}
	if !isSupportedImage("images/demo.webp") {
		t.Fatal("expected user-uploaded lowercase webp to be supported")
	}
	if isSupportedImage("images/demo.webP") {
		t.Fatal("expected generated mixed-case webP to be skipped")
	}
	if isSupportedImage("images/demo.txt") {
		t.Fatal("expected TXT to be unsupported")
	}
}

func TestDerivedObjectPattern(t *testing.T) {
	if !derivedObjectPattern.MatchString("images/demo-w800.jpg") {
		t.Fatal("expected derived pattern to match resized image")
	}
	if derivedObjectPattern.MatchString("images/demo.jpg") {
		t.Fatal("expected original image not to match derived pattern")
	}
}

func TestHandleW480MetadataWritesMetadataBeforeVectorOnlyUpdate(t *testing.T) {
	var mu sync.Mutex
	events := []string{}
	vectorDone := make(chan struct{})
	errCh := make(chan string, 1)

	prevUpdateMetadata := updateImageMetadata
	prevComputeVector := computeImageVector
	prevUpdateVector := updateImageVectorOnly
	t.Cleanup(func() {
		updateImageMetadata = prevUpdateMetadata
		computeImageVector = prevComputeVector
		updateImageVectorOnly = prevUpdateVector
	})

	recordUnexpected := func(msg string) {
		select {
		case errCh <- msg:
		default:
		}
	}

	updateImageMetadata = func(_ Config, _ string, _ string, _ string, _ map[string]interface{}, imageVector []float64) error {
		if len(imageVector) > 0 {
			recordUnexpected("metadata update should not receive image vector")
		}
		mu.Lock()
		events = append(events, "metadata")
		mu.Unlock()
		return nil
	}
	computeImageVector = func([]byte) ([]float64, error) {
		mu.Lock()
		events = append(events, "compute-vector")
		mu.Unlock()
		return []float64{0.1, 0.2}, nil
	}
	updateImageVectorOnly = func(_ Config, _ string, imageVector []float64) error {
		if len(imageVector) != 2 {
			recordUnexpected("vector-only update should receive computed vector")
		}
		mu.Lock()
		events = append(events, "vector-only")
		close(vectorDone)
		mu.Unlock()
		return nil
	}

	p := &Processor{cfg: Config{EnableImageVector: true}}
	p.handleW480Metadata("images/a.jpg", "bucket", "a", image.NewRGBA(image.Rect(0, 0, 8, 8)), map[string]interface{}{"k": "v"}, []byte("w480"))

	select {
	case <-vectorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vector update")
	}
	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("unexpected events: %v", events)
	}
	want := []string{"metadata", "compute-vector", "vector-only"}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("metadata should be written before vector work, got %v", events)
		}
	}
}

func TestHandleW480MetadataKeepsMetadataWhenVectorFails(t *testing.T) {
	metadataDone := make(chan struct{})
	vectorAttempted := make(chan struct{})

	prevUpdateMetadata := updateImageMetadata
	prevComputeVector := computeImageVector
	prevUpdateVector := updateImageVectorOnly
	t.Cleanup(func() {
		updateImageMetadata = prevUpdateMetadata
		computeImageVector = prevComputeVector
		updateImageVectorOnly = prevUpdateVector
	})

	updateImageMetadata = func(Config, string, string, string, map[string]interface{}, []float64) error {
		close(metadataDone)
		return nil
	}
	computeImageVector = func([]byte) ([]float64, error) {
		close(vectorAttempted)
		return nil, errors.New("vector oom")
	}
	updateImageVectorOnly = func(Config, string, []float64) error {
		t.Fatal("vector update should not run after compute failure")
		return nil
	}

	p := &Processor{cfg: Config{EnableImageVector: true}}
	p.handleW480Metadata("images/a.jpg", "bucket", "a", image.NewRGBA(image.Rect(0, 0, 8, 8)), nil, []byte("w480"))

	select {
	case <-metadataDone:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata update did not run")
	}
	select {
	case <-vectorAttempted:
	case <-time.After(2 * time.Second):
		t.Fatal("vector compute did not run")
	}
}

func TestHandleW480MetadataUpdatesImageLabels(t *testing.T) {
	labelDone := make(chan struct{})
	var mu sync.Mutex
	events := []string{}

	prevUpdateMetadata := updateImageMetadata
	prevDetectLabels := detectImageLabels
	prevUpdateLabels := updateImageLabels
	prevMarkLabelsFailed := markImageLabelsFailed
	t.Cleanup(func() {
		updateImageMetadata = prevUpdateMetadata
		detectImageLabels = prevDetectLabels
		updateImageLabels = prevUpdateLabels
		markImageLabelsFailed = prevMarkLabelsFailed
	})

	updateImageMetadata = func(Config, string, string, string, map[string]interface{}, []float64) error {
		mu.Lock()
		events = append(events, "metadata")
		mu.Unlock()
		return nil
	}
	detectImageLabels = func(cfg Config, imageBytes []byte) ([]ImageLabel, error) {
		if cfg.ImageLabelMaxResults != 5 || string(imageBytes) != "w480" {
			t.Fatalf("bad label request cfg=%+v bytes=%q", cfg, string(imageBytes))
		}
		mu.Lock()
		events = append(events, "detect-labels")
		mu.Unlock()
		return []ImageLabel{
			{Description: "Owl", Score: 0.9},
			{Description: "Animal", Score: 0.3},
		}, nil
	}
	updateImageLabels = func(_ Config, imageFileID string, labels []ImageLabel, suggestions []ImageLabelSuggestion) error {
		if imageFileID != "a" || len(labels) != 2 || len(suggestions) != 1 || suggestions[0].Tag != "owl" {
			t.Fatalf("unexpected labels update id=%s labels=%+v suggestions=%+v", imageFileID, labels, suggestions)
		}
		mu.Lock()
		events = append(events, "update-labels")
		close(labelDone)
		mu.Unlock()
		return nil
	}
	markImageLabelsFailed = func(Config, string, string) error {
		t.Fatal("label failure should not be marked on success")
		return nil
	}

	p := &Processor{cfg: Config{
		EnableImageLabel:     true,
		ImageLabelMinScore:   0.75,
		ImageLabelMaxResults: 5,
	}}
	p.handleW480Metadata("images/a.jpg", "bucket", "a", image.NewRGBA(image.Rect(0, 0, 8, 8)), nil, []byte("w480"))

	select {
	case <-labelDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for label update")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"metadata", "detect-labels", "update-labels"}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events=%v", events)
		}
	}
}

func TestHandleW480MetadataMarksImageLabelsFailed(t *testing.T) {
	failedDone := make(chan struct{})

	prevUpdateMetadata := updateImageMetadata
	prevDetectLabels := detectImageLabels
	prevUpdateLabels := updateImageLabels
	prevMarkLabelsFailed := markImageLabelsFailed
	t.Cleanup(func() {
		updateImageMetadata = prevUpdateMetadata
		detectImageLabels = prevDetectLabels
		updateImageLabels = prevUpdateLabels
		markImageLabelsFailed = prevMarkLabelsFailed
	})

	updateImageMetadata = func(Config, string, string, string, map[string]interface{}, []float64) error {
		return nil
	}
	detectImageLabels = func(Config, []byte) ([]ImageLabel, error) {
		return nil, errors.New("vision unavailable")
	}
	updateImageLabels = func(Config, string, []ImageLabel, []ImageLabelSuggestion) error {
		t.Fatal("label update should not run after detection failure")
		return nil
	}
	markImageLabelsFailed = func(_ Config, imageFileID, reason string) error {
		if imageFileID != "a" || !strings.Contains(reason, "vision unavailable") {
			t.Fatalf("unexpected failure id=%s reason=%q", imageFileID, reason)
		}
		close(failedDone)
		return nil
	}

	p := &Processor{cfg: Config{EnableImageLabel: true}}
	p.handleW480Metadata("images/a.jpg", "bucket", "a", image.NewRGBA(image.Rect(0, 0, 8, 8)), nil, []byte("w480"))

	select {
	case <-failedDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for label failure")
	}
}
