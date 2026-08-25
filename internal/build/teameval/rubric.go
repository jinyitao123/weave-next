package teameval

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

// ScenarioArtifact is the evaluator-only view of one frozen scenario run.
// Output is untrusted candidate data; deterministic checks never interpret it
// as instructions.
type ScenarioArtifact struct {
	ScenarioID     string
	TerminalStatus string
	Expected       string
	Output         string
}

// RubricEvaluation is the deterministic business-quality evidence generated
// from one contract and all of its scenario artifacts.
type RubricEvaluation struct {
	Scores         []teambuild.RubricScore
	SevereDefects  []string
	FailureSamples []string
}

// EvaluateRubric applies the contract's dimensions to every scenario and
// returns the worst scenario score for each dimension. This deliberately
// conservative aggregation prevents one good run from hiding a bad scenario.
// The baseline checks terminal success, substantive output, expected-keyword
// coverage, and (for explicitly structural dimensions) machine-detectable
// JSON/heading/list structure. Semantic LLM judging remains a future layer.
func EvaluateRubric(
	contract teambuild.EvaluationContract,
	artifacts []ScenarioArtifact,
) RubricEvaluation {
	result := RubricEvaluation{
		Scores: make([]teambuild.RubricScore, 0, len(contract.Rubric)),
	}
	failureScenario := make(map[string]bool)
	for _, dimension := range contract.Rubric {
		minimum := dimension.MaxScore
		details := make([]string, 0, len(artifacts))
		if len(artifacts) == 0 {
			minimum = 0
			details = append(details, "no scenario artifacts")
		}
		for _, artifact := range artifacts {
			base, detail := deterministicScenarioScore(dimension, artifact)
			score := scaleRubricScore(base, dimension.MaxScore)
			if score < minimum {
				minimum = score
			}
			details = append(details, fmt.Sprintf("%s=%d/%d (%s)",
				artifact.ScenarioID, score, dimension.MaxScore, detail))
			if score < dimension.PassThreshold && !failureScenario[artifact.ScenarioID] {
				failureScenario[artifact.ScenarioID] = true
				result.FailureSamples = append(result.FailureSamples,
					fmt.Sprintf("scenario %s output: %s", artifact.ScenarioID, outputExcerpt(artifact.Output)))
			}
		}
		result.Scores = append(result.Scores, teambuild.RubricScore{
			DimensionID: dimension.ID,
			Score:       minimum,
			Reason: fmt.Sprintf(
				"deterministic worst-scenario baseline; %s; semantic LLM judge deferred",
				strings.Join(details, "; "),
			),
		})
	}

	definition := strings.TrimSpace(contract.SevereDefectDefinition)
	for _, artifact := range artifacts {
		if severeDefectObserved(definition, artifact.Output) {
			result.SevereDefects = append(result.SevereDefects, fmt.Sprintf(
				"scenario %s matched severe defect definition %q",
				artifact.ScenarioID, definition,
			))
		}
	}
	return result
}

// FailedRubricDimensions returns every missing or below-threshold dimension
// in contract order. Missing scores fail closed.
func FailedRubricDimensions(
	contract teambuild.EvaluationContract,
	scores []teambuild.RubricScore,
) []string {
	byID := make(map[string]int, len(scores))
	for _, score := range scores {
		byID[score.DimensionID] = score.Score
	}
	failed := make([]string, 0)
	for _, dimension := range contract.Rubric {
		score, present := byID[dimension.ID]
		if !present || score < dimension.PassThreshold {
			failed = append(failed, dimension.ID)
		}
	}
	return failed
}

func deterministicScenarioScore(
	dimension teambuild.RubricDimension,
	artifact ScenarioArtifact,
) (int, string) {
	terminal := 0
	if isSuccessfulTerminal(artifact.TerminalStatus) {
		terminal = 100
	}
	substantive := substantiveOutputScore(artifact.Expected, artifact.Output)
	coverage := expectedKeywordCoverage(artifact.Expected, artifact.Output)

	dimensionText := strings.ToLower(strings.Join([]string{
		dimension.ID, dimension.Name, dimension.Description,
	}, " "))
	base := (20*terminal + 20*substantive + 60*coverage) / 100
	check := "terminal+substantive+expected_coverage"
	if containsAny(dimensionText,
		"structure", "format", "section", "table", "json",
		"结构", "格式", "分节", "表格") {
		structure := structuredOutputScore(artifact.Output)
		base = (20*terminal + 20*substantive + 30*coverage + 30*structure) / 100
		check = "terminal+substantive+expected_coverage+structure"
	}
	return base, fmt.Sprintf("%s %d/100", check, base)
}

func scaleRubricScore(base, maximum int) int {
	if base <= 0 || maximum <= 0 {
		return 0
	}
	if base >= 100 {
		return maximum
	}
	return (base*maximum + 50) / 100
}

func substantiveOutputScore(expected, output string) int {
	outputLength := utf8.RuneCountInString(strings.TrimSpace(output))
	if outputLength == 0 {
		return 0
	}
	target := utf8.RuneCountInString(strings.TrimSpace(expected)) / 2
	if target < 12 {
		target = 12
	}
	if outputLength >= target {
		return 100
	}
	return outputLength * 100 / target
}

func expectedKeywordCoverage(expected, output string) int {
	terms := rubricTerms(expected)
	if len(terms) == 0 {
		return 0
	}
	normalizedOutput := strings.ToLower(output)
	matched := 0
	for _, term := range terms {
		if strings.Contains(normalizedOutput, term) {
			matched++
		}
	}
	return matched * 100 / len(terms)
}

func rubricTerms(value string) []string {
	fields := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := make(map[string]bool, len(fields))
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] || (utf8.RuneCountInString(field) < 3 && !containsCJK(field)) {
			continue
		}
		seen[field] = true
		terms = append(terms, field)
	}
	return terms
}

func containsCJK(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func structuredOutputScore(output string) int {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return 0
	}
	if json.Valid([]byte(trimmed)) {
		return 100
	}
	lines := strings.Split(trimmed, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "- ") ||
			strings.HasPrefix(line, "* ") || strings.Contains(line, "|") {
			return 100
		}
	}
	if len(lines) >= 2 {
		return 70
	}
	return 0
}

func severeDefectObserved(definition, output string) bool {
	normalizedOutput := strings.ToLower(strings.TrimSpace(output))
	if normalizedOutput == "" {
		return false
	}
	if strings.Contains(normalizedOutput, "severe_defect:") ||
		strings.Contains(normalizedOutput, "严重缺陷：") ||
		strings.Contains(normalizedOutput, "严重缺陷:") {
		return true
	}
	normalizedDefinition := strings.ToLower(strings.TrimSpace(definition))
	return utf8.RuneCountInString(normalizedDefinition) >= 4 &&
		strings.Contains(normalizedOutput, normalizedDefinition)
}

func outputExcerpt(output string) string {
	const maximum = 160
	output = strings.Join(strings.Fields(strings.TrimSpace(output)), " ")
	if output == "" {
		return "<empty>"
	}
	runes := []rune(output)
	if len(runes) <= maximum {
		return output
	}
	return string(runes[:maximum]) + "..."
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
