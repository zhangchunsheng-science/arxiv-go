package main

import (
	"bytes"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhangchunsheng-science/arxiv-go/taskdb"
)

func fail(err error) {
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
}

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Println("ok  ", name)
		return
	}
	fmt.Println("FAIL", name, "-", detail)
	fmt.Println("FAIL: integration test")
	os.Exit(1)
}

var cmdEnv []string

func run(bin string, args ...string) (int, string) {
	cmd := exec.Command(bin, args...)
	cmd.Env = cmdEnv
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, out.String()
}

const feedXML = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <link href="http://arxiv.org/api/query" rel="self" type="application/atom+xml"/>
  <title type="html">ArXiv Query: mock</title>
  <id>http://arxiv.org/api/mock</id>
  <updated>2026-11-15T00:00:00-05:00</updated>
  <opensearch:totalResults xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/">1</opensearch:totalResults>
  <opensearch:startIndex xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/">0</opensearch:startIndex>
  <opensearch:itemsPerPage xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/">5</opensearch:itemsPerPage>
  <entry>
    <id>http://arxiv.org/abs/1607.03037v2</id>
    <updated>2016-07-12T14:50:48Z</updated>
    <published>2016-07-08T03:34:24Z</published>
    <title>Evidence for new magnetic monopoles... with mock suffix</title>
    <summary>We present a Monte Carlo study...</summary>
    <author><name>C. L. Henley</name></author>
    <link href="http://arxiv.org/abs/1607.03037v2" rel="alternate" type="text/html"/>
    <link title="pdf" href="http://arxiv.org/pdf/1607.03037v2" rel="related" type="application/pdf"/>
    <arxiv:primary_category xmlns:arxiv="http://arxiv.org/schemas/atom" term="cond-mat.str-el" scheme="http://arxiv.org/schemas/atom"/>
    <category term="cond-mat.str-el" scheme="http://arxiv.org/schemas/atom"/>
  </entry>
