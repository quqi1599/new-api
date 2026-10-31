package websearch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

var markdownLink = regexp.MustCompile(`\[[^\]]*\]\((https?://[^\s)]+)\)`)

func Alpha(ctx context.Context, s *Session, body []byte) (Object, error) {
	root := gjson.ParseBytes(body)
	commands := root.Get("commands")
	if !commands.IsObject() {
		return nil, errors.New("Codex search requires a commands object")
	}
	p := &Plan{Limit: MaxCalls}
	if settings := root.Get("settings"); settings.Exists() && settings.Type != gjson.Null {
		if err := p.settings(settings); err != nil {
			return nil, err
		}
	}
	count := 0
	for name, value := range commands.Map() {
		if name == "response_length" {
			if value.Type != gjson.String {
				return nil, errors.New("invalid response_length")
			}
			continue
		}
		switch name {
		case "search_query", "open", "find", "click":
		default:
			if value.Type != gjson.Null {
				return nil, fmt.Errorf("DeepSeek supports search_query/open/find/click; %s is unavailable", name)
			}
			continue
		}
		if value.Type == gjson.Null {
			continue
		}
		if !value.IsArray() {
			return nil, errors.New("web commands must be arrays")
		}
		count += len(value.Array())
		if count > MaxCalls {
			return nil, errors.New("too many web commands")
		}
		for _, op := range value.Array() {
			if !op.IsObject() {
				return nil, errors.New("invalid web command")
			}
			for key := range op.Map() {
				allowed := "ref_id lineno"
				if name == "search_query" {
					allowed = "q recency domains"
				}
				if name == "find" {
					allowed = "ref_id pattern"
				}
				if name == "click" {
					allowed = "ref_id id"
				}
				if !strings.Contains(" "+allowed+" ", " "+key+" ") {
					return nil, fmt.Errorf("unsupported %s field %s", name, key)
				}
			}
		}
	}
	if count == 0 {
		return nil, errors.New("web commands are empty")
	}
	var output strings.Builder
	var results []any
	for _, q := range commands.Get("search_query").Array() {
		query := q.Get("q").String()
		allowed := append([]string(nil), p.Allowed...)
		for _, d := range q.Get("domains").Array() {
			if !validDomain(d.String()) {
				return nil, errors.New("invalid search domain")
			}
			allowed = append(allowed, d.String())
		}
		if len(allowed) > 0 {
			query += " site:" + strings.Join(allowed, " OR site:")
		}
		for _, d := range p.Blocked {
			query += " -site:" + d
		}
		if days := q.Get("recency"); days.Exists() && days.Type != gjson.Null {
			if days.Type != gjson.Number || days.Int() < 0 || days.Int() > 36500 || days.Float() != float64(days.Int()) {
				return nil, errors.New("invalid search recency")
			}
			query += " after:" + time.Now().UTC().AddDate(0, 0, -int(days.Int())).Format("2006-01-02")
		}
		hits, err := s.Search(ctx, SearchRequest{Query: query, Count: 10})
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			filtered := make([]Hit, 0, len(hits))
			for _, hit := range hits {
				if allowedToolURL(hit.URL, allowed, p.Blocked) {
					filtered = append(filtered, hit)
				}
			}
			if len(filtered) == 0 {
				s.Meter.Search--
			}
			hits = filtered
		}
		for _, hit := range hits {
			id := NewID("turn_na_search_")
			snippet := hit.Summary
			if snippet == "" {
				snippet = hit.Snippet
			}
			if err := s.Store.Put(ctx, id, Record{URL: hit.URL, Title: hit.Title}); err != nil {
				return nil, err
			}
			results = append(results, map[string]any{"type": "text_result", "ref_id": id, "url": hit.URL, "title": hit.Title, "snippet": snippet})
			fmt.Fprintf(&output, "%s (%s)\n[%s] %s\n\n", hit.Title, hit.URL, id, snippet)
		}
		if len(hits) == 0 {
			fmt.Fprintf(&output, "No search results for %q.\n", q.Get("q").String())
		}
	}
	for _, kind := range []string{"open", "find", "click"} {
		for _, op := range commands.Get(kind).Array() {
			ref := op.Get("ref_id").String()
			record := Record{URL: ref}
			if !PublicURL(ref) {
				var err error
				record, err = s.Store.Get(ctx, ref)
				if err != nil {
					return nil, err
				}
			}
			if !allowedToolURL(record.URL, p.Allowed, p.Blocked) {
				return nil, errors.New("fetch URL is not allowed")
			}
			if record.Content == "" {
				page, err := s.Fetch(ctx, record.URL)
				if err != nil {
					return nil, err
				}
				record.Title, record.Content = page.Title, page.Content
			}
			if kind == "click" {
				links := markdownLink.FindAllStringSubmatch(record.Content, -1)
				id := op.Get("id")
				if id.Type != gjson.Number || id.Int() < 0 || id.Int() >= int64(len(links)) || id.Float() != float64(id.Int()) {
					return nil, errors.New("click link ID is unavailable")
				}
				record.URL = links[id.Int()][1]
				if !allowedToolURL(record.URL, p.Allowed, p.Blocked) {
					return nil, errors.New("fetch URL is not allowed")
				}
				page, err := s.Fetch(ctx, record.URL)
				if err != nil {
					return nil, err
				}
				record.Title, record.Content = page.Title, page.Content
			}
			id := NewID("turn_na_view_")
			if err := s.Store.Put(ctx, id, record); err != nil {
				return nil, err
			}
			lines := strings.Split(record.Content, "\n")
			start := int(op.Get("lineno").Int())
			if start < 0 {
				start = 0
			}
			if start > len(lines) {
				start = len(lines)
			}
			if kind == "find" {
				pattern := op.Get("pattern").String()
				if pattern == "" {
					return nil, errors.New("find requires a pattern")
				}
				matched := -1
				for i, line := range lines {
					if strings.Contains(strings.ToLower(line), strings.ToLower(pattern)) {
						matched = i
						break
					}
				}
				if matched < 0 {
					fmt.Fprintf(&output, "[%s] Pattern %q was not found in %s\n", id, pattern, record.URL)
					continue
				}
				start = max(0, matched-3)
			}
			fmt.Fprintf(&output, "%s (%s)\n[%s] Total lines: %d\n", record.Title, record.URL, id, len(lines))
			for i := start; i < min(len(lines), start+160); i++ {
				fmt.Fprintf(&output, "L%d: %s\n", i, lines[i])
			}
			for i, m := range markdownLink.FindAllStringSubmatch(record.Content, -1) {
				if i >= 100 {
					break
				}
				fmt.Fprintf(&output, "Link %d: %s\n", i, m[1])
			}
			results = append(results, map[string]any{"type": "text_result", "ref_id": id, "url": record.URL, "title": record.Title, "snippet": fmt.Sprintf("Total lines: %d", len(lines))})
		}
	}
	limit := 40000
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() && maxTokens.Int() > 0 {
		limit = min(limit, int(min(maxTokens.Int(), 10000))*4)
	}
	text := []rune(output.String())
	if len(text) > limit {
		text = text[:limit]
		text = append(text, []rune("\n[Output truncated; use open with a line offset.]\n")...)
	}
	return Object{"output": Raw(string(text)), "results": Raw(results), "usage": Raw(map[string]any{"server_tool_use": s.Meter})}, nil
}
