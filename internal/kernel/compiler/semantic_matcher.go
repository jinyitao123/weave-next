package compiler

import (
	"context"
	"math"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
)

// matchTimeout bounds the embedding call: stdlib.SkillMatcher.Match carries no
// context, so a hung embedder endpoint must not stall the chat compile path —
// on timeout the matcher falls back to keyword matching.
const matchTimeout = 10 * time.Second

// SemanticMatcher uses embeddings to match skills by meaning, not keywords.
// Works across languages — a Chinese message matches an English skill description.
// Falls back to KeywordMatcher if embedder is unavailable.
type SemanticMatcher struct {
	Embedder  contract.Embedder
	Threshold float64 // minimum cosine similarity to consider a match (default 0.3)
	fallback  stdlib.SkillMatcher
}

func NewSemanticMatcher(embedder contract.Embedder) *SemanticMatcher {
	return &SemanticMatcher{
		Embedder:  embedder,
		Threshold: 0.3,
		fallback:  &stdlib.KeywordMatcher{},
	}
}

func (m *SemanticMatcher) Match(userMessage string, skills []stdlib.SkillDef) []stdlib.SkillMatchResult {
	if m.Embedder == nil || len(skills) == 0 || userMessage == "" {
		return m.fallback.Match(userMessage, skills)
	}

	// Build texts to embed: [userMessage, skill0_desc, skill1_desc, ...]
	texts := make([]string, 0, len(skills)+1)
	texts = append(texts, userMessage)
	for _, s := range skills {
		texts = append(texts, s.Name+" "+s.Description)
	}

	ctx, cancel := context.WithTimeout(context.Background(), matchTimeout)
	defer cancel()
	embeddings, err := m.Embedder.Embed(ctx, texts)
	if err != nil || len(embeddings) != len(texts) {
		// Embedder failed — fall back to keyword matching
		return m.fallback.Match(userMessage, skills)
	}

	msgVec := embeddings[0]
	var results []stdlib.SkillMatchResult

	for i, s := range skills {
		skillVec := embeddings[i+1]
		score := cosineSimilarity(msgVec, skillVec)
		if score >= m.Threshold {
			results = append(results, stdlib.SkillMatchResult{Skill: s, Score: score})
		}
	}

	// Sort by score descending
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	return results
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
