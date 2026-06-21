package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type RunLogEntry struct {
	PersonaID       string    `json:"persona_id"`
	Model           string    `json:"model"`
	TokensIn        int       `json:"tokens_in"`
	TokensOut       int       `json:"tokens_out"`
	TokensReasoning int       `json:"tokens_reasoning,omitempty"`
	TimeMS          int64     `json:"time_ms"`
	RawOutput       string    `json:"raw_output,omitempty"`
	Findings        []Finding `json:"findings,omitempty"`
	Primers         []string  `json:"primers,omitempty"`
	InputPrice      float64   `json:"input_price,omitempty"`  // Price per million tokens
	OutputPrice     float64   `json:"output_price,omitempty"` // Price per million tokens
	FinishReason    string    `json:"finish_reason,omitempty"`
}

type RunResults struct {
	Stats          []RunLogEntry
	AllFindings    []Finding
	WaivedFindings []Finding
	PostRunOutputs []string
	PreRunAnalyses map[string][]string
	Summary        string
	Report         string
	StartTime      time.Time
	TotalElapsed   time.Duration

	// Added stats
	LinesAdded   int
	LinesRemoved int
	LinesChanged int

	statsMu          sync.Mutex
	findingsMu       sync.Mutex
	postRunOutputsMu sync.Mutex
	preRunAnalysesMu sync.Mutex
}

func NewRunResults() *RunResults {
	return &RunResults{
		PreRunAnalyses: make(map[string][]string),
		StartTime:      time.Now(),
	}
}

// Cost returns the estimated USD cost for this log entry using per-million-token prices.
// Output billing includes reasoning tokens (Anthropic includes them in output_tokens).
func (e RunLogEntry) Cost() float64 {
	return (float64(e.TokensIn)*e.InputPrice + float64(e.TokensOut+e.TokensReasoning)*e.OutputPrice) / 1_000_000
}

func (rr *RunResults) AddStat(entry RunLogEntry) {
	rr.statsMu.Lock()
	defer rr.statsMu.Unlock()
	rr.Stats = append(rr.Stats, entry)
}

func (rr *RunResults) AddFindings(findings []Finding) {
	rr.findingsMu.Lock()
	defer rr.findingsMu.Unlock()
	rr.AllFindings = append(rr.AllFindings, findings...)
}

func (rr *RunResults) AddPostRunOutput(output string) {
	rr.postRunOutputsMu.Lock()
	defer rr.postRunOutputsMu.Unlock()
	rr.PostRunOutputs = append(rr.PostRunOutputs, output)
}

func (rr *RunResults) AddPreRunAnalysis(file string, analysis string) {
	rr.preRunAnalysesMu.Lock()
	defer rr.preRunAnalysesMu.Unlock()
	rr.PreRunAnalyses[file] = append(rr.PreRunAnalyses[file], analysis)
}

func (rr *RunResults) Finish() {
	rr.TotalElapsed = time.Since(rr.StartTime)
}

func (rr *RunResults) SetDiffStats(ctx *PRContext) {
	for _, f := range ctx.Files {
		lines := strings.Split(f.Diff, "\n")
		for _, line := range lines {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) < 2 {
				continue
			}
			diffLine := parts[1]
			if strings.HasPrefix(diffLine, "+") && !strings.HasPrefix(diffLine, "+++ ") {
				rr.LinesAdded++
			} else if strings.HasPrefix(diffLine, "-") && !strings.HasPrefix(diffLine, "--- ") {
				rr.LinesRemoved++
			}
		}
	}
	rr.LinesChanged = rr.LinesAdded + rr.LinesRemoved
}

func (rr *RunResults) GetStatsString() string {
	issueCounts := make(map[string]int)
	for _, f := range rr.AllFindings {
		level := strings.ToLower(f.SeverityHint)
		if level == "" {
			level = "unknown"
		}
		issueCounts[level]++
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("lines_added=%d\n", rr.LinesAdded))
	sb.WriteString(fmt.Sprintf("lines_removed=%d\n", rr.LinesRemoved))
	sb.WriteString(fmt.Sprintf("lines_changed=%d\n", rr.LinesChanged))

	// All different issue levels
	for level, count := range issueCounts {
		sb.WriteString(fmt.Sprintf("issues_%s=%d\n", level, count))
	}

	// Token/cost summaries
	totalTokensIn := 0
	totalTokensOut := 0
	totalTokensReasoning := 0
	totalCost := 0.0

	for _, entry := range rr.Stats {
		totalTokensIn += entry.TokensIn
		totalTokensOut += entry.TokensOut
		totalTokensReasoning += entry.TokensReasoning
		totalCost += entry.Cost()
	}

	sb.WriteString(fmt.Sprintf("tokens_in=%d\n", totalTokensIn))
	sb.WriteString(fmt.Sprintf("tokens_out=%d\n", totalTokensOut))
	if totalTokensReasoning > 0 {
		sb.WriteString(fmt.Sprintf("tokens_reasoning=%d\n", totalTokensReasoning))
	}
	sb.WriteString(fmt.Sprintf("total_cost=%.6f\n", totalCost))

	return sb.String()
}

