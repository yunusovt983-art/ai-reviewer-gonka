# AI Reviewer

A single-binary Go CLI that reviews a GitHub PR using AI personas.

## The Vision
AI code review is becoming essential, especially as more code is itself AI-generated. `ai-reviewer` is built on the idea that good review is not one prompt, but an orchestration problem: multiple specialized reviewers, project-specific context, and clear policy working together to produce high-signal, cost-conscious feedback.

- Specialized personas instead of one generic reviewer
- Repo-aware primers and waivers instead of ambient global behavior
- Structured findings and auditable artifacts instead of opaque output

See [VISION.md](VISION.md) for the longer rationale and design philosophy.

## Architecture

```
╔══════════════════════════════════════════════════════════════════════════════════╗
║                       ai-reviewer  ·  High-Level Design                          ║
╚══════════════════════════════════════════════════════════════════════════════════╝

  INPUT                        PIPELINE  (7 stages)                    OUTPUT
  ───────────────              ──────────────────────────────────       ──────────────
  ┌─────────────┐   gh CLI     ┌───────────────────────────────┐
  │  GitHub PR  │─────────────▶│  ① Pre-Explainers            │
  │  commit     │              │     role: explainer, pre      │
  │  file diff  │   git diff   │     GenerateJSON → analysis   │
  │  branches   │─────────────▶│     SHA-cached per file       │
  └─────────────┘              └──────────────┬────────────────┘
                                              │ analysis injected ↓
  KNOWLEDGE                    ┌──────────────▼────────────────────────────────────┐
  ─────────                    │  ② Reviewers                  parallel (≤N)      │
  ┌──────────┐                 │  ┌────────────┐ ┌────────────┐ ┌────────────┐     │
  │ Persona  │─── instructions▶│  │  security  │ │    perf    │ │   style    │     │
  └──────────┘                 │  │  persona   │ │  persona   │ │  persona   │     │
  ┌──────────┐                 │  └─────┬──────┘ └─────┬──────┘ └─────┬──────┘     │
  │  Primer  │─── context ────▶│        └──────────────┴──────────────┘            │
  └──────────┘    (if matched) │                       │ raw text output           │
                               └───────────────────────┼───────────────────────────┘
                                                       │
  FILTERING                    ┌─────────────────────  ▼ ──────────────────────────┐
  ─────────                    │  ③ Normalize           (FastestClient)            │
  ┌──────────┐                 │     raw text ──▶ []Finding{file·line·severity}    │
  │FilterSet │─── prune files  └───────────────────────┬────────────────────────  ┘
  │path·date │    per-persona                           │
  │func·regex│                 ┌─────────────────────  ▼ ──────────────────────────┐  ┌───────────┐
  └──────────┘                 │  ④ Waiver Filter       (LLM-judge)               │─▶│  Waived   │
                               │     location filter → LLM confirm → suppress      │  │ Findings  │
  POLICY                       └───────────────────────┬─────────────────────────  ┘  └───────────┘
  ──────                                               │ surviving findings
  ┌──────────┐                 ┌─────────────────────  ▼ ──────────────────────────┐
  │  Waiver  │─── rules ──────▶│  ⑤ Aggregate          (BalancedClient)           │
  └──────────┘                 │     dedup · cluster · assign final severity       │
                               └───────────────────────┬────────────────────────  ┘
                                                       │
                               ┌─────────────────────  ▼ ───────────────────────────┐
                               │  ⑥ Post-Explainers    (role: explainer, post)     │
                               │     findings summary injected if include_findings  │
                               └───────────────────────┬────────────────────────────┘
                                                       │
                               ┌─────────────────────  ▼ ──────────────────────────┐
                               │  ⑦ Report & Artifacts                            │
                               │     summary.md   ·  report.md   ·  findings.json  │  ◀── stdout
                               │     agent_handoff.md  ·  run-log.jsonl            │
                               └───────────────────────────────────────────────────┘

  PROVIDERS                        MODEL CATEGORIES (late binding)
  ─────────                        ────────────────────────────────────────────────
  ┌──────────────┐  Generate()     ┌────────────────────────────────────────────┐
  │  OpenAI      │◀────────────────│                                            │
  │  Anthropic   │◀────────────────│  fastest_good  ──▶  normalize · waivers    │
  │  Gemini      │◀────────────────│  balanced      ──▶  aggregate              │
  └──────────────┘                 │  best_code     ──▶  deep review personas   │
    ClientPool                     │  frontier_best ──▶  most critical checks   │
    (cached by                     │                                            │
    model+level)                   │  profile: gemini_std | openai | anthropic  │
                                   └────────────────────────────────────────────┘
```



