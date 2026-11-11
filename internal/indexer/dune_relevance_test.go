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

func TestShortWorkDoesNotAcceptSameAuthorLongerWork(t *testing.T) {
	for _, tc := range []struct {
		name, title, author     string
		wrong, right, qualifier string
	}{
		{
			name: "Dune sequel", title: "Dune", author: "Frank Herbert",
			wrong:     "Dune Messiah by Frank Herbert [ENG / MP3]",
			right:     "Dune by Frank Herbert [ENG / MP3]",
			qualifier: "Dune (Unabridged) by Frank Herbert [ENG / MP3]",
		},
		{
			name: "Frankenstein sibling", title: "Frankenstein", author: "Mary Shelley",
			wrong:     "Frankenstein Unbound by Mary Shelley [ENG / EPUB]",
			right:     "Frankenstein by Mary Shelley [ENG / EPUB]",
			qualifier: "Frankenstein; or, The Modern Prometheus by Mary Shelley [ENG / EPUB]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filterRelevant(toResults(tc.wrong, tc.right, tc.qualifier), tc.title, tc.author, nil)
			if contains(got, tc.wrong) {
				t.Errorf("different same-author work was kept: %q, got %v", tc.wrong, resultTitles(got))
			}
			for _, keep := range []string{tc.right, tc.qualifier} {
				if !contains(got, keep) {
					t.Errorf("right work or known alternate title was dropped: %q, got %v", keep, resultTitles(got))
				}
			}
		})
	}
}

func TestDuneAuthorFirstSequelIsNotTheNovel(t *testing.T) {
	wrong := "Frank Herbert - Dune Messiah [ENG / MP3]"
	right := "Frank Herbert - Dune [ENG / MP3]"
	got := filterRelevant(toResults(wrong, right), "Dune", "Frank Herbert", nil)
	if contains(got, wrong) || !contains(got, right) {
		t.Fatalf("author-first sequel must not fill Dune: %v", resultTitles(got))
	}
}

func TestDuneSequelSubtitleAndEditionCannotFillNovel(t *testing.T) {
	right := "Dune (Unabridged) by Frank Herbert [ENG / MP3]"
	for _, wrong := range []string{
		"Dune: Messiah by Frank Herbert [ENG / MP3]",
		"Dune Messiah: A Novel by Frank Herbert [ENG / MP3]",
	} {
		t.Run(wrong, func(t *testing.T) {
			got := filterRelevant(toResults(wrong, right), "Dune", "Frank Herbert", nil)
			if contains(got, wrong) || !contains(got, right) {
				t.Fatalf("distinct work must not fill Dune: %v", resultTitles(got))
			}
		})
	}
}

func TestDuneTerminalEditionLabelsDoNotHideSequelOrImitateAnotherWork(t *testing.T) {
	right := "Dune by Frank Herbert [ENG / MP3]"
	for _, wrong := range []string{
		"Dune Messiah [Unabridged] by Frank Herbert [ENG / MP3]",
		"Dune Messiah (A Novel) by Frank Herbert [ENG / MP3]",
		"Dune Messiah [Narrated by Simon Vance] by Frank Herbert [ENG / MP3]",
		"Dune Messiah [Unabridged MP3] by Frank Herbert [ENG / MP3]",
	} {
		t.Run(wrong, func(t *testing.T) {
			got := filterRelevant(toResults(wrong, right), "Dune", "Frank Herbert", nil)
			if contains(got, wrong) || !contains(got, right) {
				t.Fatalf("edition label must not hide a distinct work: %v", resultTitles(got))
			}
		})
	}
	for _, edition := range []string{
		"Dune (Illustrated) by Frank Herbert [ENG / MP3]",
		"Dune (Dramatized) by Frank Herbert [ENG / MP3]",
		"Dune [Narrated by Simon Vance] by Frank Herbert [ENG / MP3]",
		"Dune [Unabridged MP3] by Frank Herbert [ENG / MP3]",
	} {
		t.Run(edition, func(t *testing.T) {
			got := filterRelevant(toResults(edition), "Dune", "Frank Herbert", nil)
			if !contains(got, edition) {
				t.Fatalf("recognized edition was dropped: %v", resultTitles(got))
			}
		})
	}
}

