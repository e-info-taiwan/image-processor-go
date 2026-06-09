package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const visionLabelDetectionFeature = "LABEL_DETECTION"

type ImageLabel struct {
	MID         string  `json:"mid,omitempty"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
	Topicality  float64 `json:"topicality,omitempty"`
}

type ImageLabelSuggestion struct {
	Tag        string  `json:"tag"`
	Label      string  `json:"label"`
	Score      float64 `json:"score"`
	Topicality float64 `json:"topicality,omitempty"`
	Source     string  `json:"source"`
}

type visionAnnotateRequest struct {
	Requests []visionAnnotateItem `json:"requests"`
}

type visionAnnotateItem struct {
	Image    visionImage     `json:"image"`
	Features []visionFeature `json:"features"`
}

type visionImage struct {
	Content string `json:"content"`
}

type visionFeature struct {
	Type       string `json:"type"`
	MaxResults int    `json:"maxResults,omitempty"`
}

type visionAnnotateResponse struct {
	Responses []visionAnnotateResult `json:"responses"`
}

type visionAnnotateResult struct {
	LabelAnnotations []ImageLabel    `json:"labelAnnotations,omitempty"`
	Error            *visionAPIError `json:"error,omitempty"`
}

type visionAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var (
	visionAPIEndpoint = "https://vision.googleapis.com/v1/images:annotate"
	visionTokenSource = func() (oauth2.TokenSource, error) {
		return google.DefaultTokenSource(oauth2.NoContext, "https://www.googleapis.com/auth/cloud-platform")
	}
	visionHTTPClient = http.DefaultClient
)

func DetectImageLabels(cfg Config, imageBytes []byte) ([]ImageLabel, error) {
	if len(imageBytes) == 0 {
		return nil, fmt.Errorf("empty image bytes")
	}

	maxResults := cfg.ImageLabelMaxResults
	if maxResults <= 0 {
		maxResults = 10
	}

	reqPayload := visionAnnotateRequest{
		Requests: []visionAnnotateItem{
			{
				Image: visionImage{
					Content: base64.StdEncoding.EncodeToString(imageBytes),
				},
				Features: []visionFeature{
					{
						Type:       visionLabelDetectionFeature,
						MaxResults: maxResults,
					},
				},
			},
		},
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal vision request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, visionAPIEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create vision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	ts, err := visionTokenSource()
	if err != nil {
		return nil, fmt.Errorf("create vision token source: %w", err)
	}
	token, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("get vision token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)

	resp, err := visionHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do vision request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vision server error (%d): %s", resp.StatusCode, string(respBody))
	}

	var parsed visionAnnotateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode vision response: %w", err)
	}
	if len(parsed.Responses) == 0 {
		return nil, fmt.Errorf("vision response missing annotations")
	}
	first := parsed.Responses[0]
	if first.Error != nil {
		return nil, fmt.Errorf("vision annotate error (%d): %s", first.Error.Code, first.Error.Message)
	}

	return first.LabelAnnotations, nil
}

func FilterImageLabelSuggestions(labels []ImageLabel, minScore float64) []ImageLabelSuggestion {
	if minScore <= 0 {
		minScore = 0.75
	}

	byTag := map[string]ImageLabelSuggestion{}
	for _, label := range labels {
		description := strings.TrimSpace(label.Description)
		if description == "" || label.Score < minScore {
			continue
		}

		tag := normalizeImageLabelTag(description)
		if tag == "" {
			continue
		}

		suggestion := ImageLabelSuggestion{
			Tag:        tag,
			Label:      description,
			Score:      label.Score,
			Topicality: label.Topicality,
			Source:     "google-cloud-vision",
		}
		if existing, ok := byTag[tag]; !ok || suggestion.Score > existing.Score {
			byTag[tag] = suggestion
		}
	}

	suggestions := make([]ImageLabelSuggestion, 0, len(byTag))
	for _, suggestion := range byTag {
		suggestions = append(suggestions, suggestion)
	}
	sort.SliceStable(suggestions, func(i, j int) bool {
		if suggestions[i].Score == suggestions[j].Score {
			return suggestions[i].Tag < suggestions[j].Tag
		}
		return suggestions[i].Score > suggestions[j].Score
	})
	return suggestions
}

func normalizeImageLabelTag(label string) string {
	tag := strings.ToLower(strings.TrimSpace(label))
	tag = strings.Join(strings.Fields(tag), " ")
	return tag
}
