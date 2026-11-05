package indexer

import "testing"

func TestDuneShortTitle(t *testing.T) {
	const (
		biography = "Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]"
		audio     = "Frank Herbert - Dune [ENG / M4B]"
		ebook     = "Dune by Frank Herbert [ENG / EPUB]"
	)

	got := filterRelevant(toResults(biography, audio, ebook), "Dune", "Frank Herbert", nil)
	if contains(got, biography) {
		t.Errorf("biography must be rejected for Dune by Frank Herbert, got %v", resultTitles(got))
	}
	for _, want := range []string{audio, ebook} {
		if !contains(got, want) {
			t.Errorf("expected %q to stay relevant, got %v", want, resultTitles(got))
		}
	}
}
