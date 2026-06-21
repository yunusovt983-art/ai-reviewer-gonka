package main

import (
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

//go:embed agent_handoff.md.tmpl
var handoffTemplateSource string

var handoffTemplate = template.Must(template.New("handoff").Parse(handoffTemplateSource))

func main() {
	s := NewRunSettings()

	var err error
	ctx := context.Background()

	// 1. Check dependencies
	fmt.Println("--- Checking dependencies...")
	if err := checkDependencies(s); err != nil {
		log.Fatal(err)
	}

	if s.IsContext() {
		if s.Command == "concepts" {
			runContextConcepts(ctx, s)
		} else {
			runContextPrimers(ctx, s)
		}
		return
	}

	// 2. Load RunConfig (Discovered settings)
	runConfig, err := NewRunConfig(ctx, s)
	if err != nil {
		log.Fatal(err)
	}

	if s.DryRun {
		return
	}

	if s.ContextEval {
		runContextEval(ctx, runConfig, s)
		return
	}

	runOne(ctx, runConfig, s)
}

func runOne(ctx context.Context, runConfig *RunConfig, s *RunSettings) {
	// 2.5 Initialize stats from diff
	runResults := NewRunResults()
	runResults.SetDiffStats(runConfig.GlobalContext)

	concurrency := s.Concurrency
	sem := make(chan struct{}, concurrency)

	// Stage 1: Pre-run Explainers
	runPersonas(ctx, runConfig.PreRunToRun, runConfig, runResults, sem, "pre-run explainers")

	// Stage 2: Reviewers
	runPersonas(ctx, runConfig.ReviewersToRun, runConfig, runResults, sem, "reviewers")

	if s.PromptOnly {
		// Stage 3: Post-run Explainers
		runPersonas(ctx, runConfig.PostRunToRun, runConfig, runResults, sem, "post-run explainers")
		runConfig.OutputHandler.Println("--- Prompt generation complete. Stopping as requested by --prompt-only.")

		// Agent Handoff
		handoff := generateAgentHandoff(runConfig, runResults)
		runConfig.OutputHandler.SaveRunFile("agent_handoff.md", handoff)

		return
	}

	// Stage 2.5: Waivers
	ApplyWaivers(ctx, runConfig, runResults)

	// 7. Aggregation Step
	runConfig.OutputHandler.Println("--- Aggregating all findings...")
	findingsData, _ := json.MarshalIndent(runResults.AllFindings, "", "  ")
	runConfig.OutputHandler.SaveRunFile("all_findings.json", string(findingsData))

	aggStart := time.Now()
	summary, aggResult, err := AggregateFindings(ctx, runConfig.BalancedClient, runResults.AllFindings)
	runResults.Summary = summary
	aggElapsed := time.Since(aggStart)
	if err != nil {
		runConfig.OutputHandler.Printf("Error aggregating findings: %v\n", err)
		runResults.Summary = "Error generating aggregated summary."
	}
	runConfig.OutputHandler.SaveRunFile("summary.md", runConfig.OutputHandler.StripMarkers(runResults.Summary))

	// Log Aggregation usage
	balancedCfg, cfgErr := runConfig.getAggregationModelConfig()
	if cfgErr != nil {
		runConfig.OutputHandler.Printf("Warning: could not resolve aggregation model pricing: %v\n", cfgErr)
	}
	aggEntry := RunLogEntry{
		PersonaID:       "aggregator",
		Model:           aggResult.Model,
		TokensIn:        aggResult.TokensIn,
		TokensOut:       aggResult.TokensOut,
		TokensReasoning: aggResult.TokensReasoning,
		TimeMS:          aggElapsed.Milliseconds(),
		InputPrice:      balancedCfg.InputPricePerMillion,
		OutputPrice:     balancedCfg.OutputPricePerMillion,
		FinishReason:    aggResult.FinishReason,
	}
	runResults.AddStat(aggEntry)
	runConfig.OutputHandler.LogRun(aggEntry)

	// Stage 3: Post-run Explainers
	runPersonas(ctx, runConfig.PostRunToRun, runConfig, runResults, sem, "post-run explainers")

	// 8. Report
	runConfig.OutputHandler.Println("--- Generating report...")
	runResults.Finish()
	runResults.Report = generateReport(s.PRNumber, s.CommitHash, runConfig.PRInfo.BaseRefOid, runConfig.PRInfo.HeadRefOid, runResults, s.FilePatterns, runConfig.OutputHandler)
	runConfig.OutputHandler.Printf("%s", runConfig.OutputHandler.Highlight(runResults.Report))
	runConfig.OutputHandler.SaveRunFile("report.md", runConfig.OutputHandler.StripMarkers(runResults.Report))

	// 9. Agent Handoff
	handoff := generateAgentHandoff(runConfig, runResults)
	runConfig.OutputHandler.SaveRunFile("agent_handoff.md", handoff)

	// 10. Stats
	runConfig.OutputHandler.SaveRunFile("stats.txt", runResults.GetStatsString())
}

func runPersonas(ctx context.Context, personas []PersonaRun, rc *RunConfig, rr *RunResults, sem chan struct{}, stageLabel string) {
	if len(personas) == 0 {
		return
	}
	rc.OutputHandler.Printf("--- Executing %d %s...\n", len(personas), stageLabel)
	var wg sync.WaitGroup
	for _, run := range personas {
		wg.Add(1)
		go func(run PersonaRun) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := run.Execute(ctx, rc, rr); err != nil {
				rc.OutputHandler.Printf("Error executing %s %s: %v, skipping\n", stageLabel, run.Persona.ColoredID, err)
			}
		}(run)
	}
	wg.Wait()
}

