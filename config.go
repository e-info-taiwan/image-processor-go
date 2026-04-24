package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                 string
	ResizeTargets        []ResizeTarget
	EnableWatermark      bool
	WatermarkPath        string
	WatermarkScale       float64
	WatermarkMarginRatio float64
	WatermarkOpacity     float64
	CacheControl         string
	EnableImageVector    bool
	DbHost               string
	DbName               string
	DbUser               string
	DbPassword           string
	QueriedDbTable       string
}

type ResizeTarget struct {
	Label string
	Width int
}

func LoadConfig() (Config, error) {
	targets, err := ParseResizeTargets(envOrDefault("RESIZE_TARGETS", "w480,w800,w1200,w1600,w2400"))
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Port:                 envOrDefault("PORT", "8080"),
		ResizeTargets:        targets,
		EnableWatermark:      parseBoolEnv("ENABLE_WATERMARK", false),
		WatermarkPath:        os.Getenv("WATERMARK_PATH"),
		WatermarkScale:       parseFloatEnv("WATERMARK_SCALE", 0.15),
		WatermarkMarginRatio: parseFloatEnv("WATERMARK_MARGIN_RATIO", 0.025),
		WatermarkOpacity:     parseFloatEnv("WATERMARK_OPACITY", 1.0),
		CacheControl:         envOrDefault("CACHE_CONTROL", "public, max-age=31536000"),
		EnableImageVector:    parseBoolEnv("ENABLE_IMAGE_VECTOR", false),
		DbHost:               envOrDefault("DB_HOST", "localhost"),
		DbName:               envOrDefault("DB_NAME", "postgres"),
		DbUser:               envOrDefault("DB_USER", "postgres"),
		DbPassword:           os.Getenv("DB_PASSWORD"),
		QueriedDbTable:       envOrDefault("QUERIED_DB_TABLE", "Photo"),
	}

	if cfg.EnableWatermark && cfg.WatermarkPath == "" {
		return Config{}, fmt.Errorf("WATERMARK_PATH is required when ENABLE_WATERMARK=true")
	}

	return cfg, nil
}

func ParseResizeTargets(raw string) ([]ResizeTarget, error) {
	parts := strings.Split(raw, ",")
	targets := make([]ResizeTarget, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.HasPrefix(part, "w") {
			return nil, fmt.Errorf("invalid resize target %q", part)
		}

		width, err := strconv.Atoi(strings.TrimPrefix(part, "w"))
		if err != nil || width <= 0 {
			return nil, fmt.Errorf("invalid resize target %q", part)
		}

		targets = append(targets, ResizeTarget{
			Label: part,
			Width: width,
		})
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("no resize targets configured")
	}
	return targets, nil
}

func parseBoolEnv(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func parseFloatEnv(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return value
}
