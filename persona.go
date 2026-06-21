package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Persona struct {
	ID                string `yaml:"id"`
	AIReview          string `yaml:"ai_review"`
	ColoredID         string
	ModelCategory     string    `yaml:"model_category"`
	MaxTokens         *int      `yaml:"max_tokens"`
	Filters           FilterSet `yaml:",inline"`
	Role              string    `yaml:"role"`  // reviewer (default) | explainer
	Stage             string    `yaml:"stage"` // pre | post
	IncludeFindings   bool      `yaml:"include_findings"`
	IncludeExplainers []string  `yaml:"include_explainers"`
	ExcludeDiff       bool      `yaml:"exclude_diff"`
	Instructions      string
}

type PersonaRun struct {
	Persona Persona
	Context *PRContext
}

func LoadPersonas(searchPaths []string, repo string, headSHA string, oh *OutputHandler) ([]Persona, error) {
	scanner := NewScanner(searchPaths, repo, headSHA, oh)
	results, err := scanner.Load("persona", func() any { return &Persona{} })
	if err != nil && len(results) == 0 {
		return nil, err
	}
	if err != nil {
		oh.Printf("Warning: issues encountered while loading personas: %v\n", err)
	}

	if len(results) == 0 {
		return nil, nil // No personas found is okay
	}

	var personas []Persona
	for _, res := range results {
		p := res.Raw.(*Persona)
		p.Instructions = res.Instructions
		if p.Role == "" {
			p.Role = "reviewer"
		}
		p.ColoredID = "\033[32m" + p.ID + "\033[0m"
		personas = append(personas, *p)
	}

	return personas, nil
}

func (p Persona) Run(ctx context.Context, rc *RunConfig, rr *RunResults, personaContext *PRContext) (string, ModelResult, time.Duration, []PrimerMatch, error) {
	var preRunAnalyses map[string][]string
	var summary string

	if p.Role == "reviewer" || (p.Role == "explainer" && p.Stage == "post") {
		preRunAnalyses = rr.PreRunAnalyses
	}
	if p.Role == "explainer" && p.Stage == "post" {
		summary = rr.Summary
	}

	matchedPrimers := rc.FindMatches(personaContext)
	prompt := buildPrompt(p, personaContext, rc.Config.GlobalInstructions, preRunAnalyses, summary, matchedPrimers, rc.Config.PrimerTypes)

	var cachePath string
	if p.Role == "explainer" && p.Stage == "pre" && rc.PRInfo.HeadRefOid != "" {
		h := sha256.New()
		h.Write([]byte(p.Instructions))
		h.Write([]byte(rc.PRInfo.HeadRefOid))
		cachePath = filepath.Join(rc.OutputHandler.LogDir, "cache", "pre-explainers", hex.EncodeToString(h.Sum(nil))+".txt")

		if data, err := os.ReadFile(cachePath); err == nil {
			return prompt, ModelResult{Text: string(data), Model: "cached"}, 0, matchedPrimers, nil
		}
	}

	profile, ok := rc.Config.ModelProfiles[rc.ActiveProfile]
	if !ok {
		return "", ModelResult{}, 0, nil, fmt.Errorf("active profile %s not found in config", rc.ActiveProfile)
	}

	modelCfg, ok := profile[p.ModelCategory]
	if !ok {
		return "", ModelResult{}, 0, nil, fmt.Errorf("no model mapping for category %s in profile %s", p.ModelCategory, rc.ActiveProfile)
	}

	client, err := rc.ClientPool.Get(ctx, modelCfg.Provider, modelCfg.Model, modelCfg.ReasoningLevel)
	if err != nil {
		return "", ModelResult{}, 0, nil, fmt.Errorf("error creating client: %w", err)
	}

	maxTokens := 0
	if modelCfg.MaxTokens != nil {
		maxTokens = *modelCfg.MaxTokens
	}
	if p.MaxTokens != nil {
		maxTokens = *p.MaxTokens
	}
	if rc.Settings.MaxTokens != nil {
		maxTokens = *rc.Settings.MaxTokens
	}

	if rc.Settings.PromptOnly && !(p.Role == "explainer" && p.Stage == "pre") {
		return prompt, ModelResult{}, 0, matchedPrimers, nil
	}

	personaCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	start := time.Now()
	var result ModelResult
	if p.Role == "explainer" && p.Stage == "pre" {
		result, err = client.GenerateJSON(personaCtx, prompt, maxTokens)
	} else {
		result, err = client.Generate(personaCtx, prompt, maxTokens)
	}
	if err != nil {
		return prompt, ModelResult{}, 0, matchedPrimers, err
	}
	elapsed := time.Since(start)

	if cachePath != "" {
		_ = os.MkdirAll(filepath.Dir(cachePath), 0755)
		_ = os.WriteFile(cachePath, []byte(result.Text), 0644)
	}

	return prompt, result, elapsed, matchedPrimers, nil
}

