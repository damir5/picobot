package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// OpenAIProvider calls an OpenAI-compatible API (OpenAI, OpenRouter, or similar).
type OpenAIProvider struct {
	APIKey     string
	APIBase    string // e.g. https://api.openai.com/v1 or https://openrouter.ai/api/v1
	Client     *http.Client
	MaxRetries int
}

func NewOpenAIProvider(apiKey, apiBase string, timeout time.Duration, maxRetries int) *OpenAIProvider {
	if apiBase == "" {
		apiBase = "https://api.openai.com/v1" // sensible default; can be overridden
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if maxRetries <= 0 {
		maxRetries = 5
	}
	return &OpenAIProvider{
		APIKey:     apiKey,
		APIBase:    strings.TrimRight(apiBase, "/"),
		MaxRetries: maxRetries,
		Client: &http.Client{
			Timeout: timeout,
		},
	}
}

func (p *OpenAIProvider) GetDefaultModel() string { return "gpt-4o-mini" }

// Request/response shapes using the modern OpenAI "tools" format.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []messageJSON `json:"messages"`
	Tools    []toolWrapper `json:"tools,omitempty"`
}

// toolWrapper is the OpenAI tools array element: {"type": "function", "function": {...}}
type toolWrapper struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type messageJSON struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCallJSON `json:"tool_calls,omitempty"`
}

type toolCallJSON struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function toolCallFunctionJSON `json:"function"`
}

type toolCallFunctionJSON struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type messageResponseJSON struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []toolCallJSON `json:"tool_calls,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message messageResponseJSON `json:"message"`
	} `json:"choices"`
}

// Chat calls an OpenAI-compatible chat completion endpoint and returns a simplified response.
func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message, tools []ToolDefinition, model string) (LLMResponse, error) {
	if p.APIKey == "" {
		return LLMResponse{}, errors.New("OpenAI provider: API key is not configured")
	}
	if model == "" {
		model = p.GetDefaultModel()
	}

	reqBody := chatRequest{Model: model, Messages: make([]messageJSON, 0, len(messages))}
	for _, m := range messages {
		mj := messageJSON{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		// Convert provider ToolCall to JSON-serializable toolCallJSON
		for _, tc := range m.ToolCalls {
			argsBytes, _ := json.Marshal(tc.Arguments)
			mj.ToolCalls = append(mj.ToolCalls, toolCallJSON{
				ID:   tc.ID,
				Type: "function",
				Function: toolCallFunctionJSON{
					Name:      tc.Name,
					Arguments: string(argsBytes),
				},
			})
		}
		reqBody.Messages = append(reqBody.Messages, mj)
	}

	// Include tools in modern format if provided
	if len(tools) > 0 {
		reqBody.Tools = make([]toolWrapper, 0, len(tools))
		for _, t := range tools {
			params := t.Parameters
			if params == nil {
				params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
			}
			reqBody.Tools = append(reqBody.Tools, toolWrapper{
				Type: "function",
				Function: functionDef{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return LLMResponse{}, err
	}

	url := fmt.Sprintf("%s/chat/completions", p.APIBase)
	log.Printf("openai: POST %s model=%s messages=%d tools=%d bodyBytes=%d httpTimeout=%s maxRetries=%d", url, model, len(messages), len(tools), len(b), p.Client.Timeout, p.MaxRetries)

	var resp *http.Response
	var lastErr error
	for attempt := 1; attempt <= p.MaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(b)))
		if err != nil {
			return LLMResponse{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.APIKey)

		start := time.Now()
		resp, err = p.Client.Do(req)
		elapsed := time.Since(start)

		if err != nil {
			lastErr = err
			log.Printf("openai: attempt %d/%d failed after %s: %v", attempt, p.MaxRetries, elapsed, err)
			if ctx.Err() != nil {
				return LLMResponse{}, fmt.Errorf("openai: context cancelled after %d attempts: %w", attempt, err)
			}
			backoff := time.Duration(attempt) * 2 * time.Second
			log.Printf("openai: retrying in %s...", backoff)
			time.Sleep(backoff)
			continue
		}

		log.Printf("openai: attempt %d/%d response %s in %s", attempt, p.MaxRetries, resp.Status, elapsed)

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("OpenAI API error: %s - %s", resp.Status, strings.TrimSpace(string(bodyBytes)))
			log.Printf("openai: attempt %d/%d got %s, retrying...", attempt, p.MaxRetries, resp.Status)
			backoff := time.Duration(attempt) * 2 * time.Second
			time.Sleep(backoff)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := strings.TrimSpace(string(bodyBytes))
			log.Printf("OpenAI API non-2xx: %s body=%q", resp.Status, body)
			if body == "" {
				return LLMResponse{}, fmt.Errorf("OpenAI API error: %s", resp.Status)
			}
			return LLMResponse{}, fmt.Errorf("OpenAI API error: %s - %s", resp.Status, body)
		}

		// success
		lastErr = nil
		break
	}
	if lastErr != nil {
		return LLMResponse{}, fmt.Errorf("openai: all %d attempts failed: %w", p.MaxRetries, lastErr)
	}
	defer resp.Body.Close()

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return LLMResponse{}, err
	}

	if len(out.Choices) == 0 {
		return LLMResponse{}, errors.New("OpenAI API returned no choices")
	}

	msg := out.Choices[0].Message
	// If the model requested tool calls, parse them
	if len(msg.ToolCalls) > 0 {
		var tcs []ToolCall
		for _, tc := range msg.ToolCalls {
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
				// skip unparseable tool calls
				continue
			}
			tcs = append(tcs, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: parsed})
		}
		if len(tcs) > 0 {
			return LLMResponse{Content: strings.TrimSpace(msg.Content), HasToolCalls: true, ToolCalls: tcs}, nil
		}
	}

	// No tool calls
	return LLMResponse{Content: strings.TrimSpace(msg.Content), HasToolCalls: false}, nil
}
