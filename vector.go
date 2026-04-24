package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

type VectorResponse struct {
	Vector []float64 `json:"vector"`
}

func ComputeImageVector(imageBytes []byte) ([]float64, error) {
	port := os.Getenv("VECTOR_PORT")
	if port == "" {
		port = "8081"
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/vectorize", port)

	req, err := http.NewRequest("POST", url, bytes.NewReader(imageBytes))
	if err != nil {
		return nil, fmt.Errorf("create vector request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do vector request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vector server error (%d): %s", resp.StatusCode, string(body))
	}

	var vecResp VectorResponse
	if err := json.NewDecoder(resp.Body).Decode(&vecResp); err != nil {
		return nil, fmt.Errorf("decode vector response: %w", err)
	}

	return vecResp.Vector, nil
}