```bash
go build -o ai-review
# Review a PR
./ai-review pr <repo_owner>/<repo_name> <pr_number> [options]

# Review a specific commit (compared to its parent by default)
./ai-review commit <repo_owner>/<repo_name> <commit_hash> [--compare-to <hash>] [options]

# Review specific files on a branch
./ai-review file <repo_owner>/<repo_name> <branch_name> <file_pattern...> [options]

# Review the diff between two branches
./ai-review branches <repo_owner>/<repo_name> <base_branch> <head_branch> [options]

# Get raw matching primers for planned changes (deterministic, no AI calls)
./ai-review context primers <repo_owner>/<repo_name> [--files <f>] [--functions <fn>] [--concepts <c>] [--format <fmt>]

# Discover available authoring concepts (deterministic, no AI calls)
./ai-review concepts <repo_owner>/<repo_name> [--files <f>] [--functions <fn>] [--format <fmt>]
```

### Primers and Concepts

Primers are project-specific instructions that help AI reviewers understand your codebase's conventions, architecture, and constraints. They can declare `authoring_concepts` in their frontmatter.

Coding agents can use the `concepts` command to discover the shared vocabulary of a repository before deciding which context to load:

1.  Call `ai-review concepts <repo>` to see available concepts.
2.  Filter them by providing `--files` or `--functions` if needed.
3.  Choose relevant concepts and load full primer context using `ai-review context primers --concepts <concepts>`.

### Global CLI Options

- `--model-profile <name>`: Use a specific model profile from `config.yaml`.
- `--max-tokens <n>`: Override the maximum tokens for AI responses.
- `--concurrency <n>`: Set the maximum number of personas to run concurrently (default: 5).
- `--dry-run`: Scan and report what personas and primers will be applied, but do not execute any AI calls. Useful for testing configuration and filtering logic without incurring costs.
- `--context-eval`: Perform a detailed evaluation of the context window size for each persona. This runs pre-run explainers, calculates accurate token counts using `tiktoken`, and reports a breakdown of context components (persona instructions, primers, diffs, etc.) without executing the actual review personas.
- `--context-eval-csv <file>`: In addition to the console report, output the context evaluation data to a CSV file. This is designed for TreeMap visualizations, with a hierarchical `path` column (e.g., `"persona,category,subcategory"`).
- `--include-personas <ids>`: Only run these specific personas (comma-separated list of IDs).
- `--exclude-personas <ids>`: Exclude these specific personas (comma-separated list of IDs).
- `--exclude-post-explainers`: Exclude all post-run explainers. Useful if you only want the review findings without high-level context or guides.
- `--prompt-only`: Runs all pre-run explainers fully, then generates and saves the prompts for all reviewers and post-run explainers without executing them. This is useful for manual review of AI prompts or for feeding them into a different tool. Any dependencies (like the summary of findings) are automatically omitted from the prompts in this mode.

### Examples:
```bash
# Review PR #1234
./ai-review pr google/go-github 1234 --max-tokens 500

# Review commit abc1234
./ai-review commit google/go-github abc1234

# Review commit abc1234 compared to def5678
./ai-review commit google/go-github abc1234 --compare-to def5678

# Review all .go files in config directory on master branch
./ai-review file google/go-github master "config/*.go"

# Review comparison between master and feature branches
./ai-review branches google/go-github master feature

# Dry run to see what would be executed for PR #1234
./ai-review pr google/go-github 1234 --dry-run

# Evaluate context window sizes for PR #1234
./ai-review pr google/go-github 1234 --context-eval

# Export context evaluation to CSV for visualization
./ai-review pr google/go-github 1234 --context-eval --context-eval-csv evaluation.csv

# Only run specific reviewers
./ai-review pr google/go-github 1234 --include-personas security,style

# Exclude specific reviewers and all post-run explainers
./ai-review pr google/go-github 1234 --exclude-personas logging --exclude-post-explainers

# Get primers for planned changes to specific files and functions
./ai-review context primers google/go-github --files "src/auth.go" --functions "Login"

# Get primers matching specific authoring concepts in JSON format
./ai-review context primers google/go-github --concepts "security,auth" --format json

# Discover all available authoring concepts for a repo
./ai-review concepts google/go-github

# Discover concepts relevant to specific files
./ai-review concepts google/go-github --files "src/auth.go" --format names
```

## Setup

1. Install Go 1.25+
2. Install GitHub CLI (`gh`) and authenticate: `gh auth login`
3. Install Git
4. Set up credentials for the AI providers you actually use in `config.yaml`:
   - `OPENAI_API_KEY` for `provider: openai`
   - `ANTHROPIC_API_KEY` for `provider: anthropic`
   - `GEMINI_API_KEY` for `provider: gemini`

