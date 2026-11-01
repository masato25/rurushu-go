package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OllamaProvider uses Ollama's native API. In particular, it disables model
// thinking so a small RIGHT completion budget is spent on the final answer
// instead of being consumed entirely by hidden reasoning tokens.
type OllamaProvider struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewOllamaProvider(baseURL string) *OllamaProvider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	return &OllamaProvider{BaseURL: baseURL, HTTPClient: &http.Client{Timeout: 0}}
}

func (p *OllamaProvider) ID() string   { return "ollama" }
func (p *OllamaProvider) Name() string { return "Ollama" }

func (p *OllamaProvider) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama list models error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(payload.Models))
	for _, model := range payload.Models {
		models = append(models, model.Name)
	}
	return models, nil
}

func (p *OllamaProvider) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamEvent, error) {
	type ollamaMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	messages := make([]ollamaMessage, 0, len(req.Messages))
	for _, msg := range SanitizeMessages(req.Messages) {
		messages = append(messages, ollamaMessage{Role: msg.Role, Content: msg.Content})
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   true,
		"think":    false,
	}
	options := map[string]any{}
	if req.MaxTokens != nil {
		options["num_predict"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}
	if len(options) > 0 {
		body["options"] = options
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama connection error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama chat error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	out := make(chan StreamEvent, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var chunk struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				Done            bool `json:"done"`
				PromptEvalCount int  `json:"prompt_eval_count"`
				EvalCount       int  `json:"eval_count"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
				out <- StreamEvent{Type: EventError, Error: fmt.Errorf("ollama stream decode error: %w", err)}
				return
			}
			if chunk.Message.Content != "" {
				out <- StreamEvent{Type: EventToken, Text: chunk.Message.Content}
			}
			if chunk.Done {
				if chunk.PromptEvalCount > 0 || chunk.EvalCount > 0 {
					out <- StreamEvent{Type: EventUsage, Usage: &TokenUsage{PromptTokens: chunk.PromptEvalCount, CompletionTokens: chunk.EvalCount, TotalTokens: chunk.PromptEvalCount + chunk.EvalCount}}
				}
				out <- StreamEvent{Type: EventDone}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			out <- StreamEvent{Type: EventError, Error: fmt.Errorf("ollama stream read error: %w", err)}
			return
		}
		out <- StreamEvent{Type: EventError, Error: fmt.Errorf("ollama stream closed without completion")}
	}()
	return out, nil
}
