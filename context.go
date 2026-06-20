package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"ai-reviewer/internal/domain/codebase"

	"github.com/pkoukk/tiktoken-go"
)

// Domain model lives in internal/domain/codebase. These aliases keep the rest of
// package main (and its tests) referring to the types/functions unqualified while
// the pure logic is owned by the domain package. Git acquisition (below) is infra
// and depends on the domain, not the reverse.
type (
	PRInfo           = codebase.PRInfo
	PRContext        = codebase.PRContext
	FileContext      = codebase.FileContext
	FilterSet        = codebase.FilterSet
	MatchOptions     = codebase.MatchOptions
	FileMatchOptions = codebase.FileMatchOptions
	LineRange        = codebase.LineRange
)

var (
	AnnotateDiff          = codebase.AnnotateDiff
	ParseAnnotatedFileDiff = codebase.ParseAnnotatedFileDiff
	pathIncluded          = codebase.PathIncluded
)

func GetPRInfo(repo, prNumber string) (*PRInfo, error) {
	fmt.Printf("    -> Running gh pr view %s...\n", prNumber)
	cmd := exec.Command("gh", "pr", "view", prNumber, "-R", repo, "--json", "title,body,baseRefName,baseRefOid,headRefName,headRefOid")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("error running gh pr view: %w, output: %s", err, string(output))
	}

	var pr PRInfo
	if err := json.Unmarshal(output, &pr); err != nil {
		return nil, fmt.Errorf("error unmarshaling gh output: %w", err)
	}

	// Fetch commit date for the head of the PR
	cmd = exec.Command("git", "show", "-s", "--format=%cI", pr.HeadRefOid)
	dateOutput, err := cmd.Output()
	if err == nil {
		dateStr := strings.TrimSpace(string(dateOutput))
		if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
			pr.CommitDate = t
		}
	}

	return &pr, nil
}

func GetCommitInfo(commitHash, compareTo string) (*PRInfo, error) {
	fmt.Printf("    -> Getting info for commit %s...\n", commitHash)

	// Get commit message and date
	cmd := exec.Command("git", "show", "-s", "--format=%s%n%n%b%n--DATE--%n%cI", commitHash)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("error getting commit info: %w, output: %s", err, string(output))
	}

	fullContent := string(output)
	parts := strings.Split(fullContent, "\n--DATE--\n")
	msgPart := strings.TrimSpace(parts[0])
	dateStr := ""
	if len(parts) > 1 {
		dateStr = strings.TrimSpace(parts[1])
	}

	msgLines := strings.SplitN(msgPart, "\n", 2)
	title := msgLines[0]
	body := ""
	if len(msgLines) > 1 {
		body = strings.TrimSpace(msgLines[1])
	}

	var commitDate time.Time
	if dateStr != "" {
		if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
			commitDate = t
		}
	}

	// Get full SHA for head
	cmd = exec.Command("git", "rev-parse", commitHash)
	headSHA, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("error resolving commit hash: %w", err)
	}

	baseSHA := compareTo
	if baseSHA == "" {
		// Default to parent commit
		cmd = exec.Command("git", "rev-parse", commitHash+"^")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("error getting parent commit: %w", err)
		}
		baseSHA = strings.TrimSpace(string(out))
	} else {
		// Resolve compareTo to full SHA
		cmd = exec.Command("git", "rev-parse", compareTo)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("error resolving comparison commit: %w", err)
		}
		baseSHA = strings.TrimSpace(string(out))
	}

	return &PRInfo{
		Title:       title,
		Body:        body,
		BaseRefOid:  baseSHA,
		HeadRefOid:  strings.TrimSpace(string(headSHA)),
		IsCommit:    true,
		CommitDate:  commitDate,
		BaseRefName: baseSHA[:8], // Short SHA for display
		HeadRefName: strings.TrimSpace(string(headSHA))[:8],
	}, nil
}

