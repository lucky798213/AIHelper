package intelligence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type EmbeddingConfig struct {
	Enabled  bool
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
}

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

type OpenAICompatibleEmbedder struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

func NewOpenAICompatibleEmbedder(cfg EmbeddingConfig) (*OpenAICompatibleEmbedder, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("embedding api_key is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("embedding model is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAICompatibleEmbedder{
		baseURL:    baseURL,
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (e *OpenAICompatibleEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	reqBody, err := json.Marshal(openAIEmbeddingRequest{
		Model: e.model,
		Input: text,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("embedding request failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	var decoded openAIEmbeddingResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, err
	}
	if len(decoded.Data) == 0 {
		return nil, fmt.Errorf("embedding response has no data")
	}
	return decoded.Data[0].Embedding, nil
}

type openAIEmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type openAIEmbeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}
