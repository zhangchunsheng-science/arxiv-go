package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/zhangchunsheng-science/arxiv-go/taskdb"
)

const (
	maxResultsPerRequest = 2000
	maxTotalResults      = 30000
)

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	baseURL    = envDefault("ARXIV_BASE_URL", "https://export.arxiv.org/api/query")
	pdfBaseURL = envDefault("ARXIV_PDF_BASE", "https://arxiv.org/pdf")
)

var (
	newIDRe   = regexp.MustCompile(`^\d{4}\.\d{4,5}(v\d+)?$`)
	oldIDRe   = regexp.MustCompile(`^[a-z-]+(\.[A-Z]{2})?/\d{7}(v\d+)?$`)
	versionRe = regexp.MustCompile(`v\d+$`)
)

func validID(id string) bool {
	if newIDRe.MatchString(id) {
		mm, _ := strconv.Atoi(id[2:4])
		return mm >= 1 && mm <= 12
	}
	if oldIDRe.MatchString(id) {
		i := strings.IndexByte(id, '/')
		mm, _ := strconv.Atoi(id[i+3 : i+5])
		return mm >= 1 && mm <= 12
	}
	return false
}

func buildURL(searchQuery string, ids []string, start, max int, sortBy, sortOrder string) (string, error) {
	searchQuery = strings.TrimSpace(searchQuery)
	if max <= 0 {
		max = 10
	}
	if sortBy == "" {
		sortBy = "relevance"
	}
	if sortOrder == "" {
		sortOrder = "descending"
	}
	if ids == nil {
		ids = []string{}
	}
	if searchQuery == "" && len(ids) == 0 {
		return "", fmt.Errorf("empty query: need search terms or ids")
	}
	if start < 0 {
		return "", fmt.Errorf("start must be >= 0")
	}
	if max > maxResultsPerRequest {
		return "", fmt.Errorf("max %d exceeds per-request cap %d", max, maxResultsPerRequest)
	}
	if start >= maxTotalResults {
		return "", fmt.Errorf("start %d exceeds cumulative cap %d", start, maxTotalResults)
	}
	switch sortBy {
	case "relevance", "lastUpdatedDate", "submittedDate":
	default:
		return "", fmt.Errorf("sortBy must be one of relevance, lastUpdatedDate, submittedDate")
	}
	if sortOrder != "ascending" && sortOrder != "descending" {
		return "", fmt.Errorf("sortOrder must be ascending or descending")
	}
	for _, id := range ids {
		if !validID(id) {
			return "", fmt.Errorf("malformed arXiv id %q", id)
		}
	}
	v := url.Values{}
	v.Set("search_query", searchQuery)
	v.Set("id_list", strings.Join(ids, ","))
	v.Set("start", strconv.Itoa(start))
	v.Set("max_results", strconv.Itoa(max))
	v.Set("sortBy", sortBy)
	v.Set("sortOrder", sortOrder)
	return baseURL + "?" + v.Encode(), nil
}

