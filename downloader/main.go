package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/cloudwego/hertz/pkg/protocol"
	"gorm.io/gorm"

	"github.com/zhangchunsheng-science/arxiv-go/taskdb"
)

const maxBodyBytes = 64 << 20

func doGet(hc *client.Client, rawURL, userAgent string, rangeStart int64, timeout time.Duration) (*protocol.Response, error) {
	uri := rawURL
	for hops := 0; hops < 5; hops++ {
		req := protocol.AcquireRequest()
		req.SetMethod("GET")
		req.SetRequestURI(uri)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept-Encoding", "identity")
		if rangeStart > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rangeStart))
		}
		resp := protocol.AcquireResponse()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := hc.Do(ctx, req, resp)
		cancel()
		protocol.ReleaseRequest(req)
		if err != nil {
			protocol.ReleaseResponse(resp)
			return nil, err
		}
		code := resp.StatusCode()
		if code >= 300 && code < 400 {
			loc := string(resp.Header.PeekLocation())
			protocol.ReleaseResponse(resp)
			if loc == "" {
				return nil, fmt.Errorf("HTTP %d without Location", code)
			}
			base, perr := url.Parse(uri)
			if perr != nil {
				return nil, perr
			}
			ref, perr := url.Parse(loc)
			if perr != nil {
				return nil, perr
			}
			uri = base.ResolveReference(ref).String()
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("too many redirects for %s", rawURL)
}