func TestDuneSceneStyleSequelIsNotTheNovel(t *testing.T) {
	wrong := "Frank Herbert - Dune.Messiah [ENG / MP3]"
	right := "Frank Herbert - Dune.Unabridged [ENG / MP3]"
	got := filterRelevant(toResults(wrong, right), "Dune", "Frank Herbert", nil)
	if contains(got, wrong) || !contains(got, right) {
		t.Fatalf("scene-style sequel must not fill Dune: %v", resultTitles(got))
	}
}

func TestDuneDottedByCreditSequelIsNotNovel(t *testing.T) {
	wrong := "Dune.Messiah.by.Frank.Herbert.[ENG / MP3]"
	right := "Dune.by.Frank.Herbert.[ENG / MP3]"
	got := filterRelevant(toResults(wrong, right), "Dune", "Frank Herbert", nil)
	if contains(got, wrong) || !contains(got, right) {
		t.Fatalf("dotted by-credit sequel must not fill Dune: %v", resultTitles(got))
	}
}

func TestDuneNarratorByCreditDoesNotImitateAnotherWork(t *testing.T) {
	for _, right := range []string{
		"Dune [Narrated by Frank Herbert] [ENG / MP3]",
		"Dune [Read by Frank Herbert] [ENG / MP3]",
		"Dune [Performed by Frank Herbert] [ENG / MP3]",
		"Dune [Presented by Frank Herbert] [ENG / MP3]",
		"Dune [Told by Frank Herbert] [ENG / MP3]",
		"Dune [Narrated by Frank Herbert] by Frank Herbert [ENG / MP3]",
	} {
		t.Run(right, func(t *testing.T) {
			got := filterRelevant(toResults(right), "Dune", "Frank Herbert", nil)
			if !contains(got, right) {
				t.Fatalf("author-narrated novel was dropped: %v", resultTitles(got))
			}
		})
	}
	for _, wrong := range []string{
		"Dune Messiah [Narrated by Frank Herbert] by Frank Herbert [ENG / MP3]",
		"Dune Messiah [Performed by Frank Herbert] by Frank Herbert [ENG / MP3]",
		"Dune Messiah [Presented by Frank Herbert] by Frank Herbert [ENG / MP3]",
		"Dune Messiah [Told by Frank Herbert] by Frank Herbert [ENG / MP3]",
	} {
		if got := filterRelevant(toResults(wrong), "Dune", "Frank Herbert", nil); contains(got, wrong) {
			t.Fatalf("sequel author credit outside narrator label was kept: %v", resultTitles(got))
		}
	}
}

func TestDuneDottedByCreditPreservesEditionEvidence(t *testing.T) {
	wrong := "Dune.Messiah.[Unabridged].by.Frank.Herbert.[ENG / MP3]"
	right := "Dune.[Unabridged].by.Frank.Herbert.[ENG / MP3]"
	got := filterRelevant(toResults(wrong, right), "Dune", "Frank Herbert", nil)
	if contains(got, wrong) || !contains(got, right) {
		t.Fatalf("dotted by-credit edition must not hide another work: %v", resultTitles(got))
	}
}

func TestDuneDottedAuthorFirstSequelIsNotNovel(t *testing.T) {
	wrong := "Frank.Herbert.-.Dune.Messiah.[ENG / MP3]"
	right := "Frank.Herbert.-.Dune.[ENG / MP3]"
	got := filterRelevant(toResults(wrong, right), "Dune", "Frank Herbert", nil)
	if contains(got, wrong) || !contains(got, right) {
		t.Fatalf("dotted author-first sequel must not fill Dune: %v", resultTitles(got))
	}
}

func TestDuneBookTitleMetadataDisclosesAnotherWork(t *testing.T) {
	rows := toResults("Dune by Frank Herbert [ENG / MP3]", "Dune Messiah [ENG / MP3]")
	rows[0].BookTitle = "Dune Messiah"
	rows[1].Author = "Frank Herbert"
	if got := filterRelevant(rows, "Dune", "Frank Herbert", nil); len(got) != 0 {
		t.Fatalf("indexer book-title and author metadata named a different work: %v", resultTitles(got))
	}
}