type OutputHandler struct {
	RunDir string
	LogDir string
}

func NewOutputHandler(runDir, logDir string) *OutputHandler {
	return &OutputHandler{
		RunDir: runDir,
		LogDir: logDir,
	}
}

func (h *OutputHandler) SaveRunFile(relPath, content string) {
	path := filepath.Join(h.RunDir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		fmt.Printf("Warning: could not create directory for %s: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		fmt.Printf("Warning: could not save to %s: %v\n", path, err)
	}
}

func (h *OutputHandler) LogRun(entry RunLogEntry) {
	_ = os.MkdirAll(h.LogDir, 0755)
	f, err := os.OpenFile(filepath.Join(h.LogDir, "run-log.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	data, _ := json.Marshal(entry)
	f.Write(append(data, '\n'))
}

func (h *OutputHandler) MarkPersona(id string) string {
	return "@persona{" + id + "}"
}

func (h *OutputHandler) LinkPersonas(s string) string {
	re := regexp.MustCompile(`@persona\{([^}]+)\}`)
	return re.ReplaceAllString(s, "[$0]($1/raw.md)")
}

func (h *OutputHandler) Highlight(s string) string {
	re := regexp.MustCompile(`@persona\{([^}]+)\}`)
	return re.ReplaceAllString(s, "\033[32m$1\033[0m")
}

func (h *OutputHandler) StripMarkers(s string) string {
	re := regexp.MustCompile(`@persona\{([^}]+)\}`)
	return re.ReplaceAllString(s, "$1")
}

func (h *OutputHandler) Printf(format string, a ...any) {
	fmt.Printf(format, a...)
}

func (h *OutputHandler) Println(a ...any) {
	fmt.Println(a...)
}

type RunSettings struct {
	Command               string
	Repo                  string
	PRNumber              string
	CommitHash            string
	CompareTo             string
	FilePatterns          []string
	MaxTokens             *int
	Concurrency           int
	ModelProfile          string
	InitialCwd            string
	ExeDir                string
	DryRun                bool
	ContextEval           bool
	ContextEvalCSV        string
	IncludePersonas       []string
	ExcludePersonas       []string
	ExcludePostExplainers bool
	PromptOnly            bool

	// Context Primers
	ContextFormat    string
	PlannedFiles     []string
	PlannedFunctions []string
	PlannedConcepts  []string
}

type RunConfig struct {
	Settings      *RunSettings
	Config        *Config
	Personas      []Persona
	Primers       []Primer
	Waivers       []Waiver
	PRInfo        *PRInfo
	GlobalContext *PRContext
	RunDir        string
	SearchPaths   []string

	PreRunToRun     []PersonaRun
	PreRunToSkip    []PersonaRun
	ReviewersToRun  []PersonaRun
	ReviewersToSkip []PersonaRun
	PostRunToRun    []PersonaRun
	PostRunToSkip   []PersonaRun

	BalancedClient ModelClient
	FastestClient  ModelClient
	OutputHandler  *OutputHandler
	ActiveProfile  string
}

func NewRunSettings() *RunSettings {
	return NewRunSettingsFromArgs(os.Args)
}

func NewRunSettingsFromArgs(args []string) *RunSettings {
	initialCwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Error getting current working directory: %v", err)
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("Error getting executable path: %v", err)
	}
	exeDir := filepath.Dir(exePath)

	s := &RunSettings{
		Concurrency: 5, // Default concurrency
		InitialCwd:  initialCwd,
		ExeDir:      exeDir,
	}

	if len(args) < 2 {
		s.PrintUsage()
		os.Exit(1)
	}

	// Find the first non-flag argument to be the command
	var command string
	var commandIdx int = -1
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if arg == "pr" || arg == "commit" || arg == "file" || arg == "branches" || arg == "context" || arg == "concepts" {
				command = arg
				commandIdx = i
				break
			}
		}
	}

	if command == "" {
		s.PrintUsage()
		os.Exit(0)
	}

	s.Command = command

	// All other arguments are passed to the sub-command parser
	subArgs := append([]string{}, args[1:commandIdx]...)
	subArgs = append(subArgs, args[commandIdx+1:]...)

	switch s.Command {
	case "pr":
		s.parsePRArgs(subArgs)
	case "commit":
		s.parseCommitArgs(subArgs)
	case "file":
		s.parseFileArgs(subArgs)
	case "branches":
		s.parseBranchesArgs(subArgs)
	case "context":
		if len(subArgs) > 0 && subArgs[0] == "concepts" {
			s.Command = "concepts"
			s.parseConceptsArgs(subArgs)
		} else {
			s.parseContextArgs(subArgs)
		}
	case "concepts":
		s.parseConceptsArgs(subArgs)
	default:
		fmt.Printf("Unknown command: %s\n", s.Command)
		s.PrintUsage()
		os.Exit(1)
	}

	return s
}

