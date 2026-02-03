package main

import "testing"

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in   string
		out  string
		ok   bool
	}{
		{"https://example.org", "https://example.org/", true},
		{"https://example.org/", "https://example.org/", true},
		{"https://example.org/path", "https://example.org/path", true},
		{"http://example.org", "http://example.org/", true},
		{"not-a-url", "", false},
		{"https://", "", false},
	}

	for _, c := range cases {
		got, ok := normalizeURL(c.in)
		if ok != c.ok {
			t.Fatalf("normalizeURL(%q) ok=%v, want %v", c.in, ok, c.ok)
		}
		if ok && got != c.out {
			t.Fatalf("normalizeURL(%q)=%q, want %q", c.in, got, c.out)
		}
	}
}

func TestParseURLList(t *testing.T) {
	input := "" +
		"# comment\n" +
		"https://example.org\n" +
		"https://example.org/path\n" +
		"  https://example.org/space  # trailing comment\n" +
		"ftp://example.org\n" +
		"not-a-url\n" +
		"\n"

	got := parseURLList(input)
	want := []string{
		"https://example.org/",
		"https://example.org/path",
		"https://example.org/space",
	}
	if len(got) != len(want) {
		t.Fatalf("parseURLList count=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseURLList[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractDirective(t *testing.T) {
	policy := "default-src 'self'; img-src https://img.example.org data:; connect-src 'self' https://api.example.org;"

	img := extractDirective(policy, "img-src")
	if img != "img-src https://img.example.org data:" {
		t.Fatalf("img-src directive=%q", img)
	}

	conn := extractDirective(policy, "connect-src")
	if conn != "connect-src 'self' https://api.example.org" {
		t.Fatalf("connect-src directive=%q", conn)
	}

	missing := extractDirective(policy, "script-src")
	if missing != "" {
		t.Fatalf("script-src directive=%q, want empty", missing)
	}
}
