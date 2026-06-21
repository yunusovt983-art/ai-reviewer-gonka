// Package review is the heart of the review domain: the Finding value object,
// the ModelClient port (implemented by infra adapters), and the pure pipeline
// domain services (normalize raw reviewer text into Findings, aggregate Findings
// into a report). It depends only on stdlib — providers, git and config live in
// infra and depend on this package via the ModelClient port.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ModelClient is the port the domain uses to talk to any LLM provider.
// infra/modelclient adapters (OpenAI/Anthropic/Gemini) implement it.
type ModelClient interface {
	Generate(ctx context.Context, prompt string, maxTokens int) (ModelResult, error)
	GenerateJSON(ctx context.Context, prompt string, maxTokens int) (ModelResult, error)
}

type ModelResult struct {
	Text            string
	TokensIn        int
	TokensOut       int
	TokensReasoning int
	Provider        string
	Model           string
	FinishReason    string
}

type Finding struct {
	Source       string  `json:"source"`
	File         string  `json:"file"`
	LineStart    *int    `json:"line_start,omitempty"`
	LineEnd      *int    `json:"line_end,omitempty"`
	Summary      string  `json:"summary"`
	Details      string  `json:"details,omitempty"`
	SeverityHint string  `json:"severity_hint"` // low | medium | high | critical | unknown
	Confidence   float64 `json:"confidence"`    // 0.0–1.0
}

type NormalizationResponse struct {
	Findings []Finding `json:"findings"`
}

type PreRunAnalysis struct {
	File     string `json:"file"`
	Analysis string `json:"analysis"`
}

type PreRunExplainerResponse struct {
	Files []PreRunAnalysis `json:"files"`
}

const PreRunExplainerSystemPrompt = `
You are a pre-run explainer for automated code review.
Your task is to analyze each file in the diff and provide critical information or research as requested.

You MUST:
- Provide analysis for EACH file mentioned in the diff.
- Keep the analysis concise and focused on the requested research.
- Output ONLY valid JSON in the specified format.

OUTPUT FORMAT (JSON ONLY):
{
  "files": [
    {
      "file": "<path/to/file>",
      "analysis": "<your analysis for this file>"
    }
  ]
}
`

const NormalizationSystemPrompt = `
You are a normalization extractor for automated code review.

Your task is to convert a reviewer’s raw output into a list of concrete findings.

You MUST:
	•	Remove all reasoning, commentary, and self-reflection
	•	Extract only actionable issues explicitly mentioned
	•	Preserve file names and line numbers exactly as stated
	•	Preserve attribution to the source persona
	•	Lower confidence if locations are approximate

You MUST NOT:
	•	Invent new issues
	•	Re-analyze the code
	•	Guess or fabricate line numbers
	•	Add severity not implied by the reviewer

If the reviewer reports no issues, return an empty list.

OUTPUT FORMAT (JSON ONLY)

{
  "findings": [
    {
      "source": "<persona_id>",
      "file": "<path>",
      "line_start": <int or null>,
      "line_end": <int or null>,
      "summary": "<short, concrete issue>",
      "details": "<full description of the issue, including why it is a problem and how to fix it>",
      "severity_hint": "critical | high | medium | low",
      "confidence": <float between 0.0 and 1.0>
    }
  ]
}

Severity Hint Guidelines:
1. critical - issues that will likely cause catastrophic failures (data loss/corruption, exposure to attack, consensus failures, application crashes, etc). These should NEVER be ignored.
2. high - issues that will greatly impact perf, unlikely but possible critical issues, break important patterns, etc.
3. medium - issues that could lightly impact perf, mislead future programmers, break established (but non vital) patterns and practices, etc.
4. low - nitpicks/ cleaning up/ etc.

For severity, if the reviewer has indicated a severity assume they are correct, but only relative to their focus. You may downgrade the severity if, from a more global perspective, the issue is not as severe.

Return only valid JSON. No extra text.
`

