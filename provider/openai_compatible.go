package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPError preserves enough response metadata for higher-level runtimes to
// distinguish a quota/rate-limit pause from a permanent provider failure.
type HTTPError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "openai-compatible http error"
	}
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("openai-compatible error (status %d)", e.StatusCode)
	}
	return fmt.Sprintf("openai-compatible error (status %d): %s", e.StatusCode, body)
}

// IsRateLimitError reports whether err represents a provider usage/rate limit.
// 429 is the canonical signal; a small set of common quota phrases is also
// accepted because OpenAI-compatible gateways sometimes translate limits into
// another 4xx status.
func IsRateLimitError(err error) (*HTTPError, bool) {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr == nil {
		return nil, false
	}
	if httpErr.StatusCode == http.StatusTooManyRequests {
		return httpErr, true
	}
	body := strings.ToLower(httpErr.Body)
	for _, marker := range []string{"rate limit", "quota exceeded", "usage limit", "limit reached", "too many requests"} {
		if strings.Contains(body, marker) {
			return httpErr, true
		}
	}
	return nil, false
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := when.Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

// OpenAICompatibleProvider implements the Provider interface for OpenAI-compatible cloud/gateway APIs.
type OpenAICompatibleProvider struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	mu         sync.RWMutex
}

// NewOpenAICompatibleProvider initializes a OpenAICompatibleProvider with the specified base URL and API key.
// Default URL is https://api.openai.com/v1 if not specified.
func NewOpenAICompatibleProvider(baseURL, apiKey string) *OpenAICompatibleProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return &OpenAICompatibleProvider{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 0, // No timeout for SSE streaming; context controls lifecycle
		},
	}
}

func (p *OpenAICompatibleProvider) ID() string {
	return "openai-compatible"
}

func (p *OpenAICompatibleProvider) Name() string {
	return "OpenAI-compatible"
}

// HasAPIKey reports whether a credential is currently configured without
// exposing the credential itself.
func (p *OpenAICompatibleProvider) HasAPIKey() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return strings.TrimSpace(p.APIKey) != ""
}

// SetAPIKey replaces the bearer credential used by subsequent requests.
func (p *OpenAICompatibleProvider) SetAPIKey(apiKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.APIKey = strings.TrimSpace(apiKey)
}

func (p *OpenAICompatibleProvider) apiKey() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.APIKey
}

// SetBaseURL updates the endpoint URL for OpenAI-compatible.
func (p *OpenAICompatibleProvider) SetBaseURL(baseURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	p.BaseURL = strings.TrimRight(baseURL, "/")
}

func (p *OpenAICompatibleProvider) baseURL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.BaseURL == "" {
		return "https://api.openai.com/v1"
	}
	return p.BaseURL
}