You do not need to set all three unless your configuration uses all three.

## Configuration

The tool expects repository-specific configuration under `.ai-review/<repo_owner>/<repo_name>/`. The main config file lives at `.ai-review/<repo_owner>/<repo_name>/config.yaml`, and local personas, primers, and waivers also live under that repo-scoped directory tree.

### Model Selection

Model selection is handled through **Definitions** and **Profiles**. This allows you to define models once and reuse them across different tiers (e.g., "cheap", "balanced", "best_code") and switch between entirely different providers (e.g., Gemini vs OpenAI) using a single flag.

#### 1. Model Definitions

Model definitions describe the underlying LLM, its provider, and its pricing. You can define these in `config.yaml` or in a standalone `models.yaml` file for reuse across multiple repositories.

```yaml
model_definitions:
  gpt_4o:
    provider: openai
    model: gpt-4o
    max_tokens: 32000
    input_price_per_million: 2.50
    output_price_per_million: 10.00
  gemini_2_flash:
    provider: gemini
    model: gemini-2.0-flash
    max_tokens: 32000
    input_price_per_million: 0.10
    output_price_per_million: 0.40
```

#### 2. Model Profiles

Profiles map abstract categories used by personas (like `cheap`, `balanced`, `best_code`) to specific model definitions. You can also override any definition field (like `reasoning_level`) at the profile level.

```yaml
default_profile: gemini_standard

model_profiles:
  gemini_standard:
    cheap:
      id: gemini_2_flash
    balanced:
      id: gemini_2_flash
    best_code:
      id: gemini_1_5_pro
      reasoning_level: high
  
  openai:
    cheap:
      id: gpt_4o_mini
    balanced:
      id: gpt_4o
    best_code:
      id: o1
```

#### 3. models.yaml

To avoid repeating model definitions in every repository's `config.yaml`, you can create a `models.yaml` file. The tool searches for `models.yaml` in:
1. `.ai-review/` or `.ai-reviewer/` relative to any search path.
2. The same directory as your active `config.yaml`.

Definitions in `models.yaml` are merged with those in `config.yaml`.

### Discovery and Organization

The tool scans for personas, primers, and waivers in two locations:

- **Repository branch scanning**: The tool scans all `.md` files in the repository branch being evaluated. A file is included as a persona, primer, or waiver if it contains an explicit `ai_review: persona`, `ai_review: primer`, or `ai_review: waiver` field in its YAML frontmatter. This allows you to keep committed artifacts alongside the code they relate to.
- **Repo-scoped local directories**: Any `.md` file within `.ai-review/<owner>/<repo>/personas/`, `.ai-review/<owner>/<repo>/primers/`, or `.ai-review/<owner>/<repo>/waivers/` is automatically loaded from the local checkout of this tool. All subdirectories are searched recursively. Files in these directories do **not** require an `ai_review` field in their frontmatter.

The local checkout is repo-scoped only. The tool does not load local global directories like `.ai-review/personas/` or arbitrary local Markdown files outside `.ai-review/<owner>/<repo>/...`.

### Context Primers

The `context primers` command is a deterministic tool for pre-authoring context lookup. It does not make any AI calls or require a review run to exist. It is designed for external coding agents (like Codex, Claude Code, or Junie) to load relevant repository context before starting a change. It does not require `gh` to be installed as it operates on local files.

Matches are determined by a combination of regular filters and authoring concepts:
- **Regular Filters**: `path_filters` (matched against `--files`) and `function_filters` (matched against `--functions`).
- **Authoring Concepts**: `authoring_concepts` (defined in primer frontmatter and matched against `--concepts`).

**Matching Rules:**
- If a primer has `authoring_concepts` and the user provided `--concepts`, **both** the regular filters and at least one concept must match.
- If the primer has no `authoring_concepts`, only regular filters must match.
- If the user provided no `--concepts`, only regular filters must match.
- If the user provided `--concepts` but the primer has no `authoring_concepts`, the concepts do not affect the match (only regular filters are checked).
- Concept-only input (without `--files` or `--functions`) will only match primers that have no regular filters (or empty filters).

#### Example Primer with Authoring Concepts:
```markdown
---
id: governance-parameters
type: implementation
authoring_concepts: ["governance", "params"]
path_filters: ["./inference-chain/**/params.go"] 
---
Parameters are controlled via governance votes...
```

### Personas

Create persona files in `.ai-review/<owner>/<repo>/personas/` for local repo-scoped configuration, or commit them anywhere in the target repository with `ai_review: persona` in frontmatter. Personas support several fields in their YAML frontmatter:

