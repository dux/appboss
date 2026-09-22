package release

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLatestReadsTagName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"tag_name": "v84"}`)
	}))
	defer server.Close()

	previous := APIBase
	APIBase = server.URL
	defer func() { APIBase = previous }()

	tag, err := Latest(context.Background())
	if err != nil || tag != "v84" {
		t.Fatalf("Latest = %q, %v; want v84", tag, err)
	}
}

// A repository without a release is a plain answer, not a bare 404.
func TestLatestNamesAnUnreleasedRepo(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	previous := APIBase
	APIBase = server.URL
	defer func() { APIBase = previous }()

	if _, err := Latest(context.Background()); err == nil || err.Error() != Repo+" has no published release yet" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNumber(t *testing.T) {
	for _, item := range []struct {
		tag    string
		number int
		ok     bool
	}{
		{"v81", 81, true},
		{"v0", 0, true},
		{"dev", 0, false},
		{"81", 0, false},
		{"v0.1.0", 0, false},
		{"", 0, false},
	} {
		number, ok := Number(item.tag)
		if number != item.number || ok != item.ok {
			t.Errorf("Number(%q) = %d, %v; want %d, %v", item.tag, number, ok, item.number, item.ok)
		}
	}
}

func TestURLs(t *testing.T) {
	if got := TagURL("v84"); got != DownloadBase+"/"+Repo+"/releases/tag/v84" {
		t.Errorf("TagURL = %q", got)
	}
	if got := AssetURL("v84", "checksums.txt"); got != DownloadBase+"/"+Repo+"/releases/download/v84/checksums.txt" {
		t.Errorf("AssetURL = %q", got)
	}
}
