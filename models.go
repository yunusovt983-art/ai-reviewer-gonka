package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"ai-reviewer/internal/domain/review"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sashabaranov/go-openai"
	"google.golang.org/genai"
)

var (
	envCache map[string]string
	envOnce  sync.Once
)

func getEnv(key string) string {
	envOnce.Do(func() {
		envCache = make(map[string]string)
		data, err := os.ReadFile("KEYS.env")
		if err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					envCache[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
				}
			}
		}
	})

	if val, ok := envCache[key]; ok {
		return val
	}
	return os.Getenv(key)
}

// ModelClient and ModelResult are owned by internal/domain/review; aliased here
// so existing code in package main compiles without qualification.
type ModelClient = review.ModelClient
type ModelResult = review.ModelResult

type ModelCategory string

const (
	FastestGood  ModelCategory = "fastest_good"
	Balanced     ModelCategory = "balanced"
	BestCode     ModelCategory = "best_code"
	FrontierBest ModelCategory = "frontier_best"
)

// OpenAI Client
type OpenAIClient struct {
	client         *openai.Client
	model          string
	reasoningLevel string
}

func NewOpenAIClient(apiKey, model, reasoningLevel string) *OpenAIClient {
	return &OpenAIClient{
		client:         openai.NewClient(apiKey),
		model:          model,
		reasoningLevel: reasoningLevel,
	}
}

func (c *OpenAIClient) Generate(ctx context.Context, prompt string, maxTokens int) (ModelResult, error) {
	return c.generate(ctx, prompt, maxTokens, false)
}

func (c *OpenAIClient) GenerateJSON(ctx context.Context, prompt string, maxTokens int) (ModelResult, error) {
	return c.generate(ctx, prompt, maxTokens, true)
}

func (c *OpenAIClient) generate(ctx context.Context, prompt string, maxTokens int, jsonMode bool) (ModelResult, error) {
	modelDisplay := c.model
	if c.reasoningLevel != "" && c.reasoningLevel != "none" {
		modelDisplay = fmt.Sprintf("%s(%s)", c.model, c.reasoningLevel)
	}
	req := openai.ChatCompletionRequest{
		Model: c.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
	}
	if jsonMode {
		req.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}
	if maxTokens > 0 {
		req.MaxCompletionTokens = maxTokens
	}

	if c.reasoningLevel != "" && c.reasoningLevel != "none" {
		req.ReasoningEffort = c.reasoningLevel
	}

	resp, err := c.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return ModelResult{}, err
	}

	tokensIn := resp.Usage.PromptTokens
	tokensOut := resp.Usage.CompletionTokens
	tokensReasoning := 0
	if resp.Usage.CompletionTokensDetails != nil {
		tokensReasoning = resp.Usage.CompletionTokensDetails.ReasoningTokens
	}

	return ModelResult{
		Text:            resp.Choices[0].Message.Content,
		TokensIn:        tokensIn,
		TokensOut:       tokensOut,
		TokensReasoning: tokensReasoning,
		Provider:        "openai",
		Model:           modelDisplay,
		FinishReason:    string(resp.Choices[0].FinishReason),
	}, nil
}

// Anthropic Client
type AnthropicClient struct {
	client         *anthropic.Client
	model          string
	reasoningLevel string
}

func NewAnthropicClient(apiKey, model, reasoningLevel string) *AnthropicClient {
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &AnthropicClient{
		client:         &c,
		model:          model,
		reasoningLevel: reasoningLevel,
	}
}

func (c *AnthropicClient) Generate(ctx context.Context, prompt string, maxTokens int) (ModelResult, error) {
	return c.generate(ctx, prompt, maxTokens, false)
}

func (c *AnthropicClient) GenerateJSON(ctx context.Context, prompt string, maxTokens int) (ModelResult, error) {
	// Anthropic doesn't have a simple "JSON mode" flag in the same way OpenAI does,
	// but we can ask for it in the prompt or use tool use.
	// For now, we'll just append a JSON instruction if it's not already there and use regular Generate.
	// Actually, newer Anthropic models support structured output via tools, but for simplicity
	// here we will just rely on the system prompt for now, OR we could implement tool use.
	// The prompt already asks for JSON.
	return c.generate(ctx, prompt, maxTokens, true)
}

