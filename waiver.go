package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// LineRange is defined in internal/domain/codebase and aliased in context.go.

type Waiver struct {
	ID            string    `yaml:"id"`
	AIReview      string    `yaml:"ai_review"`
	ModelCategory string    `yaml:"model_category"`
	Filters       FilterSet `yaml:",inline"`
	Instructions  string
}

type WaiverMatch struct {
	Waiver    Waiver
	AppliedTo []string
}

type WaiverEvaluation struct {
	Applies   bool    `json:"applies"`
	Certainty float64 `json:"certainty"`
	Why       string  `json:"why"`
}

func LoadWaivers(searchPaths []string, repo string, headSHA string, oh *OutputHandler) ([]Waiver, error) {
	scanner := NewScanner(searchPaths, repo, headSHA, oh)
	results, err := scanner.Load("waiver", func() any { return &Waiver{} })
	if err != nil && len(results) == 0 {
		return nil, err
	}
	if err != nil {
		oh.Printf("Warning: issues encountered while loading waivers: %v\n", err)
	}

	var waivers []Waiver
	for _, res := range results {
		w := res.Raw.(*Waiver)
		w.Instructions = res.Instructions
		waivers = append(waivers, *w)
	}

	return waivers, nil
}

func ApplyWaivers(ctx context.Context, rc *RunConfig, rr *RunResults) {
	if len(rc.Waivers) == 0 || len(rr.AllFindings) == 0 {
		return
	}

	rc.OutputHandler.Println("--- Checking waivers...")

	var remainingFindings []Finding
	for _, finding := range rr.AllFindings {
		waived := false
		var applicableWaivers []Waiver

		// 1. Filter waivers based on location
		for _, w := range rc.Waivers {
			// Find FileContext for this finding
			var fileCtx *FileContext
			for i := range rc.GlobalContext.Files {
				if rc.GlobalContext.Files[i].Filename == finding.File {
					fileCtx = &rc.GlobalContext.Files[i]
					break
				}
			}

			if fileCtx == nil {
				continue
			}

			fs := w.Filters
			if err := fs.Compile(); err != nil {
				rc.OutputHandler.Printf("    Warning: error compiling filters for waiver %s: %v\n", w.ID, err)
				continue
			}

			if fileCtx.Matches(FileMatchOptions{
				FilterSet:      &fs,
				Branch:         rc.GlobalContext.Branch,
				CommitDate:     rc.GlobalContext.CommitDate,
				FindingSummary: finding.Summary,
				FindingDetails: finding.Details,
			}) {
				// Also check line numbers if specified
				if len(fs.LineNumberFilters) > 0 {
					lineMatch := false
					findingStart := 0
					if finding.LineStart != nil {
						findingStart = *finding.LineStart
					}
					findingEnd := findingStart
					if finding.LineEnd != nil {
						findingEnd = *finding.LineEnd
					}

					for _, r := range fs.LineNumberFilters {
						if findingStart >= r.Start && findingEnd <= r.End {
							lineMatch = true
							break
						}
					}
					if !lineMatch {
						continue
					}
				}

				applicableWaivers = append(applicableWaivers, w)
			}
		}

		if len(applicableWaivers) > 0 {
			// 2. Use LLM to confirm if waiver applies
			confirmedWaiver, evaluation, err := evaluateWaivers(ctx, rc, finding, applicableWaivers)
			if err == nil && evaluation.Applies {
				rc.OutputHandler.Printf("    -> Finding in %s waived by %s (Certainty: %.2f): %s\n", finding.File, confirmedWaiver.ID, evaluation.Certainty, evaluation.Why)
				finding.Details = fmt.Sprintf("%s\n\n[Waived by %s: %s]", finding.Details, confirmedWaiver.ID, evaluation.Why)
				rr.WaivedFindings = append(rr.WaivedFindings, finding)
				waived = true
			} else if err != nil {
				rc.OutputHandler.Printf("    Warning: error evaluating waivers for %s: %v\n", finding.File, err)
			}
		}

		if !waived {
			remainingFindings = append(remainingFindings, finding)
		}
	}

	rr.AllFindings = remainingFindings
}

func evaluateWaivers(ctx context.Context, rc *RunConfig, finding Finding, waivers []Waiver) (*Waiver, WaiverEvaluation, error) {
	// For simplicity, we'll just check against the first applicable waiver's instructions and model
	// If multiple apply, we could combine them, but the requirement says "The waiver(s) (the content under the .md)"
	// and "Run this context with the model specified in the Waiver".

	w := waivers[0]

	modelCategory := w.ModelCategory
	if modelCategory == "" {
		modelCategory = string(FastestGood)
	}

	profile, ok := rc.Config.ModelProfiles[rc.ActiveProfile]
	if !ok {
		return nil, WaiverEvaluation{}, fmt.Errorf("active profile %s not found in config", rc.ActiveProfile)
	}

	modelCfg, ok := profile[modelCategory]
	if !ok {
		modelCfg = profile[string(Balanced)]
	}

	client, err := GetModelClient(ctx, modelCfg.Provider, modelCfg.Model, modelCfg.ReasoningLevel)
	if err != nil {
		return nil, WaiverEvaluation{}, err
	}

	// 1. The diff for just the file or files that the issue applied to.
	var fileDiff string
	for _, f := range rc.GlobalContext.Files {
		if f.Filename == finding.File {
			fileDiff = f.Diff
			break
		}
	}

	// 2. The description and location of the issue found.
	location := finding.File
	if finding.LineStart != nil {
		location = fmt.Sprintf("%s:%d", location, *finding.LineStart)
		if finding.LineEnd != nil {
			location = fmt.Sprintf("%s-%d", location, *finding.LineEnd)
		}
	}
	issueContext := fmt.Sprintf("Issue: %s\nDetails: %s\nLocation: %s", finding.Summary, finding.Details, location)

	// 3. The waiver(s) (the content under the .md)
	var waiverInstructions strings.Builder
	for _, waiver := range waivers {
		waiverInstructions.WriteString(fmt.Sprintf("--- WAIVER: %s ---\n%s\n", waiver.ID, waiver.Instructions))
	}

	prompt := fmt.Sprintf(`You are an assistant determining if a code review finding should be waived based on provided waivers.

CODE DIFF:
%s

ISSUE:
%s

APPLICABLE WAIVERS:
%s

Specify if any of these waivers apply to this issue.
OUTPUT FORMAT (JSON ONLY):
{
  "applies": bool,
  "certainty": float (0-1),
  "why": "short explanation"
}
`, fileDiff, issueContext, waiverInstructions.String())

	result, err := client.GenerateJSON(ctx, prompt, 0)
	if err != nil {
		return nil, WaiverEvaluation{}, err
	}

	text := extractJSON(result.Text)
	var evaluation WaiverEvaluation
	if err := json.Unmarshal([]byte(text), &evaluation); err != nil {
		return nil, WaiverEvaluation{}, err
	}

	return &w, evaluation, nil
}