```markdown
---
id: security
role: reviewer           # optional: reviewer (default) | explainer
stage: pre              # optional: pre | post (only for explainers)
include_findings: true  # optional: include the aggregated summary report (only for post-run explainers)
include_explainers: ["state-modified"] # optional: list of pre-run explainer IDs to include their analysis for files
exclude_diff: true      # optional: exclude the full unified diff and show stats instead
model_category: best_code
max_tokens: 4096        # optional: overrides model category limit
path_filters:           # optional: only run if these files changed
  - "inference-chain/**/*.go"
exclude_filters:        # optional: ignore these files
  - "**/*_test.go"
regex_filters:          # optional: only include files where changed lines match any of these regexes
  - "TODO"
any:                    # optional: logical OR between sub-filters
  - path_filters: ["src/legacy/**"]
  - regex_filters: ["TODO: rewrite"]
all:                    # optional: logical AND between sub-filters
  - path_filters: ["**/*.go"]
  - any:
      - function_filters: ["HandleRequest"]
      - regex_filters: ["auth_check"]
branch_filters:         # optional: only apply to specific branch globs
  - "main"
  - "release/*"
function_filters:       # optional: only apply if specific functions are modified
  - "ProcessData"
line_numbers_filter:    # optional: list of line ranges
  - start: 10
    end: 20
date_filter: "2025-01-01" # optional: only apply if commit date is BEFORE this date
---
You are a security expert. Review the following PR for security vulnerabilities.
```

#### Roles and Stages

- **Reviewer**: (Default) Analyzes the code and produces findings. Findings are automatically normalized into structured data and later aggregated.
- **Explainer (Pre)**: Runs before reviewers. Must output JSON (file-to-analysis mapping). Its analysis is injected into the context of all subsequent personas for that file.
- **Explainer (Post)**: Runs after reviewers. Its full output is included in the final report under an "Explanations" section. Useful for providing human-readable guides or high-level summaries.

#### Advanced Logical Filtering

Filters normally operate as a logical **AND** (a file must match the path filters AND the regex filters, etc.). For more complex logic, you can use `any` (OR) and `all` (AND) blocks, which can be nested arbitrarily:

- **any**: The filter matches if *any* of its sub-filters match.
- **all**: The filter matches only if *all* of its sub-filters match.

Existing flat filters (like `path_filters` and `regex_filters` defined at the top level) continue to work as an implicit `all` block for backward compatibility.

### Primers

Primers provide extra context, constraints, or blueprints to personas based on the specific files they are analyzing. They are included in the persona prompt only if the persona is analyzing files that match the primer's filters.

Create primer files in `.ai-review/<owner>/<repo>/primers/` for local repo-scoped configuration, or commit them anywhere in the target repository with `ai_review: primer` in frontmatter. They support the same filtering fields as personas (`path_filters`, `exclude_filters`, `regex_filters`, `branch_filters`, `function_filters`, `line_numbers_filter`, `date_filter`):

```markdown
---
id: inference-chain-blueprint
type: blueprint
path_filters:
  - "inference-chain/**/*.go"
---
When modifying the inference chain, ensure that you follow the established patterns:
1. ...
```

The `type` field matches the types defined in `config.yaml` to provide additional intent to the AI.

### Waivers

Waivers allow you to automatically suppress specific findings based on predefined rules. This is useful for ignoring known issues, legacy code patterns, or false positives.

Create waiver files in `.ai-review/<owner>/<repo>/waivers/` for local repo-scoped configuration, or commit them anywhere in the target repository with `ai_review: waiver` in frontmatter. Waivers use the same filters as Personas and Primers to determine their applicability to a finding's location.

```markdown
---
id: ignore-legacy-auth
model_category: fastest_good
path_filters:
  - "legacy/auth/**/*.go"
date_filter: "2024-01-01"
---
We are aware of the weak hashing in the legacy auth module, but it is scheduled for decommissioning and should not be flagged in new PRs unless the logic is significantly altered.
```

#### How Waivers Work

1. **Location Filtering**: When a reviewer produces a finding, the tool checks for any Waivers whose filters (`path_filters`, `branch_filters`, etc.) match the finding's location.
2. **LLM Validation**: If a waiver's location matches, the tool sends the finding's details, the relevant code diff, and the waiver's instructions to an LLM (specified by `model_category`).
3. **Decision**: The LLM determines if the waiver truly applies to this specific issue.
4. **Reporting**: Waived issues are excluded from the main sections of the report ("Must Fix", etc.) and listed in a separate "Waived Issues" section at the end of the report.

