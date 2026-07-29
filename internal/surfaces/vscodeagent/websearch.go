package vscodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var webSearchEndpoint = "https://api.duckduckgo.com/"

type webSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

type duckDuckGoResponse struct {
	Heading       string                  `json:"Heading"`
	AbstractText  string                  `json:"AbstractText"`
	AbstractURL   string                  `json:"AbstractURL"`
	RelatedTopics []duckDuckGoRelatedItem `json:"RelatedTopics"`
	Results       []duckDuckGoRelatedItem `json:"Results"`
}

type duckDuckGoRelatedItem struct {
	Text        string                  `json:"Text"`
	FirstURL    string                  `json:"FirstURL"`
	Result      string                  `json:"Result"`
	Name        string                  `json:"Name"`
	Topics      []duckDuckGoRelatedItem `json:"Topics"`
	Icon        map[string]any          `json:"Icon"`
	Description string                  `json:"Description"`
}

func (s *Server) commandWebSearch(ctx context.Context, args string) (string, error) {
	query := strings.TrimSpace(args)
	if query == "" {
		return "", fmt.Errorf("usage: /websearch <query>")
	}
	results, err := webSearch(ctx, query, 6)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return fmt.Sprintf("No web search results for %q.", query), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q:\n", query)
	for i, result := range results {
		title := strings.TrimSpace(result.Title)
		if title == "" {
			title = result.URL
		}
		if result.URL != "" {
			fmt.Fprintf(&b, "%d. [%s](%s)\n", i+1, title, result.URL)
		} else {
			fmt.Fprintf(&b, "%d. %s\n", i+1, title)
		}
		if strings.TrimSpace(result.Snippet) != "" {
			fmt.Fprintf(&b, "   %s\n", strings.TrimSpace(result.Snippet))
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func webSearch(ctx context.Context, query string, limit int) ([]webSearchResult, error) {
	endpoint, err := url.Parse(webSearchEndpoint)
	if err != nil {
		return nil, fmt.Errorf("websearch endpoint: %w", err)
	}
	q := endpoint.Query()
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("no_html", "1")
	q.Set("skip_disambig", "1")
	endpoint.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("websearch request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "contenox-vscode-agent/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("websearch request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("websearch returned HTTP %d", resp.StatusCode)
	}

	var decoded duckDuckGoResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("websearch decode: %w", err)
	}
	return collectWebSearchResults(decoded, limit), nil
}

func collectWebSearchResults(decoded duckDuckGoResponse, limit int) []webSearchResult {
	if limit <= 0 {
		limit = 6
	}
	out := make([]webSearchResult, 0, limit)
	seen := map[string]struct{}{}
	add := func(result webSearchResult) {
		if len(out) >= limit {
			return
		}
		result.Title = compactSpace(stripHTML(result.Title))
		result.Snippet = compactSpace(stripHTML(result.Snippet))
		result.URL = strings.TrimSpace(result.URL)
		key := result.URL
		if key == "" {
			key = result.Title + "\n" + result.Snippet
		}
		if strings.TrimSpace(key) == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, result)
	}
	if decoded.AbstractText != "" || decoded.AbstractURL != "" {
		add(webSearchResult{
			Title:   decoded.Heading,
			URL:     decoded.AbstractURL,
			Snippet: decoded.AbstractText,
		})
	}
	var walk func(items []duckDuckGoRelatedItem)
	walk = func(items []duckDuckGoRelatedItem) {
		for _, item := range items {
			if len(out) >= limit {
				return
			}
			if len(item.Topics) > 0 {
				walk(item.Topics)
				continue
			}
			add(webSearchResult{
				Title:   titleFromRelatedItem(item),
				URL:     item.FirstURL,
				Snippet: item.Text,
			})
		}
	}
	walk(decoded.Results)
	walk(decoded.RelatedTopics)
	return out
}

func titleFromRelatedItem(item duckDuckGoRelatedItem) string {
	if item.Name != "" {
		return item.Name
	}
	text := stripHTML(item.Text)
	if i := strings.Index(text, " - "); i > 0 {
		return text[:i]
	}
	if len(text) > 80 {
		return text[:80] + "..."
	}
	return text
}

func compactSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func stripHTML(value string) string {
	value = strings.ReplaceAll(value, "<b>", "")
	value = strings.ReplaceAll(value, "</b>", "")
	value = strings.ReplaceAll(value, "<br>", " ")
	value = strings.ReplaceAll(value, "<br/>", " ")
	value = strings.ReplaceAll(value, "<br />", " ")
	return value
}
