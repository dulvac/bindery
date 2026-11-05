package indexer

import "testing"

func TestDuneShortTitle(t *testing.T) {
	const (
		biography = "Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]"
		honorific = "Dune by Dr. Brian Herbert - The Biography of Frank Herbert [ENG / EPUB]"
		audio     = "Frank Herbert - Dune [ENG / M4B]"
		ebook     = "Dune by Frank Herbert [ENG / EPUB]"
	)

	got := filterRelevant(toResults(biography, honorific, audio, ebook), "Dune", "Frank Herbert", nil)
	for _, wrong := range []string{biography, honorific} {
		if contains(got, wrong) {
			t.Errorf("biography %q must be rejected for Dune by Frank Herbert, got %v", wrong, resultTitles(got))
		}
	}
	for _, want := range []string{audio, ebook} {
		if !contains(got, want) {
			t.Errorf("expected %q to stay relevant, got %v", want, resultTitles(got))
		}
	}
}