const AggregatorSystemPrompt = `
You are an aggregator for automated code review findings.
Your goal is to turn many individual findings into a concise, human-usable review.

The aggregator:
	•	Deduplicates similar findings
	•	Clusters related issues
	•	Assigns final severity
	•	Produces short, actionable lists
	•	Uses the provided details to explain the problem and solution clearly.
	•	Produces one-line summaries per persona
	•	Includes a file/line number reference for every finding
	•	You MUST include specific line numbers (e.g., ./filename.go:10-15) for all findings to ensure they are actionable.


The aggregator must not:
	•	Analyze code
	•	Invent new findings
	•	Add fake precision

You must produce Markdown with five sections:
	1.	Must Fix (critical)
	2.	Major Issues (high)
	3.	Review Carefully (medium)
	4.	Consider (low)
	5.	Persona Summaries

Each finding in sections 1-4 should include the summary, the source personas, the file/line reference, and a clear explanation of the issue and suggested fix based on the provided details.

Plus a short executive summary paragraph at the top.

Example structure:

## Summary
<3–5 sentences, high level>

## 🛑 Must Fix
- **<summary>** (sources: @persona{persona1}, @persona{persona2}, ./filename.go:10-15)
  <Full description and fix instructions from details>

## ❗ Major Issues
- **<summary>** (sources: @persona{persona1}, ./filename.go:20)
  <Full description and fix instructions from details>

## ⚠️ Review Carefully
- **<summary>** (sources: @persona{persona1}, ./filename.go:30-35)
  <Full description and fix instructions from details>

## 💭 Consider
- **<summary>** (sources: @persona{persona3}, ./filename.go:40)
  <Full description and fix instructions from details>

## Persona Summaries
- @persona{Persona1}: ❌ Critical/Major issues
- @persona{Persona2}: ⚠️ Minor issues
- @persona{Persona3}: ✅ Looks reasonable

Instructions for aggregation:
	•	Cut all chatter
	•	Prefer bullet points
	•	Preserve source attribution
	•	Upgrade severity when multiple personas agree
	•	Downgrade severity for low-confidence findings
	•	Explicitly call out disagreements
	•	Use @persona{ID} whenever you refer to a persona's ID.
	•	ALWAYS include file and line number information in the findings list.
`

func ExtractJSON(text string) string {
	if idx := strings.Index(text, "```json"); idx != -1 {
		text = text[idx+7:]
		if endIdx := strings.Index(text, "```"); endIdx != -1 {
			text = text[:endIdx]
		}
	} else if idx := strings.Index(text, "```"); idx != -1 {
		text = text[idx+3:]
		if endIdx := strings.Index(text, "```"); endIdx != -1 {
			text = text[:endIdx]
		}
	}
	return strings.TrimSpace(text)
}

func NormalizePersonaOutput(ctx context.Context, client ModelClient, personaID, rawOutput string) ([]Finding, ModelResult, error) {
	prompt := fmt.Sprintf("%s\n\n--- PERSONA OUTPUT (%s) ---\n%s", NormalizationSystemPrompt, personaID, rawOutput)

	normCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	result, err := client.GenerateJSON(normCtx, prompt, 0)
	if err != nil {
		return nil, ModelResult{}, err
	}

	text := ExtractJSON(result.Text)

	var response NormalizationResponse
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		return nil, result, fmt.Errorf("error unmarshaling normalization response: %w", err)
	}

	return response.Findings, result, nil
}

func ParsePreRunExplainerOutput(rawOutput string) ([]PreRunAnalysis, error) {
	text := ExtractJSON(rawOutput)
	var response PreRunExplainerResponse
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		return nil, fmt.Errorf("error unmarshaling pre-run explainer response: %w", err)
	}
	return response.Files, nil
}

func AggregateFindings(ctx context.Context, client ModelClient, findings []Finding) (string, ModelResult, error) {
	if len(findings) == 0 {
		return "## Summary\nNo issues found by any persona.", ModelResult{}, nil
	}

	findingsJSON, err := json.Marshal(NormalizationResponse{Findings: findings})
	if err != nil {
		return "", ModelResult{}, err
	}

	prompt := fmt.Sprintf("%s\n\n--- FINDINGS ---\n%s", AggregatorSystemPrompt, string(findingsJSON))

	// Aggregator uses balanced, which might take a bit longer than normalization but 5m is plenty as per main.go's default
	result, err := client.Generate(ctx, prompt, 0)
	if err != nil {
		return "", ModelResult{}, err
	}

	return result.Text, result, nil
}
