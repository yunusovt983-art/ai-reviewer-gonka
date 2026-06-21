package main

import "ai-reviewer/internal/domain/review"

// The review pipeline domain (Finding, the ModelClient port, normalization and
// aggregation services) lives in internal/domain/review. These aliases keep the
// rest of package main and its tests referring to them unqualified.
type (
	Finding                 = review.Finding
	NormalizationResponse   = review.NormalizationResponse
	PreRunAnalysis          = review.PreRunAnalysis
	PreRunExplainerResponse = review.PreRunExplainerResponse
)

const (
	PreRunExplainerSystemPrompt = review.PreRunExplainerSystemPrompt
	NormalizationSystemPrompt   = review.NormalizationSystemPrompt
	AggregatorSystemPrompt      = review.AggregatorSystemPrompt
)

var (
	extractJSON                = review.ExtractJSON
	NormalizePersonaOutput     = review.NormalizePersonaOutput
	ParsePreRunExplainerOutput = review.ParsePreRunExplainerOutput
	AggregateFindings          = review.AggregateFindings
)