func (c *AnthropicClient) generate(ctx context.Context, prompt string, maxTokens int, jsonMode bool) (ModelResult, error) {
	modelDisplay := c.model
	if c.reasoningLevel != "" && c.reasoningLevel != "none" {
		modelDisplay = fmt.Sprintf("%s(%s)", c.model, c.reasoningLevel)
	}
	params := anthropic.MessageNewParams{
		Model: anthropic.Model(c.model),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	}
	if maxTokens > 0 {
		params.MaxTokens = int64(maxTokens)
	} else {
		// If 0, it means no limit, but Anthropic REQUIRES max_tokens.
		// Use 64k tokens
		params.MaxTokens = 65536
	}

	if c.reasoningLevel != "" && c.reasoningLevel != "none" {
		budget := int64(2048)
		if params.MaxTokens > 4096 {
			budget = int64(params.MaxTokens / 2)
		} else if params.MaxTokens <= 2048 {
			// Budget must be < max_tokens and >= 1024
			// If maxTokens is too low, we might need to increase it or skip thinking
			if params.MaxTokens > 1024 {
				budget = 1024
			} else {
				// Can't enable thinking if max_tokens <= 1024
				budget = 0
			}
		}

		if budget >= 1024 {
			params.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
			// Anthropic requires max_tokens to be GREATER than budget_tokens.
			// It should be enough to cover thinking + some output.
			if params.MaxTokens <= budget {
				params.MaxTokens = budget + 1024
			}
		}
	}

	stream := c.client.Messages.NewStreaming(ctx, params)
	tokensReasoning := 0
	text := ""
	var inputTokens int64
	var outputTokens int64
	var stopReason string

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "message_start":
			inputTokens = event.Message.Usage.InputTokens
			outputTokens = event.Message.Usage.OutputTokens
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				text += event.Delta.Text
			}
		case "message_delta":
			outputTokens = event.Usage.OutputTokens
			stopReason = string(event.Delta.StopReason)
		}
	}

	if err := stream.Err(); err != nil {
		return ModelResult{}, err
	}

	return ModelResult{
		Text:            text,
		TokensIn:        int(inputTokens),
		TokensOut:       int(outputTokens),
		TokensReasoning: tokensReasoning,
		Provider:        "anthropic",
		Model:           modelDisplay,
		FinishReason:    stopReason,
	}, nil
}

// Gemini Client
type GeminiClient struct {
	client         *genai.Client
	model          string
	reasoningLevel string
}

func NewGeminiClient(ctx context.Context, apiKey, model, reasoningLevel string) (*GeminiClient, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &GeminiClient{
		client:         client,
		model:          model,
		reasoningLevel: reasoningLevel,
	}, nil
}

func (c *GeminiClient) Generate(ctx context.Context, prompt string, maxTokens int) (ModelResult, error) {
	return c.generate(ctx, prompt, maxTokens, false)
}

func (c *GeminiClient) GenerateJSON(ctx context.Context, prompt string, maxTokens int) (ModelResult, error) {
	return c.generate(ctx, prompt, maxTokens, true)
}

func (c *GeminiClient) generate(ctx context.Context, prompt string, maxTokens int, jsonMode bool) (ModelResult, error) {
	modelDisplay := c.model
	if c.reasoningLevel != "" && c.reasoningLevel != "none" {
		modelDisplay = fmt.Sprintf("%s(%s)", c.model, c.reasoningLevel)
	}
	config := &genai.GenerateContentConfig{}
	if jsonMode {
		config.ResponseMIMEType = "application/json"
	}
	if maxTokens > 0 {
		config.MaxOutputTokens = int32(maxTokens)
	}

	if c.reasoningLevel != "" && c.reasoningLevel != "none" {
		config.ThinkingConfig = &genai.ThinkingConfig{}
		switch c.reasoningLevel {
		case "low":
			config.ThinkingConfig.ThinkingLevel = genai.ThinkingLevelLow
		case "medium":
			config.ThinkingConfig.ThinkingLevel = genai.ThinkingLevelMedium
		case "high":
			config.ThinkingConfig.ThinkingLevel = genai.ThinkingLevelHigh
		}
		config.ThinkingConfig.IncludeThoughts = false
	}

	resp, err := c.client.Models.GenerateContent(ctx, c.model, genai.Text(prompt), config)
	if err != nil {
		return ModelResult{}, err
	}

	if len(resp.Candidates) == 0 {
		return ModelResult{}, fmt.Errorf("empty response from Gemini")
	}

	tokensIn := 0
	tokensOut := 0
	tokensReasoning := 0
	if resp.UsageMetadata != nil {
		tokensIn = int(resp.UsageMetadata.PromptTokenCount)
		tokensOut = int(resp.UsageMetadata.CandidatesTokenCount)
		tokensReasoning = int(resp.UsageMetadata.ThoughtsTokenCount)
	}

	finishReason := string(resp.Candidates[0].FinishReason)

	if len(resp.Candidates[0].Content.Parts) == 0 {
		return ModelResult{
			TokensIn:        tokensIn,
			TokensOut:       tokensOut,
			TokensReasoning: tokensReasoning,
			Provider:        "gemini",
			Model:           c.model,
			FinishReason:    finishReason,
		}, nil
	}

	text := ""
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Text != "" {
			text += part.Text
		}
	}

	return ModelResult{
		Text:            text,
		TokensIn:        tokensIn,
		TokensOut:       tokensOut,
		TokensReasoning: tokensReasoning,
		Provider:        "gemini",
		Model:           modelDisplay,
		FinishReason:    finishReason,
	}, nil
}

func GetModelClient(ctx context.Context, provider, model, reasoningLevel string) (ModelClient, error) {
	switch provider {
	case "openai":
		apiKey := getEnv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY not set")
		}
		return NewOpenAIClient(apiKey, model, reasoningLevel), nil
	case "anthropic":
		apiKey := getEnv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
		}
		return NewAnthropicClient(apiKey, model, reasoningLevel), nil
	case "gemini":
		apiKey := getEnv("GEMINI_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY not set")
		}
		return NewGeminiClient(ctx, apiKey, model, reasoningLevel)
	default:
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}
}