func NewRunConfig(ctx context.Context, s *RunSettings) (*RunConfig, error) {
	rc := &RunConfig{
		Settings: s,
	}

	// 0. Initialize OutputHandler early
	rc.RunDir = s.RunDir()
	logDir := filepath.Join(s.InitialCwd, ".ai-review", s.Repo)
	rc.OutputHandler = NewOutputHandler(rc.RunDir, logDir)

	// 1. Resolve target info
	var err error
	if s.IsFile() {
		rc.OutputHandler.Printf("--- Ensuring repo %s is available...\n", s.Repo)
		if err := EnsureRepo(s.Repo); err != nil {
			return nil, fmt.Errorf("error ensuring repo: %w", err)
		}

		if err := FetchRefs(s.Repo, "", s.CommitHash); err != nil { // commitHash is used as branch here
			return nil, fmt.Errorf("error fetching branch: %w", err)
		}

		rc.PRInfo, err = GetFileInfo(s.Repo, s.CommitHash, s.FilePatterns)
		if err != nil {
			return nil, fmt.Errorf("error getting file info: %w", err)
		}
	} else if s.IsCommit() {
		rc.OutputHandler.Printf("--- Ensuring repo %s is available...\n", s.Repo)
		if err := EnsureRepo(s.Repo); err != nil {
			return nil, fmt.Errorf("error ensuring repo: %w", err)
		}

		rc.OutputHandler.Printf("--- Fetching commit %s...\n", s.CommitHash)
		if err := FetchCommit(s.Repo, s.CommitHash); err != nil {
			return nil, fmt.Errorf("error fetching commit: %w", err)
		}

		if s.CompareTo != "" {
			rc.OutputHandler.Printf("--- Fetching comparison commit %s...\n", s.CompareTo)
			if err := FetchCommit(s.Repo, s.CompareTo); err != nil {
				return nil, fmt.Errorf("error fetching comparison commit: %w", err)
			}
		}

		rc.OutputHandler.Printf("--- Fetching commit info for %s...\n", s.CommitHash)
		rc.PRInfo, err = GetCommitInfo(s.CommitHash, s.CompareTo)
		if err != nil {
			return nil, fmt.Errorf("error getting commit info: %w", err)
		}
	} else if s.IsBranches() {
		rc.OutputHandler.Printf("--- Ensuring repo %s is available...\n", s.Repo)
		if err := EnsureRepo(s.Repo); err != nil {
			return nil, fmt.Errorf("error ensuring repo: %w", err)
		}

		rc.OutputHandler.Printf("--- Fetching branches %s and %s...\n", s.CompareTo, s.CommitHash)
		if err := FetchRefs(s.Repo, "", s.CompareTo); err != nil {
			rc.OutputHandler.Printf("    Warning: error fetching %s: %v\n", s.CompareTo, err)
		}
		if err := FetchRefs(s.Repo, "", s.CommitHash); err != nil {
			rc.OutputHandler.Printf("    Warning: error fetching %s: %v\n", s.CommitHash, err)
		}

		rc.PRInfo, err = GetBranchesInfo(s.Repo, s.CompareTo, s.CommitHash)
		if err != nil {
			return nil, fmt.Errorf("error getting branches info: %w", err)
		}
	} else {
		rc.OutputHandler.Printf("--- Fetching PR info for %s #%s...\n", s.Repo, s.PRNumber)
		rc.PRInfo, err = GetPRInfo(s.Repo, s.PRNumber)
		if err != nil {
			return nil, fmt.Errorf("error getting PR info: %w", err)
		}

		rc.OutputHandler.Printf("--- Ensuring local repository for %s...\n", s.Repo)
		if err := EnsureRepo(s.Repo); err != nil {
			return nil, fmt.Errorf("error ensuring repo: %w", err)
		}

		rc.OutputHandler.Printf("--- Fetching git refs (base: %s)...\n", rc.PRInfo.BaseRefName)
		if err := FetchRefs(s.Repo, s.PRNumber, rc.PRInfo.BaseRefName); err != nil {
			return nil, fmt.Errorf("error fetching refs: %w", err)
		}
	}

	// 2. Resolve search paths
	rc.SearchPaths = []string{}
	addPath := func(path string) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return
		}
		for _, p := range rc.SearchPaths {
			if p == abs {
				return
			}
		}
		rc.SearchPaths = append(rc.SearchPaths, abs)
	}
	addPath(s.ExeDir)
	addPath(s.InitialCwd)
	if cwd, err := os.Getwd(); err == nil {
		addPath(cwd)
	}

	// 3. Load config and personas
	rc.OutputHandler.Println("--- Loading configuration and personas...")
	rc.Config, err = LoadConfig(rc.SearchPaths, s.Repo, rc.OutputHandler)
	if err != nil {
		return nil, fmt.Errorf("error loading config: %w. Make sure .ai-review/%s/config.yaml exists in one of %v", err, s.Repo, rc.SearchPaths)
	}

	// Determine the active profile early so we can print it
	rc.ActiveProfile = s.ModelProfile
	if rc.ActiveProfile == "" {
		rc.ActiveProfile = rc.Config.DefaultProfile
	}
	if rc.ActiveProfile == "" {
		// Fallback to "default" if nothing specified
		rc.ActiveProfile = "default"
	}

	profile, ok := rc.Config.ModelProfiles[rc.ActiveProfile]
	if !ok {
		// If requested profile not found, and it's not "default", try "default"
		if rc.ActiveProfile != "default" {
			rc.OutputHandler.Printf("    Warning: profile '%s' not found, falling back to 'default'\n", rc.ActiveProfile)
			rc.ActiveProfile = "default"
			profile, ok = rc.Config.ModelProfiles[rc.ActiveProfile]
		}
	}

	if !ok {
		// If still not found, just take the first available profile if any
		if len(rc.Config.ModelProfiles) > 0 {
			for name, p := range rc.Config.ModelProfiles {
				rc.OutputHandler.Printf("    Warning: profile not found, falling back to '%s'\n", name)
				rc.ActiveProfile = name
				profile = p
				ok = true
				break
			}
		}
	}

	if !ok {
		return nil, fmt.Errorf("no model profiles found in config.yaml")
	}

	rc.OutputHandler.Printf("--- Using model profile: %s\n", rc.ActiveProfile)

	rc.Personas, err = LoadPersonas(rc.SearchPaths, s.Repo, rc.PRInfo.HeadRefOid, rc.OutputHandler)
	if err != nil {
		return nil, fmt.Errorf("error loading personas: %w. Make sure .ai-review/%s/personas/*.md exist in one of %v", err, s.Repo, rc.SearchPaths)
	}

	rc.Primers, err = LoadPrimers(rc.SearchPaths, s.Repo, rc.PRInfo.HeadRefOid, rc.OutputHandler)
	if err != nil {
		return nil, fmt.Errorf("error loading primers: %w", err)
	}

	rc.Waivers, err = LoadWaivers(rc.SearchPaths, s.Repo, rc.PRInfo.HeadRefOid, rc.OutputHandler)
	if err != nil {
		rc.OutputHandler.Printf("    Warning: error loading waivers: %v\n", err)
	}

	// 4. Extract context
	rc.OutputHandler.Println("--- Extracting PR context...")
	rc.GlobalContext, err = GetPRContext(rc.PRInfo, &FilterSet{
		IncludeFilters: s.FilePatterns,
		GlobalExcludes: rc.Config.GlobalExcludes,
	})
	if err != nil {
		return nil, fmt.Errorf("error getting context: %w", err)
	}

	// 5. Create run directory
	if !s.DryRun {
		if err := os.MkdirAll(rc.RunDir, 0755); err != nil {
			return nil, fmt.Errorf("error creating run directory: %w", err)
		}
		rc.OutputHandler.Printf("--- Run directory: %s\n", rc.RunDir)
	}

	// 6. Filter personas
	rc.filterPersonas()

	// 7. Initialize common clients
	if s.DryRun {
		return rc, nil
	}

	balancedCfg, err := rc.getAggregationModelConfig()
	if err != nil {
		return nil, err
	}
	rc.BalancedClient, err = GetModelClient(ctx, balancedCfg.Provider, balancedCfg.Model, balancedCfg.ReasoningLevel)
	if err != nil {
		return nil, fmt.Errorf("error creating balanced client: %w", err)
	}

	fastestCfg, ok := profile[string(FastestGood)]
	if !ok {
		fastestCfg = balancedCfg
	}
	rc.FastestClient, err = GetModelClient(ctx, fastestCfg.Provider, fastestCfg.Model, fastestCfg.ReasoningLevel)
	if err != nil {
		rc.FastestClient = rc.BalancedClient
	}

	return rc, nil
}

