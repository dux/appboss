package config

import "testing"

func TestNormalizeHost(t *testing.T) {
	for raw, want := range map[string]string{
		"Example.COM.":        "example.com",
		"example.com:8080":    "example.com",
		"[::1]:443":           "::1",
		"::1":                 "::1",
		"localhost":           "localhost",
		"api.example.com.:80": "api.example.com",
	} {
		if got := NormalizeHost(raw); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestBestMatchPrefersTheMostSpecificPattern(t *testing.T) {
	patterns := []string{".Example.com", "API.example.com."}
	exact, ok := BestMatch("api.example.com", patterns)
	if !ok {
		t.Fatal("api.example.com did not match")
	}
	wildcard, ok := BestMatch("www.example.com", patterns)
	if !ok {
		t.Fatal("www.example.com did not match the leading-dot pattern")
	}
	if exact <= wildcard {
		t.Fatalf("exact score %d should beat wildcard score %d", exact, wildcard)
	}
	if _, ok := BestMatch("example.org", patterns); ok {
		t.Fatal("example.org matched")
	}
	if _, ok := BestMatch("example.com", []string{"*.example.com"}); ok {
		t.Fatal("*. pattern matched the apex")
	}
}

func TestPrimaryHostPicksOneAddress(t *testing.T) {
	for _, test := range []struct {
		name      string
		canonical string
		hosts     []string
		want      string
	}{
		{"canonical wins", "b.example.com", []string{"a.example.com", "b.example.com"}, "b.example.com"},
		{"first concrete host", "", []string{"a.example.com", "b.example.com"}, "a.example.com"},
		{"leading dot matches its apex", "", []string{".sinatra.lvh.me"}, "sinatra.lvh.me"},
		{"concrete beats a pattern", "", []string{"*.example.com", "app.example.com"}, "app.example.com"},
		{"subdomains only has no apex", "", []string{"*.example.com"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PrimaryHost(test.canonical, test.hosts); got != test.want {
				t.Fatalf("PrimaryHost = %q, want %q", got, test.want)
			}
		})
	}
}
