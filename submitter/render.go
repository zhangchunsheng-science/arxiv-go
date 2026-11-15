package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

type Feed struct {
	ID           string  `xml:"id"`
	Title        string  `xml:"title"`
	Updated      string  `xml:"updated"`
	TotalResults int     `xml:"http://a9.com/-/spec/opensearch/1.1/ totalResults"`
	StartIndex   int     `xml:"http://a9.com/-/spec/opensearch/1.1/ startIndex"`
	ItemsPerPage int     `xml:"http://a9.com/-/spec/opensearch/1.1/ itemsPerPage"`
	Entries      []Entry `xml:"entry"`
}

type Entry struct {
	ID              string     `xml:"id"`
	Published       string     `xml:"published"`
	Updated         string     `xml:"updated"`
	Title           string     `xml:"title"`
	Summary         string     `xml:"summary"`
	Authors         []Author   `xml:"author"`
	DOI             string     `xml:"http://arxiv.org/schemas/atom doi"`
	Comment         string     `xml:"http://arxiv.org/schemas/atom comment"`
	JournalRef      string     `xml:"http://arxiv.org/schemas/atom journal_ref"`
	PrimaryCategory Category   `xml:"http://arxiv.org/schemas/atom primary_category"`
	Categories      []Category `xml:"category"`
	Links           []Link     `xml:"link"`
}

type Author struct {
	Name        string `xml:"name"`
	Affiliation string `xml:"http://arxiv.org/schemas/atom affiliation"`
}

type Category struct {
	Term   string `xml:"term,attr"`
	Scheme string `xml:"scheme,attr"`
}

type Link struct {
	Rel   string `xml:"rel,attr"`
	Href  string `xml:"href,attr"`
	Title string `xml:"title,attr"`
	Type  string `xml:"type,attr"`
}

type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("arxiv api error %s: %s", e.Code, e.Message)
	}
	return "arxiv api error: " + e.Message
}

func Decode(data []byte) (*Feed, error) {
	var f Feed
	if err := xml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("atom: decode: %w", err)
	}
	if len(f.Entries) == 1 && strings.Contains(f.Entries[0].ID, "/api/errors#") {
		e := f.Entries[0]
		code := e.ID
		if i := strings.IndexByte(code, '#'); i >= 0 {
			code = code[i+1:]
		}
		return nil, &APIError{Code: code, Message: strings.TrimSpace(e.Summary)}
	}
	for i := range f.Entries {
		e := &f.Entries[i]
		e.Title = strings.TrimSpace(e.Title)
		e.Summary = strings.TrimSpace(e.Summary)
		e.Comment = strings.TrimSpace(e.Comment)
		e.JournalRef = strings.TrimSpace(e.JournalRef)
	}
	return &f, nil
}

func decodeFile(path string) (*Feed, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(raw)
}

func paperID(e *Entry) string {
	id := e.ID
	if i := strings.Index(id, "arxiv.org/abs/"); i >= 0 {
		id = id[i+len("arxiv.org/abs/"):]
	}
	return id
}

type FeedResponse struct {
	Query        string      `json:"query,omitempty"`
	TotalResults int         `json:"total_results"`
	StartIndex   int         `json:"start_index"`
	ItemsPerPage int         `json:"items_per_page"`
	Entries      []EntryJSON `json:"entries"`
}

type EntryJSON struct {
	ID              string       `json:"id"`
	Title           string       `json:"title"`
	Summary         string       `json:"summary"`
	Authors         []AuthorJSON `json:"authors"`
	Published       string       `json:"published"`
	Updated         string       `json:"updated"`
	PrimaryCategory string       `json:"primary_category"`
	Categories      []string     `json:"categories"`
	Comment         string       `json:"comment,omitempty"`
	JournalRef      string       `json:"journal_ref,omitempty"`
	DOI             string       `json:"doi,omitempty"`
	AbsURL          string       `json:"abs_url"`
	PDFURL          string       `json:"pdf_url"`
	DOIURL          string       `json:"doi_url,omitempty"`
}