func GetFileInfo(repo, branch string, filePatterns []string) (*PRInfo, error) {
	fmt.Printf("    -> Getting info for branch %s, files %v...\n", branch, filePatterns)

	// Get head SHA of branch
	cmd := exec.Command("git", "rev-parse", branch)
	headSHAOut, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("error resolving branch %s: %w", branch, err)
	}
	headSHA := strings.TrimSpace(string(headSHAOut))

	// Get commit date
	cmd = exec.Command("git", "show", "-s", "--format=%cI", headSHA)
	dateOutput, err := cmd.Output()
	var commitDate time.Time
	if err == nil {
		dateStr := strings.TrimSpace(string(dateOutput))
		if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
			commitDate = t
		}
	}

	// Resolve patterns to actual files
	fs := &FilterSet{IncludeFilters: filePatterns}
	files, err := GetFilesForPatterns(fs, branch)
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no files found matching patterns %v on branch %s", filePatterns, branch)
	}

	return &PRInfo{
		Title:        fmt.Sprintf("Review of %d files on %s", len(files), branch),
		Body:         fmt.Sprintf("Reviewing files: %s", strings.Join(files, ", ")),
		BaseRefOid:   headSHA, // We'll use this as a hack for GetPRContext
		HeadRefOid:   headSHA,
		IsCommit:     false,
		CommitDate:   commitDate,
		BaseRefName:  branch,
		HeadRefName:  branch,
		FilePatterns: filePatterns,
	}, nil
}

func GetBranchesInfo(repo, base, head string) (*PRInfo, error) {
	fmt.Printf("    -> Getting comparison info for branches %s...%s...\n", base, head)

	resolveRef := func(ref string) (string, error) {
		// Try resolving as is
		cmd := exec.Command("git", "rev-parse", ref)
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}

		// Try resolving as origin/ref
		cmd = exec.Command("git", "rev-parse", "origin/"+ref)
		out, err = cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}

		// Try resolving as FETCH_HEAD if it was just fetched
		// But we have two refs, so FETCH_HEAD is not reliable.

		return "", fmt.Errorf("error resolving ref %s: %w", ref, err)
	}

	// Get head SHA of base
	baseSHA, err := resolveRef(base)
	if err != nil {
		return nil, fmt.Errorf("error resolving base branch %s: %w", base, err)
	}

	// Get head SHA of head
	headSHA, err := resolveRef(head)
	if err != nil {
		return nil, fmt.Errorf("error resolving head branch %s: %w", head, err)
	}

	// Get commit date of head
	cmd := exec.Command("git", "show", "-s", "--format=%cI", headSHA)
	dateOutput, err := cmd.Output()
	var commitDate time.Time
	if err == nil {
		dateStr := strings.TrimSpace(string(dateOutput))
		if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
			commitDate = t
		}
	}

	return &PRInfo{
		Title:       fmt.Sprintf("Review comparison %s...%s", base, head),
		Body:        fmt.Sprintf("Comparing branch %s (base) with %s (head)", base, head),
		BaseRefOid:  baseSHA,
		HeadRefOid:  headSHA,
		IsCommit:    false,
		CommitDate:  commitDate,
		BaseRefName: base,
		HeadRefName: head,
	}, nil
}

