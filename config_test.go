package main

import (
	"os"
	"testing"
)

func TestLoadConfig_DefaultsAndWatermarkError(t *testing.T) {
	defer func() {
		for _, k := range []string{
			"RESIZE_TARGETS", "ENABLE_WATERMARK", "WATERMARK_PATH", "PORT",
			"ENABLE_IMAGE_VECTOR", "ENABLE_IMAGE_LABEL", "IMAGE_LABEL_MIN_SCORE",
			"IMAGE_LABEL_MAX_RESULTS", "IMAGE_BUCKET", "BACKFILL_API_KEY",
			"MAX_SOURCE_PIXELS",
		} {
			_ = os.Unsetenv(k)
		}
	}()
	t.Setenv("RESIZE_TARGETS", "w100")
	t.Setenv("ENABLE_WATERMARK", "true")
	t.Setenv("WATERMARK_PATH", "")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected watermark path error")
	}

	t.Setenv("RESIZE_TARGETS", "w200")
	t.Setenv("ENABLE_WATERMARK", "false")
	t.Setenv("PORT", "9090")
	t.Setenv("ENABLE_IMAGE_VECTOR", "true")
	t.Setenv("ENABLE_IMAGE_LABEL", "true")
	t.Setenv("IMAGE_LABEL_MIN_SCORE", "0.8")
	t.Setenv("IMAGE_LABEL_MAX_RESULTS", "7")
	t.Setenv("IMAGE_BUCKET", "  bk  ")
	t.Setenv("BACKFILL_API_KEY", " key ")
	t.Setenv("MAX_SOURCE_PIXELS", "12345")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "9090" || !cfg.EnableImageVector {
		t.Fatalf("%+v", cfg)
	}
	if !cfg.EnableImageLabel || cfg.ImageLabelMinScore != 0.8 || cfg.ImageLabelMaxResults != 7 {
		t.Fatalf("%+v", cfg)
	}
	if cfg.ImageBucket != "bk" || cfg.BackfillAPIKey != "key" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.MaxSourcePixels != 12345 {
		t.Fatalf("%+v", cfg)
	}
}

func TestParseResizeTargets(t *testing.T) {
	targets, err := ParseResizeTargets("w480,w800,w1200")
	if err != nil {
		t.Fatalf("ParseResizeTargets returned error: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	if targets[1].Label != "w800" || targets[1].Width != 800 {
		t.Fatalf("unexpected target: %+v", targets[1])
	}
}

func TestParseResizeTargetsRejectsInvalidValue(t *testing.T) {
	if _, err := ParseResizeTargets("480"); err == nil {
		t.Fatal("expected invalid target to return an error")
	}
}

func TestParseResizeTargets_NoTargets(t *testing.T) {
	if _, err := ParseResizeTargets(", , "); err == nil {
		t.Fatal("expected error for empty targets")
	}
}

func TestParseResizeTargets_InvalidWidth(t *testing.T) {
	if _, err := ParseResizeTargets("w0"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseBoolEnv(t *testing.T) {
	key := "IMAGE_PROCESSOR_PARSE_BOOL_TEST"
	_ = os.Unsetenv(key)
	if !parseBoolEnv(key, true) {
		t.Fatal("empty should use fallback true")
	}
	t.Setenv(key, "TRUE")
	if !parseBoolEnv(key, false) {
		t.Fatal()
	}
	t.Setenv(key, "0")
	if parseBoolEnv(key, true) {
		t.Fatal()
	}
	t.Setenv(key, "garbage")
	if !parseBoolEnv(key, true) {
		t.Fatal("invalid should return fallback")
	}
	_ = os.Unsetenv(key)
}

func TestParseFloatEnv(t *testing.T) {
	key := "IMAGE_PROCESSOR_PARSE_FLOAT_TEST"
	_ = os.Unsetenv(key)
	if parseFloatEnv(key, 1.25) != 1.25 {
		t.Fatal()
	}
	t.Setenv(key, "3.5")
	if parseFloatEnv(key, 0) != 3.5 {
		t.Fatal()
	}
	t.Setenv(key, "x")
	if parseFloatEnv(key, 9) != 9 {
		t.Fatal("invalid should return fallback")
	}
	_ = os.Unsetenv(key)
}