#### Token Limit Precedence

The maximum tokens for a response is determined by (highest priority first):
1. `--max-tokens` CLI flag
2. `max_tokens` in persona frontmatter
3. `max_tokens` in `config.yaml` model mapping

## Repository Storage

By default, the tool clones repositories into a `.repos` directory relative to the current working directory. This directory is organized by owner and repository name (e.g., `.repos/google/go-github`). If you are already inside the target repository (or a subdirectory of it), the tool will use it directly instead of cloning.

A `.gitignore` file is provided in the project root to ensure that the `.repos` directory and the compiled `ai-review` binary are not tracked by version control.

## How it works

The tool executes a multi-stage pipeline:

1. **Fetch Context**: Uses `gh` CLI and `git` to fetch PR details and compute the unified diff.
2. **Pre-run Explainers**: Executes personas with `role: explainer` and `stage: pre`. They provide initial research that is injected into later prompts.
3. **Reviewers**: Executes standard personas. If any **Primers** match the files being analyzed by a persona, they are injected into its prompt as extra context. Each reviewer's raw output is immediately processed by a **Normalization** step (using a cheap model) to extract structured findings (file, line, summary, severity).
4. **Waiver Evaluation**: Any findings produced by reviewers are checked against applicable **Waivers**. If a waiver matches the location and is confirmed by an LLM, the finding is marked as waived.
5. **Post-run Explainers**: Executes personas with `role: explainer` and `stage: post`. They provide high-level context or human instructions.
6. **Aggregation**: All non-waived findings from all reviewers are sent to an **Aggregator** LLM (using the `balanced` model category). It deduplicates issues, clusters related findings, and produces a concise Markdown summary.
7. **Reporting**: The final report is printed to stdout and saved to the run directory. Waived findings are listed in a separate section.

## Output and Artifacts

Each run generates a timestamped directory in `.ai-review/<repo_owner>/<repo_name>/runs/<target_id>/<timestamp>/` containing:
- `target_id` is the PR number (for PR reviews), the short commit hash (for commit reviews), `file-<branch_name>` (for file reviews), or `branches-<target_repo>` (for branch reviews).
- `summary.md`: The aggregated Markdown summary.
- `report.md`: The full report including explanations and stats.
- `all_findings.json`: All normalized findings from all personas.
- `agent_handoff.md`: A handoff prompt for a coding agent to help with review triage.
- `persona_name/`: Subdirectories for each persona containing their `raw.md` output and `findings.json` (or `parsed.json`).

Stats and token usage are also appended to `.ai-review/<repo_owner>/<repo_name>/run-log.jsonl`.

## Cost Tracking

The final report includes a "Stats" section with:
- Token usage (In/Out) per persona and pipeline step.
- Estimated cost per step based on prices in `config.yaml`.
- Total estimated cost for the run.
- Usage summary grouped by model.

## Context Evaluation

The `--context-eval` flag allows you to analyze and optimize the context being sent to each AI persona without performing a full review. This is crucial for staying within model token limits and understanding which parts of your prompt are consuming the most space.

### How it works:
1. **Runs Pre-Run Explainers**: Since explainers provide context for subsequent reviewers, they are executed to get their actual output.
2. **Builds Full Prompts**: The tool constructs the exact prompt that would be sent to each persona, including persona instructions, matched primers, explainer outputs, and file diffs.
3. **Token Counting**: Uses `tiktoken` with the correct encoding for the selected model to provide accurate token counts.
4. **Breakdown Report**: Generates a detailed breakdown for each persona:
    - **Persona**: The base instructions for the persona.
    - **Primers**: Breakdown of tokens for each matched primer.
    - **Explainers**: Tokens from pre-run explainer analysis.
    - **Diffs**: Breakdown of tokens for each file's diff.
    - **Other**: Primers, instructions, and other metadata.

### Visualization with CSV:
Using `--context-eval-csv <filename.csv>` generates a flat data file suitable for TreeMap visualizations (like those in Excel or specialized tools). Each row includes:
- **Tokens**: Accurate token count for that specific component.
- **Chars**: Character count for that component.
- **Cost**: Estimated cost for that specific component (PPM tokens estimate).
- **Model**: The model used for this calculation (e.g., `gpt-4o(high)`).
- **Persona**: The ID of the persona.
- **Category**: The context category (e.g., `diff`, `primers`).
- **Label**: The deepest level of the hierarchical path (e.g., filename, primer ID, or category).
- **Path**: A hierarchical identifier using commas as separators, double-quoted: `"persona,category,subcategory,filename"` (e.g., `"security,diff,src,main.go"`).
