// Package fetch turns a blog URL into clean, speakable text chunks.
package fetch

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"sevi/internal/httpx"
)

// Markdown uses Jina Reader: GET <baseURL>/<url> returns the page as markdown.
// key may be empty (Jina allows keyless use at lower rate limits).
func Markdown(ctx context.Context, baseURL, key, pageURL string) (string, error) {
	req, err := http.NewRequest("GET", strings.TrimRight(baseURL, "/")+"/"+pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	body, _, err := httpx.Do(ctx, req)
	return string(body), err
}

var reSlug = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// Slug makes a short filesystem-safe name from a URL.
func Slug(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "page"
	}
	s := strings.Trim(reSlug.ReplaceAllString(p.Host+p.Path, "-"), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		return "page"
	}
	return s
}