func main() {
	var (
		dbPath      string
		resultsDir  string
		minInterval time.Duration
		timeout     time.Duration
		maxAttempts int
		verbose     bool
	)
	fs := flag.NewFlagSet("downloader", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&dbPath, "db", "", "sqlite path (default: user cache dir/arxiv-go/queue.sqlite)")
	fs.StringVar(&resultsDir, "results", "", "result dir (default: <db dir>/results)")
	fs.DurationVar(&minInterval, "min-interval", 3*time.Second, "gap after one request finishes before next starts")
	fs.DurationVar(&timeout, "timeout", 60*time.Second, "per-request HTTP timeout")
	fs.IntVar(&maxAttempts, "max-attempts", 8, "attempts before a task is marked failed")
	fs.BoolVar(&verbose, "verbose", false, "verbose logging to stderr")
	if err := fs.Parse(os.Args[1:]); err != nil {
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
	if resultsDir == "" {
		resultsDir = filepath.Join(filepath.Dir(dbPath), "results")
	}
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
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
	if err := db.Model(&taskdb.Task{}).Where("status = ?", taskdb.StatusRunning).Updates(map[string]any{
		"status": taskdb.StatusPending, "updated_at": time.Now().Unix(),
	}).Error; err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	hc, err := client.NewClient(
		client.WithDialer(standard.NewDialer()),
		client.WithKeepAlive(false),
		client.WithMaxConnsPerHost(1),
		client.WithResponseBodyStream(true),
		client.WithDialTimeout(10*time.Second),
		client.WithClientReadTimeout(timeout),
		client.WithWriteTimeout(timeout),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	userAgent := "arxiv-go/0.1 (+https://github.com/zhangchunsheng-science/arxiv-go; rate-limit compliant)"
	var lastEnd time.Time

	for {
		var t taskdb.Task
		err := db.Where("status = ?", taskdb.StatusPending).First(&t).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if err != nil {
			logger.Warn("claim scan failed", "err", err)
			continue
		}
		res := db.Model(&taskdb.Task{}).Where("id = ? AND status = ?", t.ID, taskdb.StatusPending).Updates(map[string]any{
			"status": taskdb.StatusRunning, "updated_at": time.Now().Unix(),
		})
		if res.Error != nil {
			logger.Warn("claim failed", "id", t.ID, "err", res.Error)
			continue
		}
		if res.RowsAffected == 0 {
			continue
		}

		if !lastEnd.IsZero() {
			if d := lastEnd.Add(minInterval).Sub(time.Now()); d > 0 {
				time.Sleep(d)
			}
		}

		var execErr error
		var size int64
		ext := ".xml"
		if t.Kind == taskdb.KindDownload {
			ext = ".pdf"
		}
		resultPath := filepath.Join(resultsDir, fmt.Sprintf("task-%d%s", t.ID, ext))

		if t.Kind == taskdb.KindDownload {
			var start int64
			if fi, serr := os.Stat(resultPath); serr == nil {
				start = fi.Size()
			}
			resp, herr := doGet(hc, t.URL, userAgent, start, timeout)
			if herr != nil {
				execErr = herr
			} else {
				code := resp.StatusCode()
				contentRange := string(resp.Header.Peek("Content-Range"))
				stream := resp.BodyStream()
				switch {
				case code == http.StatusPartialContent:
					var total int64 = -1
					if i := strings.LastIndexByte(contentRange, '/'); i >= 0 {
						total, _ = strconv.ParseInt(contentRange[i+1:], 10, 64)
					}
					if total <= 0 {
						execErr = fmt.Errorf("range resume without total in Content-Range")
					} else {
						var f *os.File
						if f, execErr = os.OpenFile(resultPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); execErr == nil {
							var n int64
							n, execErr = io.Copy(f, stream)
							f.Close()
							switch {
							case execErr != nil:
							case start+n != total:
								execErr = fmt.Errorf("incomplete body: got %d of %d bytes", start+n, total)
							default:
								size = total
							}
						}
					}
				case code == http.StatusOK:
					if start > 0 {
						logger.Warn("server ignored Range, rewriting from zero", "id", t.ID, "had", start)
					}
					var f *os.File
					if f, execErr = os.OpenFile(resultPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); execErr == nil {
						size, execErr = io.Copy(f, stream)
						f.Close()
					}
				case code == http.StatusRequestedRangeNotSatisfiable:
					_ = os.Remove(resultPath)
					execErr = fmt.Errorf("range not satisfiable (local file newer than remote), restart from zero")
				default:
					execErr = fmt.Errorf("GET %s: HTTP %d", t.URL, code)
				}
				protocol.ReleaseResponse(resp)
			}
		} else {
			resp, herr := doGet(hc, t.URL, userAgent, 0, timeout)
			if herr != nil {
				execErr = herr
			} else {
				code := resp.StatusCode()
				var body []byte
				body, execErr = io.ReadAll(io.LimitReader(resp.BodyStream(), maxBodyBytes+1))
				protocol.ReleaseResponse(resp)
				switch {
				case execErr != nil:
				case code != http.StatusOK:
					execErr = fmt.Errorf("GET %s: HTTP %d", t.URL, code)
				case len(body) > maxBodyBytes:
					execErr = fmt.Errorf("GET %s: body exceeds %d bytes", t.URL, maxBodyBytes)
				default:
					execErr = os.WriteFile(resultPath, body, 0o644)
					size = int64(len(body))
				}
			}
		}
		lastEnd = time.Now()

		if execErr != nil {
			attempts := t.Attempts + 1
			bytesDone := int64(0)
			if t.Kind == taskdb.KindDownload {
				if fi, serr := os.Stat(resultPath); serr == nil {
					bytesDone = fi.Size()
				}
			}
			status := taskdb.StatusPending
			if attempts >= maxAttempts {
				status = taskdb.StatusFailed
			}
			errStr := execErr.Error()
			if len(errStr) > 500 {
				errStr = errStr[:500] + "…"
			}
			if uerr := db.Model(&taskdb.Task{}).Where("id = ?", t.ID).Updates(map[string]any{
				"status": status, "attempts": attempts, "last_error": errStr, "bytes_done": bytesDone, "updated_at": time.Now().Unix(),
			}).Error; uerr != nil {
				logger.Warn("mark failed errored", "id", t.ID, "err", uerr)
			}
			logger.Warn("task failed", "id", t.ID, "kind", t.Kind, "attempt", attempts, "err", execErr)
			continue
		}
		if uerr := db.Model(&taskdb.Task{}).Where("id = ?", t.ID).Updates(map[string]any{
			"status": taskdb.StatusDone, "result_path": resultPath, "bytes_total": size, "bytes_done": size,
			"last_error": "", "updated_at": time.Now().Unix(),
		}).Error; uerr != nil {
			logger.Warn("mark done errored", "id", t.ID, "err", uerr)
			continue
		}
		logger.Info("task done", "id", t.ID, "kind", t.Kind, "bytes", size)
	}
}