func GetPRContext(prInfo *PRInfo, fs *FilterSet) (*PRContext, error) {
	if fs != nil {
		if err := fs.Compile(); err != nil {
			return nil, err
		}
	}

	if prInfo.BaseRefOid == prInfo.HeadRefOid && !prInfo.IsCommit && prInfo.BaseRefOid != "" {
		// This is "file" mode, we want to see the whole content of the files as if it were a new file
		fsToUse := fs
		if fsToUse == nil {
			fsToUse = &FilterSet{}
		}
		files, err := GetFilesForPatterns(fsToUse, prInfo.HeadRefOid)
		if err != nil {
			return nil, err
		}

		var finalFiles []FileContext
		for _, file := range files {
			// Get content of the file at HeadRefOid
			cmd := exec.Command("git", "show", fmt.Sprintf("%s:%s", prInfo.HeadRefOid, file))
			content, err := cmd.Output()
			if err != nil {
				// Maybe it's a directory or doesn't exist anymore
				continue
			}

			var diffBuilder strings.Builder
			diffBuilder.WriteString(fmt.Sprintf("+++ b/%s\n", file))
			contentStr := string(content)
			lines := strings.Split(contentStr, "\n")
			// Fake a diff chunk
			diffBuilder.WriteString(fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(lines)))
			for _, line := range lines {
				diffBuilder.WriteString("+" + line + "\n")
			}

			annDiff, funcs := AnnotateDiff(diffBuilder.String())
			fileCtx := FileContext{
				Filename:     file,
				Diff:         annDiff,
				ChangedLines: lines,
				Functions:    funcs,
			}

			if fileCtx.Matches(FileMatchOptions{
				FilterSet:  fs,
				Branch:     prInfo.HeadRefName,
				CommitDate: prInfo.CommitDate,
			}) {
				finalFiles = append(finalFiles, fileCtx)
			}
		}

		return &PRContext{
			Title:       prInfo.Title,
			Description: prInfo.Body,
			Files:       finalFiles,
			Branch:      prInfo.HeadRefName,
			CommitDate:  prInfo.CommitDate,
		}, nil
	}

	fsToUse := fs
	if fsToUse == nil {
		fsToUse = &FilterSet{}
	}
	diff, err := GetDiff(fsToUse, prInfo.BaseRefOid, prInfo.HeadRefOid)
	if err != nil {
		return nil, err
	}

	var finalFiles []FileContext

	// Split diff into files
	// Git diff output starts with "diff --git" for each file
	fileDiffs := strings.Split(diff, "diff --git ")
	for i, fd := range fileDiffs {
		if i == 0 && !strings.HasPrefix(fd, "diff --git ") && fd != "" {
			// Header before first file diff if any
			continue
		}
		if fd == "" {
			continue
		}

		annDiff, funcs := AnnotateDiff("diff --git " + fd)
		fileCtx := ParseAnnotatedFileDiff(annDiff)
		fileCtx.Functions = funcs

		if fileCtx.Filename != "" && fileCtx.Matches(FileMatchOptions{
			FilterSet:  fs,
			Branch:     prInfo.HeadRefName,
			CommitDate: prInfo.CommitDate,
		}) {
			finalFiles = append(finalFiles, fileCtx)
		}
	}

	return &PRContext{
		Title:       prInfo.Title,
		Description: prInfo.Body,
		Files:       finalFiles,
		Branch:      prInfo.HeadRefName,
		CommitDate:  prInfo.CommitDate,
	}, nil
}

// GetFilesForPatterns lists files on a ref that match the filter's path globs.
// (Was a *FilterSet method; now a free function so the domain FilterSet stays
// free of git/exec.)
func GetFilesForPatterns(fs *FilterSet, branch string) ([]string, error) {
	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", branch)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("error running git ls-tree: %w", err)
	}
	allFiles := strings.Split(strings.TrimSpace(string(out)), "\n")

	var result []string
	for _, file := range allFiles {
		if file == "" {
			continue
		}

		if fs.MatchesPath(file) {
			result = append(result, file)
		}
	}

	return result, nil
}

func GetDiff(fs *FilterSet, baseSHA, headSHA string) (string, error) {
	// Triple-dot (A...B) means diff from common ancestor of A and B to B.
	// Double-dot (A..B) means diff from A to B.
	// For PRs we usually want triple-dot.
	// For "ai-review commit" we probably want double-dot if a base is specified,
	// or triple-dot if comparing to parent (which ends up being same as double-dot).
	// Let's use triple-dot as it's generally safer for PR-like workflows.
	args := []string{"diff", fmt.Sprintf("%s...%s", baseSHA, headSHA)}
	if len(fs.IncludeFilters) > 0 || len(fs.ExcludeFilters) > 0 || len(fs.GlobalExcludes) > 0 {
		args = append(args, "--")
		for _, f := range fs.IncludeFilters {
			args = append(args, f)
		}
		for _, f := range fs.ExcludeFilters {
			args = append(args, ":(exclude)"+f)
		}
		for _, f := range fs.GlobalExcludes {
			// Only exclude if not explicitly included
			if !pathIncluded(f, fs.IncludeFilters, false) {
				args = append(args, ":(exclude)"+f)
			}
		}
	}

	cmd := exec.Command("git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("error running git diff: %w, output: %s", err, string(output))
	}
	return string(output), nil
}