</feed>`

func main() {
	live := flag.Bool("live", false, "hit real arXiv instead of a local mock server")
	root := flag.String("root", ".", "module root")
	keep := flag.Bool("keep", false, "keep temp dir")
	flag.Parse()

	tmp, err := os.MkdirTemp("", "arxiv-itest-*")
	fail(err)
	defer func() {
		if !*keep {
			os.RemoveAll(tmp)
		}
	}()

	dlBin := filepath.Join(tmp, "downloader")
	subBin := filepath.Join(tmp, "submitter")
	for _, b := range []struct{ out, pkg string }{{dlBin, "./downloader"}, {subBin, "./submitter"}} {
		cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
		cmd.Dir = *root
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println("FAIL: build", b.pkg, err)
			os.Exit(1)
		}
	}

	cmdEnv = os.Environ()
	minInterval := "3s"
	if !*live {
		pdfBody := make([]byte, 16384)
		copy(pdfBody, "%PDF-1.4 mock arxiv pdf")
		truncatedOnce := false
		mux := http.NewServeMux()
		mux.HandleFunc("/api/query", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
			fmt.Fprint(w, feedXML)
		})
		mux.HandleFunc("/pdf/", func(w http.ResponseWriter, r *http.Request) {
			rng := r.Header.Get("Range")
			var start int
			if s := strings.TrimPrefix(rng, "bytes="); s != rng {
				start, _ = strconv.Atoi(strings.TrimSuffix(s, "-"))
			}
			if start == 0 && !truncatedOnce {
				truncatedOnce = true
				w.Header().Set("Content-Length", strconv.Itoa(len(pdfBody)))
				w.WriteHeader(http.StatusOK)
				w.Write(pdfBody[:6000])
				panic(http.ErrAbortHandler)
			}
			if start > 0 {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(pdfBody)-1, len(pdfBody)))
				w.Header().Set("Content-Length", strconv.Itoa(len(pdfBody)-start))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(pdfBody[start:])
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(pdfBody)))
			w.WriteHeader(http.StatusOK)
			w.Write(pdfBody)
		})
		srv := &http.Server{Handler: mux}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		fail(err)
		go srv.Serve(ln)
		defer srv.Close()
		addr := "http://" + ln.Addr().String()
		cmdEnv = append(cmdEnv, "ARXIV_BASE_URL="+addr+"/api/query", "ARXIV_PDF_BASE="+addr+"/pdf")
		minInterval = "300ms"
	}

	dbPath := filepath.Join(tmp, "queue.sqlite")
	resultsDir := filepath.Join(tmp, "results")
	dl := exec.Command(dlBin, "--db", dbPath, "--results", resultsDir, "--max-attempts", "2", "--min-interval", minInterval)
	dl.Stderr = os.Stderr
	fail(dl.Start())
	defer dl.Process.Kill()

	db, err := taskdb.Open(dbPath)
	fail(err)
	defer func() {
		if sqlDB, e := db.DB(); e == nil {
			sqlDB.Close()
		}
	}()

	code, out := run(subBin, "--db", dbPath, "search", `ti:"electron thermal conductivity"`, "--max", "5")
	check("search exit 0", code == 0, fmt.Sprintf("exit %d", code))
	check("search finds 1607.03037", strings.Contains(out, "1607.03037"), out)

	code, out = run(subBin, "--db", dbPath, "get", "1607.03037")
	check("get exit 0", code == 0, fmt.Sprintf("exit %d", code))
	check("get returns the paper", strings.Contains(out, "1607.03037"), out)

	code, out = run(subBin, "--db", dbPath, "get", "1607.03037", "--json")
	check("get json exit 0", code == 0, fmt.Sprintf("exit %d", code))
	check("get json (sonic)", strings.Contains(out, `"id": "1607.03037v`) && strings.Contains(out, `"total_results"`), out)

	code, out = run(subBin, "--db", dbPath, "download", "1607.03037")
	check("download exit 0", code == 0, fmt.Sprintf("exit %d", code))
	pdfPath := strings.TrimSpace(out)
	pdf, err := os.ReadFile(pdfPath)
	fail(err)
	check("pdf magic", len(pdf) > 4 && string(pdf[:4]) == "%PDF", pdfPath)
	check("pdf size", len(pdf) > 10000, fmt.Sprintf("%d bytes", len(pdf)))

	var dlt taskdb.Task
	fail(db.Where("kind = ?", taskdb.KindDownload).First(&dlt).Error)
	check("pdf bytes recorded", dlt.BytesDone == int64(len(pdf)), fmt.Sprintf("db %d vs file %d", dlt.BytesDone, len(pdf)))

	code, _ = run(subBin, "--db", dbPath, "search", `ti:"electron thermal conductivity"`, "--max", "5")
	check("search repeat exit 0", code == 0, fmt.Sprintf("exit %d", code))
	var nSearch, nDownload int64
	fail(db.Model(&taskdb.Task{}).Where("kind = ?", taskdb.KindSearch).Count(&nSearch).Error)
	fail(db.Model(&taskdb.Task{}).Where("kind = ?", taskdb.KindDownload).Count(&nDownload).Error)
	check("search dedup (2 rows: query + id_list)", nSearch == 2, fmt.Sprintf("got %d", nSearch))
	check("download dedup (1 row)", nDownload == 1, fmt.Sprintf("got %d", nDownload))

	code, out = run(subBin, "--db", dbPath, "search", `ti:"electron thermal conductivity"`, "--max", "3", "--no-wait")
	check("search --no-wait exit 0", code == 0, fmt.Sprintf("exit %d", code))
	nwFields := strings.Fields(out)
	var nwID int64
	if len(nwFields) == 2 && nwFields[0] == "task" {
		nwID, err = strconv.ParseInt(nwFields[1], 10, 64)
		fail(err)
	}
	check("search --no-wait prints task id", nwID > 0, out)
	nwDeadline := time.Now().Add(60 * time.Second)
	for {
		nwt, err := taskdb.Get(db, nwID)
		fail(err)
		if nwt.Status == taskdb.StatusFailed {
			check("no-wait search completes in background", false, nwt.LastError)
		}
		if nwt.Status == taskdb.StatusDone {
			if _, serr := os.Stat(nwt.ResultPath); serr == nil {
				break
			}
			check("no-wait result file exists", false, nwt.ResultPath)
		}
		if time.Now().After(nwDeadline) {
			check("no-wait search completes in background", false, "timeout, status "+nwt.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("ok   no-wait search completes in background")

	code, out = run(subBin, "--db", dbPath, "download", "1607.03037v1", "--no-wait")
	check("download --no-wait exit 0", code == 0, fmt.Sprintf("exit %d", code))
	nwFields = strings.Fields(out)
	nwID = 0
	if len(nwFields) == 3 && nwFields[1] == "task" {
		nwID, err = strconv.ParseInt(nwFields[2], 10, 64)
		fail(err)
	}
	check("download --no-wait prints task id", nwID > 0, out)
	nwDeadline = time.Now().Add(60 * time.Second)
	for {
		nwt, err := taskdb.Get(db, nwID)
		fail(err)
		if nwt.Status == taskdb.StatusFailed {
			check("no-wait download completes in background", false, nwt.LastError)
		}
		if nwt.Status == taskdb.StatusDone {
			magic, _ := os.ReadFile(nwt.ResultPath)
			check("no-wait pdf magic", len(magic) > 4 && string(magic[:4]) == "%PDF", nwt.ResultPath)
			break
		}
		if time.Now().After(nwDeadline) {
			check("no-wait download completes in background", false, "timeout, status "+nwt.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("ok   no-wait download completes in background")

	bogusID, err := taskdb.Submit(db, taskdb.KindSearch, "http://127.0.0.1:1/none", "bogus", time.Now().Add(time.Hour).Unix())
	fail(err)
	deadline := time.Now().Add(60 * time.Second)
	for {
		t, err := taskdb.Get(db, bogusID)
		fail(err)
		if t.Status == taskdb.StatusFailed {
			check("bogus attempts == 2 (max-attempts)", t.Attempts == 2, fmt.Sprintf("attempts %d", t.Attempts))
			check("bogus last_error recorded", t.LastError != "", "empty last_error")
			break
		}
		if time.Now().After(deadline) {
			check("bogus task reaches failed", false, "timeout, status "+t.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}

	fail(dl.Process.Kill())
	_ = dl.Wait()
	fmt.Println("PASS: integration test")
}
