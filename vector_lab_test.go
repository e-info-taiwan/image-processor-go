package main

import "testing"

func TestIsW480Object(t *testing.T) {
	for _, name := range []string{
		"images/a-w480.jpg",
		"images/nested/file-w480.WEBP",
		"images/a-w480.jpeg",
	} {
		if !isW480Object(name) {
			t.Fatalf("expected %q to be a w480 object", name)
		}
	}
	for _, name := range []string{
		"images/a.jpg",
		"images/a-w800.jpg",
		"a-w480.jpg",
		"images/a-w480.svg",
	} {
		if isW480Object(name) {
			t.Fatalf("expected %q not to be a w480 object", name)
		}
	}
}

func TestParseW480ObjectUsesOriginalFileID(t *testing.T) {
	fileID, format, ok := parseW480Object("images/2026/abc-123-w480.webP")
	if !ok || fileID != "2026/abc-123" || format != "webp" {
		t.Fatalf("got (%q, %q, %t), want (%q, %q, true)", fileID, format, ok, "2026/abc-123", "webp")
	}
}

func TestCanonicalPair(t *testing.T) {
	first, second := canonicalPair(9, 3)
	if first != 3 || second != 9 {
		t.Fatalf("got (%d, %d), want (3, 9)", first, second)
	}
}

func TestVectorLiteral(t *testing.T) {
	if got, want := vectorLiteral([]float64{0.1, -2, 3.5}), "[0.1,-2,3.5]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