func await(db *gorm.DB, id int64, deadline time.Time) (*taskdb.Task, error) {
	for {
		t, err := taskdb.Get(db, id)
		if err != nil {
			return nil, err
		}
		if t.Status == taskdb.StatusDone || t.Status == taskdb.StatusFailed {
			return t, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for task %d (status %s)", id, t.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, `submitter — write tasks into the queue DB and poll until done

Usage:
  submitter [flags] <command> [args]

Commands:
  search <query> [-flags]    Search arXiv (ti:/au:/cat:/date filters).
  get <id> [id...]           Fetch metadata by arXiv id.
  download <id> [id...]      Download PDF for id (version pinned via metadata).
  daily <YYYY-MM-DD>         Queue PDFs for all papers submitted that day.

Flags:
`)
	fs.PrintDefaults()
}

func main() {
	var (
		dbPath string
		wait   time.Duration
		ttl    time.Duration
		noWait bool
	)
	fs := flag.NewFlagSet("submitter", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&dbPath, "db", "", "sqlite path (default: binary dir/queue.sqlite)")
	fs.DurationVar(&wait, "timeout", 10*time.Minute, "per-task wait timeout")
	fs.DurationVar(&ttl, "ttl", 5*time.Minute, "search result freshness window")
	fs.BoolVar(&noWait, "no-wait", false, "submit tasks and exit without waiting for results")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	args := fs.Args()
	if len(args) == 0 {
		usage(fs)
		os.Exit(2)
	}
	if dbPath == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		dbPath = filepath.Join(filepath.Dir(exe), "queue.sqlite")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	db, err := taskdb.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer func() {
		if sqlDB, e := db.DB(); e == nil {
			sqlDB.Close()
		}
	}()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "search":
		rest = reorder(rest, map[string]bool{
			"max": true, "start": true, "sort-by": true, "sort-order": true,
			"cat": true, "author": true, "title": true, "from": true, "to": true,
		})
		sfs := flag.NewFlagSet("search", flag.ContinueOnError)
		sfs.SetOutput(os.Stderr)
		var (
			max       int
			start     int
			sortBy    string
			sortOrder string
			cat       string
			author    string
			title     string
			from      string
			to        string
			jsonOut   bool
			showAbs   bool
			noWaitCmd bool
		)
		sfs.IntVar(&max, "max", 10, "max results (<= 2000 per request)")
		sfs.IntVar(&start, "start", 0, "0-based offset")
		sfs.StringVar(&sortBy, "sort-by", "relevance", "relevance|lastUpdatedDate|submittedDate")
		sfs.StringVar(&sortOrder, "sort-order", "descending", "ascending|descending")
		sfs.StringVar(&cat, "cat", "", "AND a category filter, e.g. cs.LG")
		sfs.StringVar(&author, "author", "", "AND an author term, e.g. del_maestro")
		sfs.StringVar(&title, "title", "", "AND a title term (quoted phrase)")
		sfs.StringVar(&from, "from", "", "submitted on/after this date, YYYY-MM-DD")
		sfs.StringVar(&to, "to", "", "submitted on/before this date, YYYY-MM-DD")
		sfs.BoolVar(&jsonOut, "json", false, "JSON output")
		sfs.BoolVar(&showAbs, "abs", false, "print abstracts in table mode")
		sfs.BoolVar(&noWaitCmd, "no-wait", false, "submit and exit without waiting")
		if err := sfs.Parse(rest); err != nil {
			os.Exit(2)
		}

		parts := []string{strings.Join(sfs.Args(), " ")}
		if title != "" {
			parts = append(parts, `ti:"`+title+`"`)
		}
		if author != "" {
			parts = append(parts, "au:"+author)
		}
		if cat != "" {
			parts = append(parts, "cat:"+cat)
		}
		if from != "" || to != "" {
			parseDate := func(s, name string) (time.Time, error) {
				if s == "" {
					return time.Now(), nil
				}
				t, err := time.ParseInLocation("2006-01-02", s, time.Local)
				if err != nil {
					return time.Time{}, fmt.Errorf("-%s must be YYYY-MM-DD", name)
				}
				return t, nil
			}
			f, err := parseDate(from, "from")
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			t, err := parseDate(to, "to")
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			if from == "" {
				f = t.AddDate(-20, 0, 0)
			}
			if to == "" {
				t = time.Now()
			}
			fy, fm, fd := f.Date()
			fb := time.Date(fy, fm, fd, 0, 0, 0, 0, f.Location())
			ty, tm, td := t.Date()
			tb := time.Date(ty, tm, td, 0, 0, 0, 0, t.Location()).Add(24*time.Hour - time.Minute)
			parts = append(parts, fmt.Sprintf("submittedDate:[%s TO %s]", fb.UTC().Format("200601021504"), tb.UTC().Format("200601021504")))
		}
		var kept []string
		for _, p := range parts {
			if strings.TrimSpace(p) != "" {
				kept = append(kept, strings.TrimSpace(p))
			}
		}
		sq := strings.Join(kept, " AND ")
		if sq == "" {
			fmt.Fprintln(os.Stderr, "error: empty search query")
			os.Exit(1)
		}

		rawURL, err := buildURL(sq, nil, start, max, sortBy, sortOrder)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		id, err := taskdb.Submit(db, taskdb.KindSearch, rawURL, sq, time.Now().Add(ttl).Unix())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if noWait || noWaitCmd {
			fmt.Printf("task %d\n", id)
			os.Exit(0)
		}
		t, err := await(db, id, time.Now().Add(wait))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if t.Status == taskdb.StatusFailed {
			fmt.Fprintln(os.Stderr, "error: task failed:", t.LastError)
			os.Exit(1)
		}
		feed, err := decodeFile(t.ResultPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if jsonOut {
			os.Exit(emitJSON(ToFeedResponse(sq, feed)))
		}
		fmt.Fprintf(os.Stderr, "%d results (showing %d)\n", feed.TotalResults, len(feed.Entries))
		for i := range feed.Entries {
			e := ToEntryJSON(&feed.Entries[i])
			printEntry(&e, showAbs)
		}
	case "get":
		rest = reorder(rest, nil)
		gfs := flag.NewFlagSet("get", flag.ContinueOnError)
		gfs.SetOutput(os.Stderr)
		var (
			jsonOut   bool
			showAbs   bool
			noWaitCmd bool
		)
		gfs.BoolVar(&jsonOut, "json", false, "JSON output")
		gfs.BoolVar(&showAbs, "abs", false, "print abstracts in table mode")
		gfs.BoolVar(&noWaitCmd, "no-wait", false, "submit and exit without waiting")
		if err := gfs.Parse(rest); err != nil {
			os.Exit(2)
		}
		ids := gfs.Args()
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "error: provide at least one arXiv id")
			os.Exit(1)
		}
		rawURL, err := buildURL("", ids, 0, len(ids), "", "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		id, err := taskdb.Submit(db, taskdb.KindSearch, rawURL, strings.Join(ids, ","), time.Now().Add(24*time.Hour).Unix())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if noWait || noWaitCmd {
			fmt.Printf("task %d\n", id)
			os.Exit(0)
		}
		t, err := await(db, id, time.Now().Add(wait))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if t.Status == taskdb.StatusFailed {
			fmt.Fprintln(os.Stderr, "error: task failed:", t.LastError)
			os.Exit(1)
		}
		feed, err := decodeFile(t.ResultPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if jsonOut {
			os.Exit(emitJSON(ToFeedResponse("", feed)))
		}
		for i := range feed.Entries {
			e := ToEntryJSON(&feed.Entries[i])
			printEntry(&e, showAbs)
		}
	case "download":
		rest = reorder(rest, nil)
		dfs := flag.NewFlagSet("download", flag.ContinueOnError)
		dfs.SetOutput(os.Stderr)
		var noWaitCmd bool
		dfs.BoolVar(&noWaitCmd, "no-wait", false, "submit and exit without waiting")
		if err := dfs.Parse(rest); err != nil {
			os.Exit(2)
		}
		ids := dfs.Args()
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "error: provide at least one arXiv id")
			os.Exit(1)
		}
		for _, arg := range ids {
			if !validID(arg) {
				fmt.Fprintf(os.Stderr, "error: malformed arXiv id %q\n", arg)
				os.Exit(1)
			}
			paper := arg
			if !versionRe.MatchString(paper) {
				mURL, err := buildURL("", []string{arg}, 0, 1, "", "")
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
					os.Exit(1)
				}
				mid, err := taskdb.Submit(db, taskdb.KindSearch, mURL, arg, time.Now().Add(24*time.Hour).Unix())
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
					os.Exit(1)
				}
				mt, err := await(db, mid, time.Now().Add(wait))
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
					os.Exit(1)
				}
				if mt.Status == taskdb.StatusFailed {
					fmt.Fprintln(os.Stderr, "error: metadata fetch failed:", mt.LastError)
					os.Exit(1)
				}
				mfeed, err := decodeFile(mt.ResultPath)
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
					os.Exit(1)
				}
				if len(mfeed.Entries) == 0 {
					fmt.Fprintf(os.Stderr, "error: no metadata found for %s\n", arg)
					os.Exit(1)
				}
				paper = paperID(&mfeed.Entries[0])
				if !versionRe.MatchString(paper) {
					fmt.Fprintf(os.Stderr, "error: metadata for %s carries no version\n", arg)
					os.Exit(1)
				}
			}
			did, err := taskdb.Submit(db, taskdb.KindDownload, pdfBaseURL+"/"+paper, paper, 1<<62)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			if noWait || noWaitCmd {
				fmt.Printf("%s task %d\n", paper, did)
				continue
			}
			dt, err := await(db, did, time.Now().Add(wait))
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			if dt.Status == taskdb.StatusFailed {
				fmt.Fprintf(os.Stderr, "error: download %s failed: %s\n", paper, dt.LastError)
				os.Exit(1)
			}
			fmt.Println(dt.ResultPath)
			fmt.Fprintf(os.Stderr, "%s %d bytes\n", paper, dt.BytesTotal)
		}
	case "daily":
		yfs := flag.NewFlagSet("daily", flag.ContinueOnError)
		yfs.SetOutput(os.Stderr)
		var noWaitCmd bool
		yfs.BoolVar(&noWaitCmd, "no-wait", false, "queue downloads and exit without waiting")
		if err := yfs.Parse(rest); err != nil {
			os.Exit(2)
		}
		dayArgs := yfs.Args()
		if len(dayArgs) != 1 {
			fmt.Fprintln(os.Stderr, "error: usage: daily <YYYY-MM-DD>")
			os.Exit(2)
		}
		day, err := time.ParseInLocation("2006-01-02", dayArgs[0], time.Local)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: date must be YYYY-MM-DD")
			os.Exit(2)
		}
		y, m, d := day.Date()
		fb := time.Date(y, m, d, 0, 0, 0, 0, day.Location())
		tb := fb.Add(24*time.Hour - time.Minute)
		q := fmt.Sprintf("submittedDate:[%s TO %s]", fb.UTC().Format("200601021504"), tb.UTC().Format("200601021504"))
		rawURL, err := buildURL(q, nil, 0, maxResultsPerRequest, "submittedDate", "ascending")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		sid, err := taskdb.Submit(db, taskdb.KindSearch, rawURL, q, time.Now().Add(ttl).Unix())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		st, err := await(db, sid, time.Now().Add(wait))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if st.Status == taskdb.StatusFailed {
			fmt.Fprintln(os.Stderr, "error: task failed:", st.LastError)
			os.Exit(1)
		}
		feed, err := decodeFile(st.ResultPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if feed.TotalResults > len(feed.Entries) {
			fmt.Fprintf(os.Stderr, "warning: %d results exceed single-request cap, only %d queued\n", feed.TotalResults, len(feed.Entries))
		}
		fmt.Fprintf(os.Stderr, "%d papers submitted on %s\n", len(feed.Entries), dayArgs[0])
		if len(feed.Entries) == 0 {
			os.Exit(0)
		}
		type pending struct {
			paper string
			id    int64
		}
		var dls []pending
		for i := range feed.Entries {
			paper := paperID(&feed.Entries[i])
			if !versionRe.MatchString(paper) {
				fmt.Fprintf(os.Stderr, "skip %s: metadata carries no version\n", paper)
				continue
			}
			did, err := taskdb.Submit(db, taskdb.KindDownload, pdfBaseURL+"/"+paper, paper, 1<<62)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			dls = append(dls, pending{paper: paper, id: did})
		}
		if noWait || noWaitCmd {
			fmt.Fprintf(os.Stderr, "queued %d downloads\n", len(dls))
			os.Exit(0)
		}
		deadline := time.Now().Add(wait + time.Duration(len(dls))*4*time.Second)
		failed := 0
		for i, x := range dls {
			t, err := await(db, x.id, deadline)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			if t.Status == taskdb.StatusFailed {
				failed++
				fmt.Fprintf(os.Stderr, "[%d/%d] %s FAILED: %s\n", i+1, len(dls), x.paper, t.LastError)
				continue
			}
			fmt.Fprintf(os.Stderr, "[%d/%d] %s %d bytes\n", i+1, len(dls), x.paper, t.BytesTotal)
		}
		if failed > 0 {
			fmt.Fprintf(os.Stderr, "%d/%d downloads failed\n", failed, len(dls))
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "submitter: unknown command %q\n\n", cmd)
		usage(fs)
		os.Exit(2)
	}
}
