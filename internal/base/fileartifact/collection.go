package fileartifact

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const MaxCollectionIssues = 128

// CollectionEvidence describes what the bounded file collector observed.
// Complete concerns collection only; it never means that business requirements
// are complete or satisfied. Nil denotes a producer with no collection receipt.
type CollectionEvidence struct {
	SchemaVersion int               `json:"schema_version"`
	Complete      bool              `json:"complete"`
	Limits        CollectionLimits  `json:"limits"`
	Issues        []CollectionIssue `json:"issues,omitempty"`
}

type CollectionLimits struct {
	MaxFiles      int `json:"max_files"`
	MaxFileBytes  int `json:"max_file_bytes"`
	MaxTotalBytes int `json:"max_total_bytes"`
}

// Claimed marks a natural-language assertion, not a frozen requirement.
type CollectionIssue struct {
	Path    string `json:"path,omitempty"`
	Reason  string `json:"reason"`
	Kind    string `json:"kind"` // missing_reference | limit | error
	Claimed bool   `json:"claimed,omitempty"`
}

func ValidateCollectionEvidence(evidence *CollectionEvidence) error {
	if evidence == nil {
		return nil
	}
	if evidence.SchemaVersion != 1 || evidence.Limits.MaxFiles <= 0 || evidence.Limits.MaxFiles > MaxArtifactCount ||
		evidence.Limits.MaxFileBytes <= 0 || evidence.Limits.MaxFileBytes > MaxArtifactBytes ||
		evidence.Limits.MaxTotalBytes <= 0 || evidence.Limits.MaxTotalBytes > MaxArtifactsTotalBytes || len(evidence.Issues) > MaxCollectionIssues {
		return fmt.Errorf("invalid artifact collection bounds or version")
	}
	for _, issue := range evidence.Issues {
		if len(issue.Path) > 512 || !utf8.ValidString(issue.Path) || strings.ContainsAny(issue.Path, "\\\x00\r\n") || strings.HasPrefix(issue.Path, "/") ||
			len(issue.Reason) == 0 || len(issue.Reason) > 160 || !utf8.ValidString(issue.Reason) {
			return fmt.Errorf("invalid artifact collection issue")
		}
		switch issue.Kind {
		case "missing_reference", "limit", "error":
		default:
			return fmt.Errorf("invalid artifact collection issue kind")
		}
		if evidence.Complete && (issue.Kind == "limit" || issue.Kind == "error") {
			return fmt.Errorf("incomplete artifact collection marked complete")
		}
	}
	return nil
}

// CollectionError returns technical failures only. Missing references and
// unsupported file capabilities belong to delivery verification.
func CollectionError(evidence *CollectionEvidence) error {
	if err := ValidateCollectionEvidence(evidence); err != nil {
		return err
	}
	if evidence != nil {
		for _, issue := range evidence.Issues {
			if issue.Kind == "error" {
				return fmt.Errorf("artifact_collection_error: %s: %q", issue.Reason, issue.Path)
			}
		}
	}
	return nil
}
