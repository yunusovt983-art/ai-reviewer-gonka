// Package codebase is the pure domain model of the "reviewable world":
// the diff/file representation and the declarative FilterSet predicate.
// It has NO infrastructure dependencies (no git/exec, no LLM) — only stdlib
// and pathspec. Git acquisition lives in the infra/app layer and depends on
// this package, never the other way around.
package codebase

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/karagenc/go-pathspec"
)

type LineRange struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

type PRInfo struct {
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	BaseRefName  string    `json:"baseRefName"`
	BaseRefOid   string    `json:"baseRefOid"`
	HeadRefName  string    `json:"headRefName"`
	HeadRefOid   string    `json:"headRefOid"`
	IsCommit     bool      `json:"isCommit"`
	CommitDate   time.Time `json:"commitDate"`
	FilePatterns []string  `json:"filePatterns"`
}

type PRContext struct {
	Title       string
	Description string
	Files       []FileContext
	Branch      string
	CommitDate  time.Time
}

type FileContext struct {
	Filename     string
	Diff         string   // Annotated diff for this file
	ChangedLines []string // Content of both added and removed lines
	Functions    []string
}

type FilterSet struct {
	IncludeFilters    []string         `yaml:"path_filters"`
	ExcludeFilters    []string         `yaml:"exclude_filters"`
	GlobalExcludes    []string         `yaml:"-"` // Passed from Config
	RegexFilters      []*regexp.Regexp `yaml:"-"`
	RawRegexFilters   []string         `yaml:"regex_filters"`
	BranchFilters     []string         `yaml:"branch_filters"`
	FunctionFilters   []string         `yaml:"function_filters"`
	LineNumberFilters []LineRange      `yaml:"line_numbers_filter"`
	DateFilter        string           `yaml:"date_filter"`
	IssueRegexes      []string         `yaml:"issue_regexes"`
	IssueRegexObjects []*regexp.Regexp `yaml:"-"`

	Any []FilterSet `yaml:"any,omitempty"`
	All []FilterSet `yaml:"all,omitempty"`
}

type MatchOptions struct {
	Filename           string
	Branch             string
	Functions          []string
	CommitDate         time.Time
	ChangedLineNumbers []int
	ChangedLines       []string
	FindingSummary     string
	FindingDetails     string
}

func (fs *FilterSet) IsEmpty() bool {
	return len(fs.IncludeFilters) == 0 &&
		len(fs.ExcludeFilters) == 0 &&
		len(fs.RawRegexFilters) == 0 &&
		len(fs.BranchFilters) == 0 &&
		len(fs.FunctionFilters) == 0 &&
		len(fs.LineNumberFilters) == 0 &&
		fs.DateFilter == "" &&
		len(fs.IssueRegexes) == 0 &&
		len(fs.Any) == 0 &&
		len(fs.All) == 0
}