func (p *OpenAICompatibleProvider) ListModels(ctx context.Context) ([]string, error) {
	endpoint := p.baseURL() + "/models"

	httpReq, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := p.apiKey(); apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai-compatible list models error: status %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var models []string
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// Stream sends a chat completion request to the OpenAI-compatible endpoint and yields streaming events.
func (p *OpenAICompatibleProvider) Stream(ctx context.Context, req CompletionRequest) (<-chan StreamEvent, error) {
	endpoint := p.baseURL() + "/chat/completions"

	sanitizedMessages := SanitizeMessages(req.Messages)

	// Convert to API messages (handle multimodal content)
	var apiMessages []map[string]interface{}
	for _, m := range sanitizedMessages {
		apiMsg := map[string]interface{}{
			"role": m.Role,
		}
		if m.Name != "" {
			apiMsg["name"] = m.Name
		}
		if m.ToolCallID != "" {
			apiMsg["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			apiMsg["tool_calls"] = m.ToolCalls
		}

		if len(m.Images) > 0 {
			var contentParts []map[string]interface{}
			if m.Content != "" {
				contentParts = append(contentParts, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			for _, img := range m.Images {
				url := img
				if !strings.HasPrefix(url, "http") && !strings.HasPrefix(url, "data:") {
					url = "data:image/jpeg;base64," + img // Default to jpeg base64 if raw
				}
				contentParts = append(contentParts, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]string{"url": url},
				})
			}
			apiMsg["content"] = contentParts
		} else {
			apiMsg["content"] = m.Content
		}
		apiMessages = append(apiMessages, apiMsg)
	}

	reqBody := map[string]interface{}{
		"model":    req.Model,
		"messages": apiMessages,
		"stream":   true,
	}

	if req.Temperature != nil {
		reqBody["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		reqBody["max_tokens"] = *req.MaxTokens
	}
	if len(req.Tools) > 0 {
		reqBody["tools"] = req.Tools
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal openai-compatible request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create openai-compatible request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if apiKey := p.apiKey(); apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible connection error: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}

	outChan := make(chan StreamEvent, 100)

	go func() {
		defer resp.Body.Close()
		defer close(outChan)

		scanner := bufio.NewScanner(resp.Body)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		accumulator := NewToolCallAccumulator()

		var eventType string
		var dataBuffer strings.Builder
		emittedText := false

		inThinkTag := false
		var thinkBuffer strings.Builder

		flushEvent := func() {
			data := dataBuffer.String()
			dataBuffer.Reset()
			evt := eventType
			eventType = ""

			if data == "" || data == "[DONE]" {
				return
			}

			var chunk struct {
				Type        string `json:"type"`
				Event       string `json:"event"`
				Delta       string `json:"delta"`
				CallID      string `json:"call_id"`
				OutputIndex int    `json:"output_index"`
				Item        *struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
					Content   []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"item"`
				Response *struct {
					OutputText string `json:"output_text"`
					Output     []struct {
						Type      string `json:"type"`
						ID        string `json:"id"`
						CallID    string `json:"call_id"`
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
						Content   []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"output"`
					Usage *TokenUsage `json:"usage"`
				} `json:"response"`
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
						Reasoning        string `json:"reasoning"`
						ToolCalls        []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *TokenUsage `json:"usage"`
			}

			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return
			}

			if chunk.Usage != nil {
				outChan <- StreamEvent{Type: EventUsage, Usage: chunk.Usage}
			}

			t := chunk.Type
			if t == "" {
				t = chunk.Event
			}
			if t == "" {
				t = evt
			}

			switch t {
			case "response.output_text.delta", "response.refusal.delta":
				if chunk.Delta != "" {
					emittedText = true
					outChan <- StreamEvent{Type: EventToken, Text: chunk.Delta}
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if chunk.Delta != "" {
					outChan <- StreamEvent{Type: EventReasoning, ReasoningText: chunk.Delta}
				}
			case "response.output_item.added":
				if chunk.Item != nil && chunk.Item.Type == "function_call" {

					id := chunk.Item.CallID
					if id == "" {
						id = chunk.Item.ID
					}
					accumulator.AddChunk(chunk.OutputIndex, id, chunk.Item.Name, "")
				}
			case "response.function_call_arguments.delta":

				accumulator.AddChunk(chunk.OutputIndex, chunk.CallID, "", chunk.Delta)
			case "response.output_item.done":
				if chunk.Item != nil && chunk.Item.Type == "function_call" {
					id := chunk.Item.CallID
					if id == "" {
						id = chunk.Item.ID
					}
					accumulator.SetFinal(chunk.OutputIndex, id, chunk.Item.Name, chunk.Item.Arguments)
				} else if chunk.Item != nil && chunk.Item.Type == "message" && !emittedText {
					var text strings.Builder
					for _, c := range chunk.Item.Content {
						text.WriteString(c.Text)
					}
					if text.String() != "" {
						emittedText = true
						outChan <- StreamEvent{Type: EventToken, Text: text.String()}
					}
				}
			case "response.completed", "response.incomplete":
				if chunk.Response != nil {
					if chunk.Response.Usage != nil {
						outChan <- StreamEvent{Type: EventUsage, Usage: chunk.Response.Usage}
					}
					text := chunk.Response.OutputText
					if text == "" && len(chunk.Response.Output) > 0 {
						var tb strings.Builder
						for _, item := range chunk.Response.Output {
							if item.Type == "function_call" {

							} else if item.Type == "message" {
								for _, c := range item.Content {
									tb.WriteString(c.Text)
								}
							}
						}
						text = tb.String()
					}
					if text != "" && !emittedText {
						emittedText = true
						outChan <- StreamEvent{Type: EventToken, Text: text}
					}
				}
			default:
				if len(chunk.Choices) > 0 {
					choice := chunk.Choices[0]

					reasoningText := choice.Delta.ReasoningContent
					if reasoningText == "" {
						reasoningText = choice.Delta.Reasoning
					}
					if reasoningText != "" {
						outChan <- StreamEvent{Type: EventReasoning, ReasoningText: reasoningText}
					}

					for _, tc := range choice.Delta.ToolCalls {
						accumulator.AddChunk(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)

					}

					content := choice.Delta.Content
					if content != "" {
						if strings.Contains(content, "<think>") {
							inThinkTag = true
							parts := strings.Split(content, "<think>")
							if parts[0] != "" {
								outChan <- StreamEvent{Type: EventToken, Text: parts[0]}
							}
							content = parts[1]
						}

						if inThinkTag {
							if strings.Contains(content, "</think>") {
								parts := strings.Split(content, "</think>")
								thinkBuffer.WriteString(parts[0])
								outChan <- StreamEvent{
									Type:          EventReasoning,
									ReasoningText: thinkBuffer.String(),
								}
								thinkBuffer.Reset()
								inThinkTag = false
								if len(parts) > 1 && parts[1] != "" {
									outChan <- StreamEvent{Type: EventToken, Text: parts[1]}
								}
							} else {
								thinkBuffer.WriteString(content)
								outChan <- StreamEvent{
									Type:          EventReasoning,
									ReasoningText: content,
								}
							}
						} else {
							outChan <- StreamEvent{Type: EventToken, Text: content}
						}
					}
				}
			}
		}

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				outChan <- StreamEvent{Type: EventError, Error: ctx.Err()}
				return
			default:
			}

			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				flushEvent()
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				dataBuffer.WriteString(strings.TrimPrefix(line, "data: "))
			}
		}
		flushEvent()

		if err := scanner.Err(); err != nil && err != io.EOF {
			outChan <- StreamEvent{Type: EventError, Error: fmt.Errorf("openai-compatible stream read error: %w", err)}
			return
		}

		toolCalls := accumulator.Collect()
		if len(toolCalls) > 0 {
			outChan <- StreamEvent{Type: EventToolCall, ToolCalls: toolCalls}
		}

		outChan <- StreamEvent{Type: EventDone}
	}()

	return outChan, nil
}