type AuthorJSON struct {
	Name        string `json:"name"`
	Affiliation string `json:"affiliation,omitempty"`
}

func ToEntryJSON(e *Entry) EntryJSON {
	authors := make([]AuthorJSON, len(e.Authors))
	for j, a := range e.Authors {
		authors[j] = AuthorJSON{Name: a.Name, Affiliation: a.Affiliation}
	}
	cats := make([]string, 0, len(e.Categories))
	for _, c := range e.Categories {
		cats = append(cats, c.Term)
	}
	absURL, pdfURL, doiURL := "", "", ""
	for _, l := range e.Links {
		if l.Rel == "alternate" && absURL == "" {
			absURL = l.Href
		}
		if l.Rel == "related" && l.Title == "pdf" && pdfURL == "" {
			pdfURL = l.Href
		}
		if l.Rel == "related" && l.Title == "doi" && doiURL == "" {
			doiURL = l.Href
		}
	}
	return EntryJSON{
		ID:              paperID(e),
		Title:           e.Title,
		Summary:         e.Summary,
		Authors:         authors,
		Published:       e.Published,
		Updated:         e.Updated,
		PrimaryCategory: e.PrimaryCategory.Term,
		Categories:      cats,
		Comment:         e.Comment,
		JournalRef:      e.JournalRef,
		DOI:             e.DOI,
		AbsURL:          absURL,
		PDFURL:          pdfURL,
		DOIURL:          doiURL,
	}
}

func ToFeedResponse(query string, f *Feed) FeedResponse {
	resp := FeedResponse{
		Query:        query,
		TotalResults: f.TotalResults,
		StartIndex:   f.StartIndex,
		ItemsPerPage: f.ItemsPerPage,
		Entries:      make([]EntryJSON, 0, len(f.Entries)),
	}
	for i := range f.Entries {
		resp.Entries = append(resp.Entries, ToEntryJSON(&f.Entries[i]))
	}
	return resp
}

func reorder(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			return append(flags, positional...)
		case len(a) > 1 && a[0] == '-':
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			hasValue := strings.ContainsRune(name, '=')
			if !hasValue && valueFlags[name] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i += 2
				continue
			}
			i++
		default:
			positional = append(positional, a)
			i++
		}
	}
	return append(flags, positional...)
}

func emitJSON(v any) int {
	enc := sonic.ConfigStd.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func printEntry(e *EntryJSON, showAbs bool) {
	date := strings.Repeat(" ", 10)
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(e.Published)); err == nil {
		date = t.Format("2006-01-02")
	}
	title := e.Title
	if r := []rune(title); len(r) > 90 {
		title = strings.TrimSpace(string(r[:89])) + "…"
	}
	fmt.Printf("%-22s %s  %s\n", e.ID, date, title)

	indent := strings.Repeat(" ", 24)
	names := make([]string, len(e.Authors))
	for i, a := range e.Authors {
		names[i] = a.Name
	}
	authorLine := strings.Join(names, "; ")
	if len(names) > 4 {
		authorLine = strings.Join(names[:4], "; ") + fmt.Sprintf("; +%d more", len(names)-4)
	}
	fmt.Printf("%s%s\n", indent, authorLine)
	if cats := strings.Join(e.Categories, " "); cats != "" {
		fmt.Printf("%s[%s]\n", indent, cats)
	}
	if showAbs && e.Summary != "" {
		words := strings.Fields(e.Summary)
		if len(words) > 0 {
			var lines []string
			line := words[0]
			for _, w := range words[1:] {
				if len(line)+1+len(w) > 96 {
					lines = append(lines, line)
					line = w
					continue
				}
				line += " " + w
			}
			lines = append(lines, line)
			for _, l := range lines {
				fmt.Printf("%s  %s\n", indent, l)
			}
		}
	}
	fmt.Println()
}
