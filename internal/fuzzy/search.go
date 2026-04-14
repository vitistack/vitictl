package fuzzy

import (
	"sort"

	"github.com/sahilm/fuzzy"
)

// Candidate is one searchable row: a primary label that is fuzzy-matched
// against, plus the original item it references.
type Candidate[T any] struct {
	Label string
	Item  T
}

// Match is a scored result.
type Match[T any] struct {
	Candidate[T]
	Score int
}

// Search fuzzy-matches `query` against the labels of `candidates`. An empty
// query returns all candidates in their original order. Results are sorted by
// descending match score (best first).
func Search[T any](query string, candidates []Candidate[T]) []Match[T] {
	if query == "" {
		out := make([]Match[T], len(candidates))
		for i, c := range candidates {
			out[i] = Match[T]{Candidate: c}
		}
		return out
	}
	labels := make([]string, len(candidates))
	for i, c := range candidates {
		labels[i] = c.Label
	}
	results := fuzzy.Find(query, labels)
	out := make([]Match[T], 0, len(results))
	for _, r := range results {
		out = append(out, Match[T]{
			Candidate: candidates[r.Index],
			Score:     r.Score,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