func runContextEval(ctx context.Context, runConfig *RunConfig, s *RunSettings) {
	runResults := NewRunResults()
	runResults.SetDiffStats(runConfig.GlobalContext)

	concurrency := s.Concurrency
	sem := make(chan struct{}, concurrency)

	// 1. Run Pre-run Explainers (needed for accuracy as per requirement)
	runPersonas(ctx, runConfig.PreRunToRun, runConfig, runResults, sem, "pre-run explainers")

	runConfig.OutputHandler.Println("--- Evaluating context windows...")

	allPersonas := append([]PersonaRun{}, runConfig.ReviewersToRun...)
	allPersonas = append(allPersonas, runConfig.PostRunToRun...)

	profile := runConfig.Config.ModelProfiles[runConfig.ActiveProfile]

	type csvRow struct {
		persona     string
		category    string
		subcategory string
		model       string
		chars       int
		tokens      int
		cost        float64
	}
	var csvRows []csvRow

	for _, pr := range allPersonas {
		modelCfg, ok := profile[pr.Persona.ModelCategory]
		modelName := ""
		inputPrice := 0.0
		if ok {
			modelName = modelCfg.Model
			if modelCfg.ReasoningLevel != "" && modelCfg.ReasoningLevel != "none" {
				modelName = fmt.Sprintf("%s(%s)", modelName, modelCfg.ReasoningLevel)
			}
			inputPrice = modelCfg.InputPricePerMillion
		}

		pb := &PromptBuilder{
			Persona:            pr.Persona,
			PRContext:          pr.Context,
			GlobalInstructions: runConfig.Config.GlobalInstructions,
			PreRunAnalyses:     runResults.PreRunAnalyses,
			Summary:            "", // No summary yet
			MatchedPrimers:     runConfig.FindMatches(pr.Context),
			PrimerTypes:        runConfig.Config.PrimerTypes,
			Model:              modelName,
		}
		_, breakdown := pb.Build()

		runConfig.OutputHandler.Printf("\nPersona: %s (%s)\n", pr.Persona.ColoredID, modelName)

		// Group by category for console output
		type catStats struct {
			chars  int
			tokens int
		}
		categories := make(map[string]catStats)

		for _, entry := range breakdown.Entries {
			csvRows = append(csvRows, csvRow{
				persona:     pr.Persona.ID,
				category:    entry.Category,
				subcategory: entry.Subcategory,
				model:       modelName,
				chars:       entry.Chars,
				tokens:      entry.Tokens,
				cost:        (float64(entry.Tokens) * inputPrice) / 1000000.0,
			})

			stats := categories[entry.Category]
			stats.chars += entry.Chars
			stats.tokens += entry.Tokens
			categories[entry.Category] = stats
		}

		orderedCats := []string{"instructions", "global_instructions", "findings", "primers", "metadata", "file_list", "pre_explainer", "diff", "system_prompt"}
		for _, cat := range orderedCats {
			if stats, ok := categories[cat]; ok {
				label := strings.ReplaceAll(cat, "_", " ")
				// Capitalize first letter of label
				if len(label) > 0 {
					label = strings.ToUpper(label[:1]) + label[1:]
				}
				runConfig.OutputHandler.Printf("  %s: %d tokens (%d chars)\n", label, stats.tokens, stats.chars)
			}
		}
		runConfig.OutputHandler.Printf("  Total: %d tokens (%d chars)\n", breakdown.TotalTokens, breakdown.TotalChars)
	}

	if s.ContextEvalCSV != "" {
		f, err := os.Create(s.ContextEvalCSV)
		if err != nil {
			runConfig.OutputHandler.Printf("Error creating CSV file: %v\n", err)
			return
		}
		defer f.Close()

		writer := csv.NewWriter(f)
		defer writer.Flush()

		writer.Write([]string{"tokens", "chars", "cost", "model", "persona", "category", "label", "path"})
		writer.Write([]string{"Integer", "Integer", "Float", "String", "String", "String", "String", "StringPath"})
		for _, row := range csvRows {
			parts := []string{row.category}
			if row.subcategory != "" {
				subparts := strings.Split(row.subcategory, "/")
				parts = append(parts, subparts...)
			}
			path := strings.Join(parts, ",")
			label := parts[len(parts)-1]
			writer.Write([]string{
				strconv.Itoa(row.tokens),
				strconv.Itoa(row.chars),
				fmt.Sprintf("%.6f", row.cost),
				row.model,
				row.persona,
				row.category,
				label,
				path,
			})
		}
		runConfig.OutputHandler.Printf("\nContext evaluation saved to %s\n", s.ContextEvalCSV)
	}
}

