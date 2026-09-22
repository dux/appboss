// Package release is the GitHub release source: which tag is published as the latest, where its
// assets live and how two tags compare. It is the one place that names the repository, so
// `dboss update` and the console's Sys tab cannot disagree about what "the latest release" is.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The release source. These are variables so a test can point the whole flow at a local server
// instead of GitHub.
var (
	Repo         = "dux/dboss"
	APIBase      = "https://api.github.com"
	DownloadBase = "https://github.com"
)

// Client is used for every release request. The timeout covers the whole response body, which
// for an install is a ~20 MB binary on whatever link the box has; a caller that needs a shorter
// deadline passes a context.
var Client = &http.Client{Timeout: 5 * time.Minute}

// Latest returns the newest published release tag.
func Latest(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", APIBase, Repo)
	response, err := Get(ctx, url)
	if err != nil {
		var status StatusError
		if errors.As(err, &status) && status.Code == http.StatusNotFound {
			return "", fmt.Errorf("%s has no published release yet", Repo)
		}
		return "", err
	}
	defer response.Body.Close()
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("read the latest release of %s: %w", Repo, err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("%s has no published release", Repo)
	}
	return release.TagName, nil
}

// AssetURL is the download address of one release asset.
func AssetURL(tag, asset string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", DownloadBase, Repo, tag, asset)
}

// TagURL is the release page of one tag, so the console can link a version.
func TagURL(tag string) string {
	return fmt.Sprintf("%s/%s/releases/tag/%s", DownloadBase, Repo, tag)
}

// Get fails on anything but 200, so a GitHub error page never lands on disk as a binary.
func Get(ctx context.Context, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := Client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, StatusError{URL: url, Status: response.Status, Code: response.StatusCode}
	}
	return response, nil
}

// StatusError keeps the code, so a missing release reads as one rather than as a bare 404.
type StatusError struct {
	URL    string
	Status string
	Code   int
}

func (e StatusError) Error() string { return fmt.Sprintf("GET %s: %s", e.URL, e.Status) }

// Number turns a v<count> tag into its number. Anything else, "dev" included, is not comparable
// and never blocks an install.
func Number(tag string) (int, bool) {
	number, err := strconv.Atoi(strings.TrimPrefix(tag, "v"))
	if err != nil || !strings.HasPrefix(tag, "v") {
		return 0, false
	}
	return number, true
}