func GetChangedFiles(fs *FilterSet, baseSHA, headSHA string) ([]string, error) {
	args := []string{"diff", "--name-only", fmt.Sprintf("%s...%s", baseSHA, headSHA)}
	if len(fs.IncludeFilters) > 0 || len(fs.ExcludeFilters) > 0 {
		args = append(args, "--")
		for _, f := range fs.IncludeFilters {
			args = append(args, f)
		}
		for _, f := range fs.ExcludeFilters {
			args = append(args, ":(exclude)"+f)
		}
	}

	cmd := exec.Command("git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("error running git diff --name-only: %w, output: %s", err, string(output))
	}
	files := strings.Split(strings.TrimSpace(string(output)), "\n")

	var result []string
	for _, f := range files {
		if f != "" {
			result = append(result, f)
		}
	}
	return result, nil
}

type PromptBuilder struct {
	Persona            Persona
	PRContext          *PRContext
	GlobalInstructions string
	PreRunAnalyses     map[string][]string
	Summary            string
	MatchedPrimers     []PrimerMatch
	PrimerTypes        map[string]PrimerType
	Model              string
}

type BreakdownEntry struct {
	Category    string
	Subcategory string
	Content     string
	Chars       int
	Tokens      int
}

type PromptBreakdown struct {
	Entries     []BreakdownEntry
	TotalChars  int
	TotalTokens int
}

func (pb *PromptBuilder) CountTokens(text string) int {
	if pb.Model == "" {
		return len(text) / 4 // Rough estimate if no model
	}

	encoding := "cl100k_base"
	model := pb.Model
	if idx := strings.Index(model, "("); idx != -1 {
		model = model[:idx]
	}
	tke, err := tiktoken.EncodingForModel(model)
	if err != nil {
		// Fallback to cl100k_base which is common for many modern models
		tke, err = tiktoken.GetEncoding(encoding)
		if err != nil {
			return len(text) / 4
		}
	}

	token := tke.Encode(text, nil, nil)
	return len(token)
}