func checkDependencies(s *RunSettings) error {
	if !s.IsContext() {
		if _, err := exec.LookPath("gh"); err != nil {
			return fmt.Errorf("github cli (gh) is not installed")
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is not installed")
	}
	return nil
}

func generateReport(prNumber, commitHash, baseSHA, headSHA string, rr *RunResults, filePatterns []string, oh *OutputHandler) string {
	var out strings.Builder
	out.WriteString("# AI Review Report\n\n")
	if filePatterns != nil {
		out.WriteString(fmt.Sprintf("## Files on %s\n", headSHA))
		out.WriteString(fmt.Sprintf("- **Patterns:** `%v`\n", filePatterns))
	} else if prNumber != "" {
		out.WriteString(fmt.Sprintf("## PR #%s\n", prNumber))
	} else {
		out.WriteString(fmt.Sprintf("## Commit %s\n", headSHA[:8]))
	}
	out.WriteString(fmt.Sprintf("- **Base Commit:** `%s`\n", baseSHA))
	out.WriteString(fmt.Sprintf("- **Head Commit:** `%s`\n\n", headSHA))
	out.WriteString(oh.LinkPersonas(rr.Summary))
	out.WriteString("\n\n")

	if len(rr.PostRunOutputs) > 0 {
		out.WriteString("## Explanations\n\n")
		for _, output := range rr.PostRunOutputs {
			out.WriteString(oh.LinkPersonas(output))
			out.WriteString("\n\n")
		}
	}

	out.WriteString("## Stats\n")
	totalIn := 0
	totalOut := 0
	totalReasoning := 0
	totalCost := 0.0

	type mStats struct {
		in, out, reasoning int
		cost               float64
	}
	modelStats := make(map[string]mStats)

	for _, s := range rr.Stats {
		cost := s.Cost()

		warning := ""
		if s.FinishReason != "" && s.FinishReason != "STOP" && s.FinishReason != "stop" && s.FinishReason != "end_turn" && s.FinishReason != "FinishReasonStop" {
			warning = fmt.Sprintf(" ⚠️ **Warning: %s**", s.FinishReason)
		}

		reasoningStr := ""
		if s.TokensReasoning > 0 {
			reasoningStr = fmt.Sprintf(" (Thinking: %d)", s.TokensReasoning)
		}

		out.WriteString(fmt.Sprintf("- %s (%s): In: %d, Out: %d%s, Time: %dms, Cost: $%.6f%s\n", oh.LinkPersonas(oh.MarkPersona(s.PersonaID)), s.Model, s.TokensIn, s.TokensOut, reasoningStr, s.TimeMS, cost, warning))
		totalIn += s.TokensIn
		totalOut += s.TokensOut
		totalReasoning += s.TokensReasoning
		totalCost += cost

		ms := modelStats[s.Model]
		ms.in += s.TokensIn
		ms.out += s.TokensOut
		ms.reasoning += s.TokensReasoning
		ms.cost += cost
		modelStats[s.Model] = ms
	}

	totalTokensStr := fmt.Sprintf("%d (In: %d, Out: %d)", totalIn+totalOut, totalIn, totalOut)
	if totalReasoning > 0 {
		totalTokensStr += fmt.Sprintf(", Thinking: %d", totalReasoning)
	}
	out.WriteString(fmt.Sprintf("\nTotal Tokens: %s\n", totalTokensStr))
	out.WriteString(fmt.Sprintf("Total Wall Time: %s\n", rr.TotalElapsed.Round(time.Millisecond)))

	out.WriteString("\n### Usage by Model\n")
	for model, ms := range modelStats {
		usageStr := fmt.Sprintf("%d tokens (In: %d, Out: %d)", ms.in+ms.out, ms.in, ms.out)
		if ms.reasoning > 0 {
			usageStr += fmt.Sprintf(", Thinking: %d", ms.reasoning)
		}
		out.WriteString(fmt.Sprintf("- %s: %s, Cost: $%.6f\n", model, usageStr, ms.cost))
	}
	out.WriteString(fmt.Sprintf("\n### Estimated Total Cost: $%.6f\n", totalCost))

	if len(rr.WaivedFindings) > 0 {
		out.WriteString("\n## Waived Issues\n\n")
		for _, f := range rr.WaivedFindings {
			location := f.File
			if f.LineStart != nil {
				location = fmt.Sprintf("%s:%d", location, *f.LineStart)
				if f.LineEnd != nil {
					location = fmt.Sprintf("%s-%d", location, *f.LineEnd)
				}
			}
			out.WriteString(fmt.Sprintf("- **%s** (%s)\n", f.Summary, location))
			out.WriteString(fmt.Sprintf("  %s\n\n", f.Details))
		}
	}

	return out.String()
}

type handoffData struct {
	Repository      string
	ReviewType      string
	PRNumber        string
	PRTitle         string
	BaseSHA         string
	HeadSHA         string
	BaseRef         string
	HeadRef         string
	FilePatterns    string
	PRBody          string
	Summary         string
	Primers         []handoffPrimer
	RunDir          string
	PreRunPersonas  []string
	ReviewPersonas  []string
	PostRunPersonas []string
}

type handoffPrimer struct {
	ID      string
	Content string
}

func generateAgentHandoff(rc *RunConfig, rr *RunResults) string {
	data := handoffData{
		Repository: rc.Settings.Repo,
		ReviewType: rc.Settings.Command,
		PRNumber:   rc.Settings.PRNumber,
		RunDir:     rc.RunDir,
	}

	if rc.PRInfo != nil {
		data.PRTitle = rc.PRInfo.Title
		data.BaseSHA = rc.PRInfo.BaseRefOid
		data.HeadSHA = rc.PRInfo.HeadRefOid
		data.BaseRef = rc.PRInfo.BaseRefName
		data.HeadRef = rc.PRInfo.HeadRefName
		data.PRBody = rc.PRInfo.Body
	} else if rc.Settings.CommitHash != "" {
		data.BaseSHA = rc.Settings.CompareTo
		data.HeadSHA = rc.Settings.CommitHash
	}

	if rc.Settings.FilePatterns != nil {
		data.FilePatterns = fmt.Sprintf("%v", rc.Settings.FilePatterns)
	}

	if rr.Summary != "" {
		data.Summary = rc.OutputHandler.StripMarkers(rr.Summary)
	}

	matches := rc.FindMatches(rc.GlobalContext)
	if len(matches) > 0 {
		seen := make(map[string]bool)
		for _, m := range matches {
			if seen[m.Primer.ID] {
				continue
			}
			seen[m.Primer.ID] = true
			data.Primers = append(data.Primers, handoffPrimer{
				ID:      m.Primer.ID,
				Content: m.Primer.Content,
			})
		}
	}

	for _, pr := range rc.PreRunToRun {
		data.PreRunPersonas = append(data.PreRunPersonas, pr.Persona.ID)
	}
	for _, pr := range rc.ReviewersToRun {
		data.ReviewPersonas = append(data.ReviewPersonas, pr.Persona.ID)
	}
	for _, pr := range rc.PostRunToRun {
		data.PostRunPersonas = append(data.PostRunPersonas, pr.Persona.ID)
	}

	var out strings.Builder
	if err := handoffTemplate.Execute(&out, data); err != nil {
		return fmt.Sprintf("Error generating handoff: %v", err)
	}

	return out.String()
}