func (pr PersonaRun) Execute(ctx context.Context, rc *RunConfig, rr *RunResults) error {
	roleStr := strings.Title(pr.Persona.Role)
	if pr.Persona.Role == "explainer" {
		roleStr = fmt.Sprintf("Explainer (%s)", strings.Title(pr.Persona.Stage))
	}
	rc.OutputHandler.Printf("    -> %s: %s\n", roleStr, pr.Persona.ColoredID)
	prompt, result, elapsed, matchedPrimers, err := pr.Persona.Run(ctx, rc, rr, pr.Context)
	if err != nil {
		return fmt.Errorf("error executing %s: %w", pr.Persona.ID, err)
	}

	var primerIDs []string
	for _, pm := range matchedPrimers {
		primerIDs = append(primerIDs, pm.Primer.ID)
	}
	if len(primerIDs) > 0 {
		rc.OutputHandler.Printf("    -> Included primers: %s\n", strings.Join(primerIDs, ", "))
	}

	rc.OutputHandler.Printf("    <- Finished %s in %s\n", pr.Persona.ColoredID, elapsed.Round(time.Millisecond))

	rc.OutputHandler.SaveRunFile(filepath.Join(pr.Persona.ID, "prompt.md"), prompt)

	if rc.Settings.PromptOnly && !(pr.Persona.Role == "explainer" && pr.Persona.Stage == "pre") {
		return nil
	}

	rc.OutputHandler.SaveRunFile(filepath.Join(pr.Persona.ID, "raw.md"), result.Text)

	// Stage-specific logic
	var findings []Finding
	switch pr.Persona.Role {
	case "explainer":
		if pr.Persona.Stage == "pre" {
			analyses, err := ParsePreRunExplainerOutput(result.Text)
			if err != nil {
				rc.OutputHandler.Printf("Warning: error parsing pre-run explainer output for %s: %v\n", pr.Persona.ColoredID, err)
			} else {
				parsedData, _ := json.MarshalIndent(analyses, "", "  ")
				rc.OutputHandler.SaveRunFile(filepath.Join(pr.Persona.ID, "parsed.json"), string(parsedData))
				for _, a := range analyses {
					rr.AddPreRunAnalysis(a.File, fmt.Sprintf("%s: %s", pr.Persona.ID, a.Analysis))
				}
			}
		} else {
			rr.AddPostRunOutput(fmt.Sprintf("### %s\n\n%s", rc.OutputHandler.MarkPersona(pr.Persona.ID), result.Text))
		}
	case "reviewer":
		rc.OutputHandler.Printf("    -> Normalizing findings for %s...\n", pr.Persona.ColoredID)
		normStart := time.Now()
		var normResult ModelResult
		var err error
		findings, normResult, err = NormalizePersonaOutput(ctx, rc.FastestClient, pr.Persona.ID, result.Text)
		normElapsed := time.Since(normStart)
		if err != nil {
			rc.OutputHandler.Printf("Warning: error normalizing findings for %s: %v. Treating as zero findings.\n", pr.Persona.ColoredID, err)
		} else {
			rr.AddFindings(findings)
			findingsData, _ := json.MarshalIndent(findings, "", "  ")
			rc.OutputHandler.SaveRunFile(filepath.Join(pr.Persona.ID, "findings.json"), string(findingsData))
		}

		// Log Normalization usage
		// We need to find the model config for the fastest model to get its price
		profile := rc.Config.ModelProfiles[rc.ActiveProfile]
		fastestCfg := profile[string(FastestGood)]
		if fastestCfg.Model == "" { // Fallback if not found
			fastestCfg = profile[string(Balanced)]
		}

		normEntry := RunLogEntry{
			PersonaID:       "normalization:" + pr.Persona.ID,
			Model:           normResult.Model,
			TokensIn:        normResult.TokensIn,
			TokensOut:       normResult.TokensOut,
			TokensReasoning: normResult.TokensReasoning,
			TimeMS:          normElapsed.Milliseconds(),
			InputPrice:      fastestCfg.InputPricePerMillion,
			OutputPrice:     fastestCfg.OutputPricePerMillion,
			FinishReason:    normResult.FinishReason,
		}
		rr.AddStat(normEntry)
		rc.OutputHandler.LogRun(normEntry)
	}

	entry := RunLogEntry{
		PersonaID:       pr.Persona.ID,
		Model:           result.Model,
		TokensIn:        result.TokensIn,
		TokensOut:       result.TokensOut,
		TokensReasoning: result.TokensReasoning,
		TimeMS:          elapsed.Milliseconds(),
		RawOutput:       result.Text,
		Findings:        findings,
		Primers:         primerIDs,
		InputPrice:      rc.Config.ModelProfiles[rc.ActiveProfile][pr.Persona.ModelCategory].InputPricePerMillion,
		OutputPrice:     rc.Config.ModelProfiles[rc.ActiveProfile][pr.Persona.ModelCategory].OutputPricePerMillion,
		FinishReason:    result.FinishReason,
	}

	rr.AddStat(entry)
	rc.OutputHandler.LogRun(entry)

	return nil
}

// end of file
