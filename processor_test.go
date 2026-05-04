package main

import (
	"errors"
	"image"
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

func TestHandleW480MetadataWritesMetadataBeforeVector(t *testing.T) {
	var mu sync.Mutex
	events := []string{}
	vectorDone := make(chan struct{})

	prevUpdateMetadata := updateImageMetadata
	prevComputeVector := computeImageVector
	prevUpdateVector := updateImageVectorOnly
	t.Cleanup(func() {
		updateImageMetadata = prevUpdateMetadata
		computeImageVector = prevComputeVector
		updateImageVectorOnly = prevUpdateVector
	})

	updateImageMetadata = func(Config, string, string, string, map[string]interface{}, []float64) error {
		mu.Lock()
		if len(events) == 2 {
			events = append(events, "metadata-vector")
			close(vectorDone)
		} else {
			events = append(events, "metadata")
		}
		mu.Unlock()
		return nil
	}
	computeImageVector = func([]byte) ([]float64, error) {
		mu.Lock()
		events = append(events, "compute-vector")
		mu.Unlock()
		return []float64{0.1, 0.2}, nil
	}
	updateImageVectorOnly = func(Config, string, []float64) error {
		t.Fatal("upload vector path should update metadata so possibleDuplicates can include vector matches")
		return nil
	}

	p := &Processor{cfg: Config{EnableImageVector: true}}
	p.handleW480Metadata("images/a.jpg", "bucket", "a", image.NewRGBA(image.Rect(0, 0, 8, 8)), map[string]interface{}{"k": "v"}, []byte("w480"))

	select {
	case <-vectorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vector update")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("unexpected events: %v", events)
	}
	want := []string{"metadata", "compute-vector", "metadata-vector"}
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