func (fs *FilterSet) Matches(opts MatchOptions) bool {
	if len(fs.Any) > 0 {
		for _, sub := range fs.Any {
			if sub.Matches(opts) {
				return true
			}
		}
		return false
	}

	if len(fs.All) > 0 {
		for _, sub := range fs.All {
			if !sub.Matches(opts) {
				return false
			}
		}
		return true
	}

	if !fs.MatchesPath(opts.Filename) {
		return false
	}

	if len(fs.BranchFilters) > 0 {
		if !PathIncluded(opts.Branch, fs.BranchFilters, true) {
			return false
		}
	}

	if len(fs.FunctionFilters) > 0 {
		matched := false
	loop:
		for _, ff := range fs.FunctionFilters {
			for _, fn := range opts.Functions {
				if fn == ff {
					matched = true
					break loop
				}
			}
		}
		if !matched {
			return false
		}
	}

	if fs.DateFilter != "" && !opts.CommitDate.IsZero() {
		cutoff, err := time.Parse("2006-01-02", fs.DateFilter)
		if err == nil {
			if !opts.CommitDate.Before(cutoff) {
				return false
			}
		}
	}

	if len(fs.LineNumberFilters) > 0 {
		matched := false
		for _, line := range opts.ChangedLineNumbers {
			for _, r := range fs.LineNumberFilters {
				if line >= r.Start && line <= r.End {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(fs.IssueRegexObjects) > 0 {
		matched := false
		for _, re := range fs.IssueRegexObjects {
			if re.MatchString(opts.FindingSummary) || re.MatchString(opts.FindingDetails) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(fs.RegexFilters) == 0 {
		return true
	}
	for _, line := range opts.ChangedLines {
		for _, re := range fs.RegexFilters {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}

type FileMatchOptions struct {
	FilterSet      *FilterSet
	Branch         string
	CommitDate     time.Time
	FindingSummary string
	FindingDetails string
}

func (f FileContext) Matches(opts FileMatchOptions) bool {
	if opts.FilterSet == nil {
		return true
	}
	return opts.FilterSet.Matches(MatchOptions{
		Filename:           f.Filename,
		Branch:             opts.Branch,
		Functions:          f.Functions,
		CommitDate:         opts.CommitDate,
		ChangedLineNumbers: f.ChangedLineNumbers(),
		ChangedLines:       f.ChangedLines,
		FindingSummary:     opts.FindingSummary,
		FindingDetails:     opts.FindingDetails,
	})
}

func (f FileContext) HasChangedLinesInRanges(ranges []LineRange) bool {
	if len(ranges) == 0 {
		return true
	}

	for _, line := range f.ChangedLineNumbers() {
		for _, r := range ranges {
			if line >= r.Start && line <= r.End {
				return true
			}
		}
	}

	return false
}

func (f FileContext) ChangedLineNumbers() []int {
	seen := make(map[int]struct{})
	var lines []int

	for _, line := range strings.Split(f.Diff, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}

		lineNo, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}

		diffLine := parts[1]
		if !strings.HasPrefix(diffLine, "+") && !strings.HasPrefix(diffLine, "-") {
			continue
		}
		if strings.HasPrefix(diffLine, "+++ ") || strings.HasPrefix(diffLine, "--- ") {
			continue
		}
		if _, ok := seen[lineNo]; ok {
			continue
		}

		seen[lineNo] = struct{}{}
		lines = append(lines, lineNo)
	}

	return lines
}

func (ctx *PRContext) ChangedFiles() []string {
	var files []string
	for _, f := range ctx.Files {
		files = append(files, f.Filename)
	}
	return files
}

func (ctx *PRContext) FullDiff() string {
	var sb strings.Builder
	for _, f := range ctx.Files {
		sb.WriteString(f.Diff)
	}
	return sb.String()
}

func ParseAnnotatedFileDiff(fd string) FileContext {
	lines := strings.Split(fd, "\n")
	var filename string
	var changedLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "+++ b/") {
			filename = strings.TrimPrefix(line, "+++ b/")
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		diffLine := parts[1]

		if (strings.HasPrefix(diffLine, "+") && !strings.HasPrefix(diffLine, "+++ ")) ||
			(strings.HasPrefix(diffLine, "-") && !strings.HasPrefix(diffLine, "--- ")) {
			content := diffLine[1:]
			changedLines = append(changedLines, content)
		}
	}
	return FileContext{Filename: filename, Diff: fd, ChangedLines: changedLines}
}

func (fs *FilterSet) Compile() error {
	for _, r := range fs.RawRegexFilters {
		re, err := regexp.Compile(r)
		if err != nil {
			return fmt.Errorf("invalid regex %s: %w", r, err)
		}
		fs.RegexFilters = append(fs.RegexFilters, re)
	}
	for _, r := range fs.IssueRegexes {
		re, err := regexp.Compile(r)
		if err != nil {
			return fmt.Errorf("invalid issue regex %s: %w", r, err)
		}
		fs.IssueRegexObjects = append(fs.IssueRegexObjects, re)
	}
	for i := range fs.Any {
		if err := fs.Any[i].Compile(); err != nil {
			return err
		}
	}
	for i := range fs.All {
		if err := fs.All[i].Compile(); err != nil {
			return err
		}
	}
	return nil
}

func (fs *FilterSet) MatchesPath(path string) bool {
	if len(fs.Any) > 0 {
		for _, sub := range fs.Any {
			if sub.MatchesPath(path) {
				return true
			}
		}
		return false
	}

	if len(fs.All) > 0 {
		for _, sub := range fs.All {
			if !sub.MatchesPath(path) {
				return false
			}
		}
		return true
	}

	if !PathIncluded(path, fs.IncludeFilters, true) {
		return false
	}

	if len(fs.ExcludeFilters) > 0 && PathIncluded(path, fs.ExcludeFilters, false) {
		return false
	}

	if len(fs.GlobalExcludes) > 0 {
		// Global excludes are applied UNLESS explicitly included
		if PathIncluded(path, fs.GlobalExcludes, false) && !PathIncluded(path, fs.IncludeFilters, false) {
			return false
		}
	}

	return true
}

func PathIncluded(path string, globs []string, defaultOnEmpty bool) bool {
	if len(globs) == 0 {
		return defaultOnEmpty
	}
	// pathspec.Match trims leading ./ from path, but it doesn't trim it from patterns.
	// So we trim it from patterns ourselves.
	cleanGlobs := make([]string, len(globs))
	for i, g := range globs {
		cleanGlobs[i] = strings.TrimPrefix(g, "./")
	}

	spec, err := pathspec.FromLines(cleanGlobs...)
	if err != nil {
		return false
	}
	return spec.Match(path)
}

var hunkHeaderRegexp = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func AnnotateDiff(diff string) (string, []string) {
	var result strings.Builder
	var functions []string
	scanner := bufio.NewScanner(strings.NewReader(diff))
	currentLine := 0
	funcRegex := regexp.MustCompile(`(?:func|function|class|def|method|type)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "@@ ") {
			matches := hunkHeaderRegexp.FindStringSubmatch(line)
			if len(matches) > 1 {
				startLine, _ := strconv.Atoi(matches[1])
				currentLine = startLine
			}
			result.WriteString(line + "\n")
		} else if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			result.WriteString(fmt.Sprintf("%d:%s\n", currentLine, line))
			matches := funcRegex.FindStringSubmatch(line)
			if len(matches) > 1 {
				functions = append(functions, matches[1])
			}
			currentLine++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- ") {
			result.WriteString(fmt.Sprintf("%d:%s\n", currentLine, line))
		} else if strings.HasPrefix(line, " ") {
			result.WriteString(fmt.Sprintf("%d:%s\n", currentLine, line))
			currentLine++
		} else {
			result.WriteString(line + "\n")
		}
	}

	return result.String(), functions
}