func (rc *RunConfig) getAggregationModelConfig() (ModelConfig, error) {
	profile := rc.Config.ModelProfiles[rc.ActiveProfile]
	if cfg, ok := profile[string(Balanced)]; ok {
		return cfg, nil
	}
	if cfg, ok := profile[string(BestCode)]; ok {
		rc.OutputHandler.Println("    Warning: 'balanced' model mapping not found; falling back to 'best_code' for aggregation")
		return cfg, nil
	}
	return ModelConfig{}, fmt.Errorf("'balanced' model mapping not found in profile '%s'", rc.ActiveProfile)
}

func (rc *RunConfig) getModelInfo(p Persona) string {
	profile, ok := rc.Config.ModelProfiles[rc.ActiveProfile]
	if !ok {
		return "unknown profile"
	}
	modelCfg, ok := profile[p.ModelCategory]
	if !ok {
		return fmt.Sprintf("no mapping for %s", p.ModelCategory)
	}
	res := modelCfg.Model
	if res == "" {
		res = modelCfg.ID
	}
	if modelCfg.ReasoningLevel != "" && modelCfg.ReasoningLevel != "none" {
		res = fmt.Sprintf("%s (%s)", res, modelCfg.ReasoningLevel)
	}
	return res
}

func (rc *RunConfig) filterPersonas() {
	rc.OutputHandler.Println("--- Filtering personas...")
	for _, p := range rc.Personas {
		// CLI: Include filter
		if len(rc.Settings.IncludePersonas) > 0 {
			found := false
			for _, id := range rc.Settings.IncludePersonas {
				if id == p.ID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		fs := p.Filters
		fs.GlobalExcludes = rc.Config.GlobalExcludes
		if len(fs.IncludeFilters) == 0 && rc.PRInfo.BaseRefOid == rc.PRInfo.HeadRefOid && !rc.PRInfo.IsCommit {
			fs.IncludeFilters = rc.PRInfo.FilePatterns
		}

		var personaContext *PRContext
		if len(fs.IncludeFilters) > 0 || len(fs.ExcludeFilters) > 0 || len(fs.RawRegexFilters) > 0 || (rc.PRInfo.BaseRefOid == rc.PRInfo.HeadRefOid && !rc.PRInfo.IsCommit && rc.PRInfo.BaseRefOid != "") ||
			len(fs.BranchFilters) > 0 || len(fs.FunctionFilters) > 0 || fs.DateFilter != "" {
			var err error
			personaContext, err = GetPRContext(rc.PRInfo, &fs)
			if err != nil {
				rc.OutputHandler.Printf("    Warning: error filtering context for persona %s: %v\n", p.ColoredID, err)
				continue
			}
		} else {
			personaContext = rc.GlobalContext
		}

		run := PersonaRun{Persona: p, Context: personaContext}
		skip := true
		for _, f := range personaContext.Files {
			// Pre-compile regexes for efficiency
			if err := fs.Compile(); err != nil {
				rc.OutputHandler.Printf("    Warning: error compiling filters for persona %s: %v\n", p.ColoredID, err)
				break
			}
			if f.Matches(FileMatchOptions{
				FilterSet:  &fs,
				Branch:     personaContext.Branch,
				CommitDate: personaContext.CommitDate,
			}) {
				skip = false
				break
			}
		}

		// CLI: Exclude filters
		if !skip {
			for _, id := range rc.Settings.ExcludePersonas {
				if id == p.ID {
					skip = true
					break
				}
			}
			if !skip && rc.Settings.ExcludePostExplainers && p.Role == "explainer" && p.Stage == "post" {
				skip = true
			}
		}

		if p.Role == "explainer" {
			if p.Stage == "pre" {
				if skip {
					rc.PreRunToSkip = append(rc.PreRunToSkip, run)
				} else {
					rc.PreRunToRun = append(rc.PreRunToRun, run)
				}
			} else {
				if skip {
					rc.PostRunToSkip = append(rc.PostRunToSkip, run)
				} else {
					rc.PostRunToRun = append(rc.PostRunToRun, run)
				}
			}
		} else {
			if skip {
				rc.ReviewersToSkip = append(rc.ReviewersToSkip, run)
			} else {
				rc.ReviewersToRun = append(rc.ReviewersToRun, run)
			}
		}
	}

	rc.OutputHandler.Println("    To be run:")
	for _, r := range rc.PreRunToRun {
		rc.OutputHandler.Printf("      - %s (explainer, pre) [%s]\n", r.Persona.ColoredID, rc.getModelInfo(r.Persona))
		rc.printMatchedPrimers(r.Context)
	}
	for _, r := range rc.ReviewersToRun {
		rc.OutputHandler.Printf("      - %s (reviewer) [%s]\n", r.Persona.ColoredID, rc.getModelInfo(r.Persona))
		rc.printMatchedPrimers(r.Context)
	}
	for _, r := range rc.PostRunToRun {
		rc.OutputHandler.Printf("      - %s (explainer, post) [%s]\n", r.Persona.ColoredID, rc.getModelInfo(r.Persona))
		rc.printMatchedPrimers(r.Context)
	}

	if len(rc.PreRunToSkip) > 0 || len(rc.ReviewersToSkip) > 0 || len(rc.PostRunToSkip) > 0 {
		rc.OutputHandler.Println("    To be skipped:")
		for _, r := range rc.PreRunToSkip {
			rc.OutputHandler.Printf("      - %s [%s]\n", r.Persona.ColoredID, rc.getModelInfo(r.Persona))
		}
		for _, r := range rc.ReviewersToSkip {
			rc.OutputHandler.Printf("      - %s [%s]\n", r.Persona.ColoredID, rc.getModelInfo(r.Persona))
		}
		for _, r := range rc.PostRunToSkip {
			rc.OutputHandler.Printf("      - %s [%s]\n", r.Persona.ColoredID, rc.getModelInfo(r.Persona))
		}
	}
}

func (rc *RunConfig) printMatchedPrimers(personaContext *PRContext) {
	matches := rc.FindMatches(personaContext)
	for _, m := range matches {
		rc.OutputHandler.Printf("        with primer: %s (matches %d files)\n", m.Primer.ID, len(m.Files))
	}
}

func (s *RunSettings) parsePRArgs(args []string) {
	fs := flag.NewFlagSet("pr", flag.ExitOnError)
	maxTokens := fs.Int("max-tokens", -1, "Override max tokens for AI response")
	concurrency := fs.Int("concurrency", s.Concurrency, "Max concurrent personas")
	modelProfile := fs.String("model-profile", s.ModelProfile, "Model profile to use from config.yaml")
	dryRun := fs.Bool("dry-run", false, "Scan and report what will be run, but don't execute")
	contextEval := fs.Bool("context-eval", false, "Prepare context for each persona and report sizes, but don't run personas")
	contextEvalCSV := fs.String("context-eval-csv", "", "Output a detailed CSV file of the context evaluation")
	includePersonas := fs.String("include-personas", "", "Only run these personas (comma-separated IDs)")
	excludePersonas := fs.String("exclude-personas", "", "Exclude these personas (comma-separated IDs)")
	excludePostExplainers := fs.Bool("exclude-post-explainers", false, "Exclude all post-run explainers")
	promptOnly := fs.Bool("prompt-only", false, "Run pre-explainers, generate prompts for others, then stop")

	remaining, _ := parseInterspersed(fs, args)

	if *maxTokens != -1 {
		s.MaxTokens = maxTokens
	}
	s.Concurrency = *concurrency
	s.ModelProfile = *modelProfile
	s.DryRun = *dryRun
	s.ContextEval = *contextEval
	s.ContextEvalCSV = *contextEvalCSV
	s.IncludePersonas = s.parseCommaList(*includePersonas)
	s.ExcludePersonas = s.parseCommaList(*excludePersonas)
	s.ExcludePostExplainers = *excludePostExplainers
	s.PromptOnly = *promptOnly

	if len(remaining) < 2 {
		s.PrintUsage()
		os.Exit(1)
	}
	s.Repo = remaining[0]
	s.PRNumber = remaining[1]
}

func (s *RunSettings) parseCommitArgs(args []string) {
	fs := flag.NewFlagSet("commit", flag.ExitOnError)
	maxTokens := fs.Int("max-tokens", -1, "Override max tokens for AI response")
	concurrency := fs.Int("concurrency", s.Concurrency, "Max concurrent personas")
	modelProfile := fs.String("model-profile", s.ModelProfile, "Model profile to use from config.yaml")
	compareTo := fs.String("compare-to", "", "Specific commit to compare to (default: parent)")
	dryRun := fs.Bool("dry-run", false, "Scan and report what will be run, but don't execute")
	contextEval := fs.Bool("context-eval", false, "Prepare context for each persona and report sizes, but don't run personas")
	contextEvalCSV := fs.String("context-eval-csv", "", "Output a detailed CSV file of the context evaluation")
	includePersonas := fs.String("include-personas", "", "Only run these personas (comma-separated IDs)")
	excludePersonas := fs.String("exclude-personas", "", "Exclude these personas (comma-separated IDs)")
	excludePostExplainers := fs.Bool("exclude-post-explainers", false, "Exclude all post-run explainers")
	promptOnly := fs.Bool("prompt-only", false, "Run pre-explainers, generate prompts for others, then stop")

	remaining, _ := parseInterspersed(fs, args)

	if *maxTokens != -1 {
		s.MaxTokens = maxTokens
	}
	s.Concurrency = *concurrency
	s.ModelProfile = *modelProfile
	s.CompareTo = *compareTo
	s.DryRun = *dryRun
	s.ContextEval = *contextEval
	s.ContextEvalCSV = *contextEvalCSV
	s.IncludePersonas = s.parseCommaList(*includePersonas)
	s.ExcludePersonas = s.parseCommaList(*excludePersonas)
	s.ExcludePostExplainers = *excludePostExplainers
	s.PromptOnly = *promptOnly

	if len(remaining) < 2 {
		s.PrintUsage()
		os.Exit(1)
	}
	s.Repo = remaining[0]
	s.CommitHash = remaining[1]
}

func (s *RunSettings) parseFileArgs(args []string) {
	fs := flag.NewFlagSet("file", flag.ExitOnError)
	maxTokens := fs.Int("max-tokens", -1, "Override max tokens for AI response")
	concurrency := fs.Int("concurrency", s.Concurrency, "Max concurrent personas")
	modelProfile := fs.String("model-profile", s.ModelProfile, "Model profile to use from config.yaml")
	dryRun := fs.Bool("dry-run", false, "Scan and report what will be run, but don't execute")
	contextEval := fs.Bool("context-eval", false, "Prepare context for each persona and report sizes, but don't run personas")
	contextEvalCSV := fs.String("context-eval-csv", "", "Output a detailed CSV file of the context evaluation")
	includePersonas := fs.String("include-personas", "", "Only run these personas (comma-separated IDs)")
	excludePersonas := fs.String("exclude-personas", "", "Exclude these personas (comma-separated IDs)")
	excludePostExplainers := fs.Bool("exclude-post-explainers", false, "Exclude all post-run explainers")
	promptOnly := fs.Bool("prompt-only", false, "Run pre-explainers, generate prompts for others, then stop")

	remaining, _ := parseInterspersed(fs, args)

	if *maxTokens != -1 {
		s.MaxTokens = maxTokens
	}
	s.Concurrency = *concurrency
	s.ModelProfile = *modelProfile
	s.DryRun = *dryRun
	s.ContextEval = *contextEval
	s.ContextEvalCSV = *contextEvalCSV
	s.IncludePersonas = s.parseCommaList(*includePersonas)
	s.ExcludePersonas = s.parseCommaList(*excludePersonas)
	s.ExcludePostExplainers = *excludePostExplainers
	s.PromptOnly = *promptOnly

	if len(remaining) < 3 {
		s.PrintUsage()
		os.Exit(1)
	}
	s.Repo = remaining[0]
	s.CommitHash = remaining[1] // branch
	s.FilePatterns = remaining[2:]
}

func (s *RunSettings) parseBranchesArgs(args []string) {
	fs := flag.NewFlagSet("branches", flag.ExitOnError)
	maxTokens := fs.Int("max-tokens", -1, "Override max tokens for AI response")
	concurrency := fs.Int("concurrency", s.Concurrency, "Max concurrent personas")
	modelProfile := fs.String("model-profile", s.ModelProfile, "Model profile to use from config.yaml")
	dryRun := fs.Bool("dry-run", false, "Scan and report what will be run, but don't execute")
	contextEval := fs.Bool("context-eval", false, "Prepare context for each persona and report sizes, but don't run personas")
	contextEvalCSV := fs.String("context-eval-csv", "", "Output a detailed CSV file of the context evaluation")
	includePersonas := fs.String("include-personas", "", "Only run these personas (comma-separated IDs)")
	excludePersonas := fs.String("exclude-personas", "", "Exclude these personas (comma-separated IDs)")
	excludePostExplainers := fs.Bool("exclude-post-explainers", false, "Exclude all post-run explainers")
	promptOnly := fs.Bool("prompt-only", false, "Run pre-explainers, generate prompts for others, then stop")

	remaining, _ := parseInterspersed(fs, args)

	if *maxTokens != -1 {
		s.MaxTokens = maxTokens
	}
	s.Concurrency = *concurrency
	s.ModelProfile = *modelProfile
	s.DryRun = *dryRun
	s.ContextEval = *contextEval
	s.ContextEvalCSV = *contextEvalCSV
	s.IncludePersonas = s.parseCommaList(*includePersonas)
	s.ExcludePersonas = s.parseCommaList(*excludePersonas)
	s.ExcludePostExplainers = *excludePostExplainers
	s.PromptOnly = *promptOnly

	if len(remaining) < 3 {
		s.PrintUsage()
		os.Exit(1)
	}
	s.Repo = remaining[0]
	s.CompareTo = remaining[1]  // base
	s.CommitHash = remaining[2] // head
}

func (s *RunSettings) parseConceptsArgs(args []string) {
	commandName := "concepts"
	startIdx := 0
	if len(args) > 0 && args[0] == "concepts" {
		startIdx = 1
		commandName = "context concepts"
	}

	fs := flag.NewFlagSet(commandName, flag.ExitOnError)
	files := fs.String("files", "", "Comma-separated list of planned target files")
	functions := fs.String("functions", "", "Comma-separated list of planned target functions")
	format := fs.String("format", "markdown", "Output format (json, markdown, names)")

	remaining, _ := parseInterspersed(fs, args[startIdx:])

	if len(remaining) < 1 {
		fmt.Println("Error: Missing repo identifier")
		s.PrintUsage()
		os.Exit(1)
	}

	s.Repo = remaining[0]
	s.PlannedFiles = s.parseCommaList(*files)
	s.PlannedFunctions = s.parseCommaList(*functions)
	s.ContextFormat = strings.ToLower(*format)
}

func (s *RunSettings) parseContextArgs(args []string) {
	if len(args) < 1 || args[0] != "primers" {
		s.PrintUsage()
		os.Exit(1)
	}

	fs := flag.NewFlagSet("context primers", flag.ExitOnError)
	files := fs.String("files", "", "Comma-separated list of planned target files")
	functions := fs.String("functions", "", "Comma-separated list of planned target functions")
	concepts := fs.String("concepts", "", "Comma-separated list of planned target concepts")
	format := fs.String("format", "markdown", "Output format (json, markdown)")

	remaining, _ := parseInterspersed(fs, args[1:])

	if len(remaining) < 1 {
		fmt.Println("Error: Missing repo identifier")
		s.PrintUsage()
		os.Exit(1)
	}

	s.Repo = remaining[0]
	s.PlannedFiles = s.parseCommaList(*files)
	s.PlannedFunctions = s.parseCommaList(*functions)
	s.PlannedConcepts = s.parseCommaList(*concepts)
	s.ContextFormat = strings.ToLower(*format)
}

func (s *RunSettings) parseCommaList(list string) []string {
	if list == "" {
		return nil
	}
	parts := strings.Split(list, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func (s *RunSettings) PrintUsage() {
	fmt.Println("Usage:")
	fmt.Println("  ai-reviewer pr <repo> <pr-number> [options]")
	fmt.Println("  ai-reviewer commit <repo> <commit-hash> [--compare-to <hash>] [options]")
	fmt.Println("  ai-reviewer file <repo> <branch> <file1> <file2> ... [options]")
	fmt.Println("  ai-reviewer branches <repo> <base> <head> [options]")
	fmt.Println("  ai-reviewer context primers <repo> [--files <f>] [--functions <fn>] [--concepts <c>] [--format <fmt>]")
	fmt.Println("  ai-reviewer concepts <repo> [--files <f>] [--functions <fn>] [--format <fmt>]")
	fmt.Println("")
	fmt.Println("Options:")
	fmt.Println("  --model-profile <name>       Model profile to use from config.yaml")
	fmt.Println("  --max-tokens <n>             Override max tokens for AI response")
	fmt.Println("  --concurrency <n>            Max concurrent personas")
	fmt.Println("  --dry-run                    Scan and report what will be run, but don't execute")
	fmt.Println("  --context-eval               Prepare context for each persona and report sizes, but don't run personas")
	fmt.Println("  --context-eval-csv <file>    Output a detailed CSV file of the context evaluation")
	fmt.Println("  --include-personas <ids>     Only run these personas (comma-separated IDs)")
	fmt.Println("  --exclude-personas <ids>     Exclude these personas (comma-separated IDs)")
	fmt.Println("  --exclude-post-explainers    Exclude all post-run explainers")
	fmt.Println("  --prompt-only                Run pre-explainers, generate prompts for others, then stop")
}

func (s *RunSettings) TargetID() string {
	switch s.Command {
	case "pr":
		return s.PRNumber
	case "commit":
		return s.CommitHash
	case "file":
		return "file-" + sanitizePath(s.CommitHash) // branch name
	case "branches":
		return "branches-" + sanitizePath(s.CompareTo) + "-" + sanitizePath(s.CommitHash) // base and head branches
	default:
		return ""
	}
}

func sanitizePath(s string) string {
	// Replace unsafe characters with '-'
	// Unsafe for filenames: / \ : * ? " < > |
	// Also replace other potential issues just in case
	re := regexp.MustCompile(`[\\/:*?"<>|]`)
	return re.ReplaceAllString(s, "-")
}

func (s *RunSettings) IsPR() bool {
	return s.Command == "pr"
}

func (s *RunSettings) IsCommit() bool {
	return s.Command == "commit"
}

func (s *RunSettings) IsFile() bool {
	return s.Command == "file"
}

func (s *RunSettings) IsBranches() bool {
	return s.Command == "branches"
}

func (s *RunSettings) IsContext() bool {
	return s.Command == "context" || s.Command == "concepts"
}

func (s *RunSettings) RunDir() string {
	runID := time.Now().Format("2006-01-02_15-04-05")
	return filepath.Join(s.InitialCwd, ".ai-review", s.Repo, "runs", s.TargetID(), runID)
}

func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		remaining := fs.Args()
		if len(remaining) > 0 {
			positionals = append(positionals, remaining[0])
			args = remaining[1:]
		} else {
			args = nil
		}
	}
	return positionals, nil
}

func printCurrentDir() {
	cwd, err := os.Getwd()
	if err != nil {
		log.Printf("Warning: Could not get current working directory: %v", err)
	} else {
		absPath, _ := filepath.Abs(cwd)
		fmt.Printf("Current working directory: %s\n", absPath)
	}
}
