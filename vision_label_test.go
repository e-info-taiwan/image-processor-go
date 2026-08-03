package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestFilterImageLabelSuggestions(t *testing.T) {
	labels := []ImageLabel{
		{Description: "Animal", Score: 0.92, Topicality: 0.8},
		{Description: "Owl", Score: 0.91, Topicality: 0.9},
		{Description: "owl", Score: 0.95, Topicality: 0.7},
		{Description: "Low score", Score: 0.2},
		{Description: "  ", Score: 0.99},
	}

	got := FilterImageLabelSuggestions(labels, 0.75)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Tag != "owl" || got[0].Label != "owl" || got[0].Score != 0.95 {
		t.Fatalf("expected higher scored duplicate owl first, got %+v", got[0])
	}
	if got[1].Tag != "animal" || got[1].Label != "Animal" {
		t.Fatalf("unexpected second suggestion: %+v", got[1])
	}
}

func TestAddPersonSuggestion(t *testing.T) {
	suggestions := AddPersonSuggestion([]ImageLabelSuggestion{{Tag: "table", Label: "Table", Score: 0.9}}, true)
	if len(suggestions) != 2 || suggestions[0].Tag != "人物" || suggestions[0].Source != "google-cloud-vision-face-detection" {
		t.Fatalf("unexpected person suggestion: %+v", suggestions)
	}

	if got := AddPersonSuggestion(suggestions, false); len(got) != 2 {
		t.Fatalf("hasPerson=false should leave suggestions unchanged: %+v", got)
	}
}

func TestDetectImageLabels_OK(t *testing.T) {
	prevEndpoint := visionAPIEndpoint
	prevTokenSource := visionTokenSource
	t.Cleanup(func() {
		visionAPIEndpoint = prevEndpoint
		visionTokenSource = prevTokenSource
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images:annotate" {
			t.Fatalf("bad path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("bad auth header %q", got)
		}

		var req visionAnnotateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if len(req.Requests) != 1 || len(req.Requests[0].Features) != 4 {
			t.Fatalf("bad request shape: %+v", req)
		}
		if req.Requests[0].Features[0].Type != "LABEL_DETECTION" || req.Requests[0].Features[0].MaxResults != 3 {
			t.Fatalf("bad feature: %+v", req.Requests[0].Features[0])
		}
		if req.Requests[0].Features[1].Type != "FACE_DETECTION" {
			t.Fatalf("bad face feature: %+v", req.Requests[0].Features[1])
		}
		if req.Requests[0].Features[2].Type != "LOGO_DETECTION" || req.Requests[0].Features[3].Type != "LANDMARK_DETECTION" {
			t.Fatalf("bad logo/landmark features: %+v", req.Requests[0].Features)
		}
		if req.Requests[0].Image.Content != base64.StdEncoding.EncodeToString([]byte("img")) {
			t.Fatalf("bad image content")
		}

		_ = json.NewEncoder(w).Encode(visionAnnotateResponse{
			Responses: []visionAnnotateResult{
				{
					LabelAnnotations: []ImageLabel{
						{Description: "Owl", Score: 0.91, Topicality: 0.88, MID: "/m/0"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	visionAPIEndpoint = srv.URL + "/v1/images:annotate"
	visionTokenSource = func() (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}), nil
	}

	labels, err := DetectImageLabels(Config{ImageLabelMaxResults: 3}, []byte("img"))
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].Description != "Owl" || labels[0].Score != 0.91 {
		t.Fatalf("unexpected labels: %+v", labels)
	}
}

func TestDetectImageLabelsWithFaces(t *testing.T) {
	prevEndpoint := visionAPIEndpoint
	prevTokenSource := visionTokenSource
	t.Cleanup(func() {
		visionAPIEndpoint = prevEndpoint
		visionTokenSource = prevTokenSource
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(visionAnnotateResponse{Responses: []visionAnnotateResult{{
			LabelAnnotations:    []ImageLabel{{Description: "Table", Score: 0.95}},
			FaceAnnotations:     []visionFace{{}},
			LogoAnnotations:     []ImageLabel{{Description: "Google", Score: 0.91}},
			LandmarkAnnotations: []ImageLabel{{Description: "Taipei 101", Score: 0.9}},
		}}})
	}))
	defer srv.Close()
	visionAPIEndpoint = srv.URL
	visionTokenSource = func() (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}), nil
	}

	labels, hasPerson, err := DetectImageLabelsWithFaces(Config{}, []byte("img"))
	if err != nil || !hasPerson || len(labels) != 3 || labels[1].Description != "Google" || labels[2].Description != "Taipei 101" {
		t.Fatalf("labels=%+v hasPerson=%t err=%v", labels, hasPerson, err)
	}
}

func TestDetectImageLabels_APIError(t *testing.T) {
	prevEndpoint := visionAPIEndpoint
	prevTokenSource := visionTokenSource
	t.Cleanup(func() {
		visionAPIEndpoint = prevEndpoint
		visionTokenSource = prevTokenSource
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(visionAnnotateResponse{
			Responses: []visionAnnotateResult{
				{
					Error: &visionAPIError{Code: 7, Message: "permission denied"},
				},
			},
		})
	}))
	defer srv.Close()

	visionAPIEndpoint = srv.URL
	visionTokenSource = func() (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}), nil
	}

	_, err := DetectImageLabels(Config{ImageLabelMaxResults: 1}, []byte("img"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected API error, got %v", err)
	}
}
