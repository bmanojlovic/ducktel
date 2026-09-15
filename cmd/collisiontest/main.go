// Two-writer collision test.
//
// Question (context.md Q2): can two independent ducktel writers share one data
// directory? nextPath() does Stat-then-Rename with no atomicity, so two
// processes can both see a name as absent, both choose it, and both rename
// onto it — the second silently overwriting the first.
//
// This runs N writers concurrently against one directory, then reconciles what
// was written against what was asked to be written.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

const (
	writers        = 4
	flushesPerW    = 60
	spansPerFlush  = 10
	expectedPerWr  = flushesPerW * spansPerFlush
	fakeHostPrefix = "host"
)

func main() {
	dir, err := os.MkdirTemp("", "ducktel-collision-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	fmt.Printf("data dir: %s\n", dir)
	fmt.Printf("writers=%d flushes/writer=%d spans/flush=%d => %d spans per writer, %d total\n\n",
		writers, flushesPerW, spansPerFlush, expectedPerWr, writers*expectedPerWr)

	// Each writer pretends to be a separate host: distinct service name so we
	// can attribute rows back to their writer in the results.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			wt := writer.New(dir, time.Hour, 1<<20)
			svc := fmt.Sprintf("%s-%d", fakeHostPrefix, id)
			<-start // all writers begin together to maximise the collision window
			for f := 0; f < flushesPerW; f++ {
				spans := make([]writer.TraceSpan, 0, spansPerFlush)
				now := time.Now()
				for s := 0; s < spansPerFlush; s++ {
					spans = append(spans, writer.TraceSpan{
						TraceID:     fmt.Sprintf("%s-%d-%d", svc, f, s),
						SpanID:      fmt.Sprintf("s-%d-%d-%d", id, f, s),
						ServiceName: svc, SpanName: "op",
						StartTime: now.UnixMicro(), EndTime: now.UnixMicro(),
						DurationMs: 1, StatusCode: "STATUS_CODE_OK",
						Attributes: "{}", ResourceAttributes: "{}",
						Events: "[]", Links: "[]",
					})
				}
				wt.Add(spans)
				if err := wt.Flush(); err != nil {
					fmt.Printf("  writer %d flush %d error: %v\n", id, f, err)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	// Count files on disk.
	var files []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(p) == ".parquet" {
			files = append(files, p)
		}
		return nil
	})
	totalWrites := writers * flushesPerW
	fmt.Printf("\n--- files ---\n")
	fmt.Printf("flush operations attempted: %d\n", totalWrites)
	fmt.Printf("parquet files on disk:      %d\n", len(files))
	if len(files) != totalWrites {
		fmt.Printf("LOST WRITES: %d file(s) overwritten\n", totalWrites-len(files))
	} else {
		fmt.Printf("no overwrite detected at file level\n")
	}

	// Reconcile rows: this is the real measure, since an overwrite replaces
	// one writer's rows with another's.
	engine, err := query.Open(dir)
	if err != nil {
		panic(err)
	}
	defer engine.Close()

	rows, _, err := engine.Query(
		"SELECT service_name, count(*) AS c FROM traces GROUP BY 1 ORDER BY 1")
	if err != nil {
		panic(err)
	}

	fmt.Printf("\n--- rows per writer ---\n")
	total := int64(0)
	for _, r := range rows {
		c := r["c"].(int64)
		total += c
		status := "ok"
		if c != int64(expectedPerWr) {
			status = fmt.Sprintf("MISMATCH (expected %d, lost %d)", expectedPerWr, int64(expectedPerWr)-c)
		}
		fmt.Printf("  %-12s %6d  %s\n", r["service_name"], c, status)
	}
	expectedTotal := int64(writers * expectedPerWr)
	fmt.Printf("\n  total: %d, expected: %d\n", total, expectedTotal)
	if total != expectedTotal {
		fmt.Printf("\n*** DATA LOSS: %d span(s) lost (%.2f%%) ***\n",
			expectedTotal-total, 100*float64(expectedTotal-total)/float64(expectedTotal))
	} else {
		fmt.Printf("\nno row loss detected\n")
	}

	// Also report any unreadable file, since a clobbered temp/rename can leave
	// a truncated parquet (which DuckDB reports as zero rows, silently).
	bad := 0
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.Size() == 0 {
			fmt.Printf("zero-byte file: %s\n", filepath.Base(f))
			bad++
		}
	}
	fmt.Printf("zero-byte files: %d\n", bad)
}