func (pb *PromptBuilder) Build() (string, PromptBreakdown) {
	p := pb.Persona
	ctx := pb.PRContext
	preRunAnalyses := pb.PreRunAnalyses
	summary := pb.Summary
	matchedPrimers := pb.MatchedPrimers
	primerTypes := pb.PrimerTypes

	var breakdown PromptBreakdown

	addEntry := func(category, subcategory, content string) {
		if content == "" {
			return
		}
		chars := len(content)
		tokens := pb.CountTokens(content)
		breakdown.Entries = append(breakdown.Entries, BreakdownEntry{
			Category:    category,
			Subcategory: subcategory,
			Content:     content,
			Chars:       chars,
			Tokens:      tokens,
		})
		breakdown.TotalChars += chars
		breakdown.TotalTokens += tokens
	}

	// Persona Instructions
	personaInstText := fmt.Sprintf("# PERSONA INSTRUCTIONS\n%s\n", p.Instructions)
	addEntry("instructions", "", personaInstText)

	// Findings
	if p.IncludeFindings && summary != "" {
		findingsText := fmt.Sprintf("\n---\n# AGGREGATED REPORT\n%s\n", summary)
		addEntry("findings", "", findingsText)
	}

	// Primers
	if len(matchedPrimers) > 0 {
		for _, pm := range matchedPrimers {
			typeName := pm.Primer.Type
			typeDesc := ""
			if pt, ok := primerTypes[typeName]; ok {
				typeDesc = pt.Description
			}

			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("\n---\n# PRIMER: %s (Type: %s)\n", pm.Primer.ID, typeName))
			if typeDesc != "" {
				sb.WriteString(fmt.Sprintf("**Type Intent:** %s\n\n", typeDesc))
			}
			sb.WriteString(fmt.Sprintf("**Applies to:**\n\n- %s", strings.Join(pm.Files, "\n- ")))
			sb.WriteString("\n### Content:\n")
			sb.WriteString(pm.Primer.Content)
			sb.WriteString("\n\n")

			addEntry("primers", pm.Primer.ID, sb.String())
		}
	}

	// PR Metadata
	metadata := fmt.Sprintf(`
---
# PR METADATA
## Title
%s

## Description
%s
`, ctx.Title, ctx.Description)
	addEntry("metadata", "", metadata)

	// File List and Pre-run Analyses
	var fileListSB strings.Builder
	fileListSB.WriteString("\n---\n# CHANGED FILES\n")
	for _, file := range ctx.ChangedFiles() {
		fileLine := fmt.Sprintf("- %s\n", file)
		fileListSB.WriteString(fileLine)

		if len(p.IncludeExplainers) > 0 {
			if analyses, ok := preRunAnalyses[file]; ok {
				for _, analysis := range analyses {
					parts := strings.SplitN(analysis, ": ", 2)
					if len(parts) > 0 {
						explainerID := parts[0]
						included := false
						for _, id := range p.IncludeExplainers {
							if id == explainerID {
								included = true
								break
							}
						}
						if included {
							analysisLine := fmt.Sprintf("  - Explainer Analysis: %s\n", analysis)
							addEntry("pre_explainer", file, analysisLine)
						}
					}
				}
			}
		}
	}
	addEntry("file_list", "", fileListSB.String())

	// Diffs
	if p.ExcludeDiff {
		addedLines := 0
		deletedLines := 0
		fullDiff := ctx.FullDiff()
		scanner := bufio.NewScanner(strings.NewReader(fullDiff))
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.SplitN(line, ":", 2)
			if len(parts) < 2 {
				continue
			}
			diffLine := parts[1]
			if strings.HasPrefix(diffLine, "+") && !strings.HasPrefix(diffLine, "+++ ") {
				addedLines++
			} else if strings.HasPrefix(diffLine, "-") && !strings.HasPrefix(diffLine, "--- ") {
				deletedLines++
			}
		}
		diffStats := fmt.Sprintf("\n---\n# DIFF STATS\n%d files changed, %d lines added, %d lines deleted. (Full diff excluded by configuration)\n", len(ctx.Files), addedLines, deletedLines)
		addEntry("diff", "", diffStats)
	} else {
		for _, f := range ctx.Files {
			fence := "```"
			diffSection := fmt.Sprintf(`
---
# UNIFIED DIFF: %s
%sdiff
%s
%s
`, f.Filename, fence, f.Diff, fence)
			addEntry("diff", f.Filename, diffSection)
		}
	}

	// Global Instructions
	if pb.GlobalInstructions != "" {
		globalInstText := fmt.Sprintf("\n---\n# STANDARD INSTRUCTIONS\n%s\n", pb.GlobalInstructions)
		addEntry("global_instructions", "", globalInstText)
	}

	// System Prompt (only for pre-run explainers)
	if p.Role == "explainer" && p.Stage == "pre" {
		addEntry("system_prompt", "", PreRunExplainerSystemPrompt+"\n\n")
	}

	// Assemble final prompt from entries
	var promptSB strings.Builder
	for _, entry := range breakdown.Entries {
		promptSB.WriteString(entry.Content)
	}

	return promptSB.String(), breakdown
}

func buildPrompt(p Persona, ctx *PRContext, globalInstructions string, preRunAnalyses map[string][]string, summary string, matchedPrimers []PrimerMatch, primerTypes map[string]PrimerType) string {
	pb := &PromptBuilder{
		Persona:            p,
		PRContext:          ctx,
		GlobalInstructions: globalInstructions,
		PreRunAnalyses:     preRunAnalyses,
		Summary:            summary,
		MatchedPrimers:     matchedPrimers,
		PrimerTypes:        primerTypes,
	}
	prompt, _ := pb.Build()
	return prompt
}
