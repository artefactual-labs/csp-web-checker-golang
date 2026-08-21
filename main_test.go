package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in  string
		out string
		ok  bool
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

func TestAppliedDirectiveFallbacks(t *testing.T) {
	policy := "default-src 'self'; script-src 'self' 'nonce-abc'; style-src 'self'; child-src https://frames.example.org;"

	scriptElem := appliedDirective(policy, "script-src-elem")
	if scriptElem != "script-src 'self' 'nonce-abc'" {
		t.Fatalf("script-src-elem applied directive=%q", scriptElem)
	}

	styleAttr := appliedDirective(policy, "style-src-attr")
	if styleAttr != "style-src 'self'" {
		t.Fatalf("style-src-attr applied directive=%q", styleAttr)
	}

	frame := appliedDirective(policy, "frame-src")
	if frame != "child-src https://frames.example.org" {
		t.Fatalf("frame-src applied directive=%q", frame)
	}

	img := appliedDirective(policy, "img-src")
	if img != "default-src 'self'" {
		t.Fatalf("img-src applied directive=%q", img)
	}
}

func TestGroupHintInlineScript(t *testing.T) {
	line := 361
	g := GroupedViolation{
		EffectiveDirective: "script-src-elem",
		BlockedOrigin:      "inline",
		Pages: map[string][]Violation{
			"https://example.org/page": {
				{
					SourceFile: "https://example.org/page",
					LineNumber: &line,
				},
			},
		},
	}
	hint := groupHint(g)
	if !strings.Contains(hint, "Open the snippet from Source") {
		t.Fatalf("inline script hint=%q", hint)
	}
}

func TestParseConfigBasicAuth(t *testing.T) {
	cfg, err := parseConfig(`{"basicAuthUsername":"  user  ","basicAuthPassword":"secret"}`)
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if cfg.BasicAuthUsername != "user" {
		t.Fatalf("BasicAuthUsername=%q, want user", cfg.BasicAuthUsername)
	}
	if cfg.BasicAuthPassword != "secret" {
		t.Fatalf("BasicAuthPassword=%q, want secret", cfg.BasicAuthPassword)
	}
	if cfg.UserAgent == "" || cfg.AcceptLanguage == "" {
		t.Fatalf("defaults were not preserved")
	}
}

func TestApplyBasicAuthFromFormOverridesProfileAuth(t *testing.T) {
	cfg := defaultConfig()
	cfg.BasicAuthUsername = "profile-user"
	cfg.BasicAuthPassword = "profile-pass"

	r := httptest.NewRequest("POST", "/runs", strings.NewReader("basic_auth_username=&basic_auth_password="))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm returned error: %v", err)
	}
	applyBasicAuthFromForm(r, &cfg)
	if cfg.BasicAuthUsername != "" || cfg.BasicAuthPassword != "" {
		t.Fatalf("blank run auth should disable auth, got username=%q password=%q", cfg.BasicAuthUsername, cfg.BasicAuthPassword)
	}

	r = httptest.NewRequest("POST", "/runs", strings.NewReader("basic_auth_username=run-user&basic_auth_password=run-pass"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm returned error: %v", err)
	}
	applyBasicAuthFromForm(r, &cfg)
	if cfg.BasicAuthUsername != "run-user" || cfg.BasicAuthPassword != "run-pass" {
		t.Fatalf("run auth not applied, got username=%q password=%q", cfg.BasicAuthUsername, cfg.BasicAuthPassword)
	}
}
