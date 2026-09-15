package writer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/parquet-go/parquet-go"
)

type Writer struct {
	dataDir       string
	flushInterval time.Duration
	bufferSize    int

	mu      sync.Mutex
	traces  []TraceSpan
	logs    []LogRecord
	metrics []MetricPoint

	// onError, if set, is called for flush failures that happen on the
	// background ticker or at shutdown. Those paths have no caller to return
	// an error to, so without this a failing writer would look healthy while
	// dropping data.
	onError func(error)

	stopCh chan struct{}
	done   chan struct{}
}

func New(dataDir string, flushInterval time.Duration, bufferSize int) *Writer {
	return &Writer{
		dataDir:       dataDir,
		flushInterval: flushInterval,
		bufferSize:    bufferSize,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// OnError registers a callback for flush errors raised on background paths.
func (w *Writer) OnError(fn func(error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onError = fn
}

func (w *Writer) report(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	fn := w.onError
	w.mu.Unlock()
	if fn != nil {
		fn(err)
	}
}

func (w *Writer) DataDir() string {
	return w.dataDir
}

func (w *Writer) Add(spans []TraceSpan) {
	w.mu.Lock()
	w.traces = append(w.traces, spans...)
	shouldFlush := len(w.traces) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.report(w.Flush())
	}
}

func (w *Writer) AddLogs(records []LogRecord) {
	w.mu.Lock()
	w.logs = append(w.logs, records...)
	shouldFlush := len(w.logs) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.report(w.Flush())
	}
}

func (w *Writer) AddMetrics(points []MetricPoint) {
	w.mu.Lock()
	w.metrics = append(w.metrics, points...)
	shouldFlush := len(w.metrics) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		w.report(w.Flush())
	}
}

func (w *Writer) Flush() error {
	w.mu.Lock()
	traces := w.traces
	logs := w.logs
	metrics := w.metrics
	w.traces, w.logs, w.metrics = nil, nil, nil
	w.mu.Unlock()

	// A failed write puts its records back at the head of the buffer so the
	// next flush retries them. Without this a transient error (full disk,
	// permission blip) silently discards everything buffered so far.
	var firstErr error
	if len(traces) > 0 {
		if err := writeParquet(w.dataDir, "traces", traces); err != nil {
			firstErr = err
			w.mu.Lock()
			dropped := requeue(&w.traces, traces)
			w.mu.Unlock()
			w.noteDropped("traces", dropped)
		}
	}
	if len(logs) > 0 {
		if err := writeParquet(w.dataDir, "logs", logs); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			w.mu.Lock()
			dropped := requeue(&w.logs, logs)
			w.mu.Unlock()
			w.noteDropped("logs", dropped)
		}
	}
	if len(metrics) > 0 {
		if err := writeParquet(w.dataDir, "metrics", metrics); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			w.mu.Lock()
			dropped := requeue(&w.metrics, metrics)
			w.mu.Unlock()
			w.noteDropped("metrics", dropped)
		}
	}
	return firstErr
}

// noteDropped reports records discarded because the retry buffer was full.
// Dropping is deliberate backpressure, but it must never be silent.
func (w *Writer) noteDropped(signal string, dropped int) {
	if dropped > 0 {
		w.report(fmt.Errorf("dropped %d %s record(s): flush buffer full", dropped, signal))
	}
}

// maxBuffered bounds how many records a signal may hold while flushes keep
// failing. Retrying forever would grow the buffer without limit and OOM a
// long-running serve process, so past this point the oldest records are
// dropped and the loss is reported rather than hidden.
const maxBuffered = 500_000

// requeue puts failed records back at the head of a signal's buffer, trimming
// from the front once the buffer exceeds maxBuffered.
func requeue[T any](buf *[]T, records []T) (dropped int) {
	merged := append(records, *buf...)
	if len(merged) > maxBuffered {
		dropped = len(merged) - maxBuffered
		merged = merged[dropped:]
	}
	*buf = merged
	return dropped
}

// writeParquet serializes records to a new Parquet file. The data is written to
// a temporary file in the same directory, fsync'd, and only then renamed into
// place. Rename is atomic within a filesystem, so a crash or an error mid-write
// can never leave a partial .parquet for the query glob to read — a truncated
// file is silently reported as zero rows by DuckDB rather than raising, which
// would make the loss invisible.
func writeParquet[T any](dataDir, signal string, records []T) error {
	now := time.Now().UTC()
	dir := filepath.Join(dataDir, signal, now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s dir: %w", signal, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer os.Remove(tmpPath)

	pw := parquet.NewGenericWriter[T](tmp)
	if _, err := pw.Write(records); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", signal, err)
	}
	if err := pw.Close(); err != nil {
		tmp.Close()
		return fmt.Errorf("closing parquet writer: %w", err)
	}
	// Flush the file's contents before the rename makes it visible.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", signal, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", signal, err)
	}

	path, err := nextPath(dir, now)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("committing %s: %w", signal, err)
	}
	return nil
}

// nextPath picks the first unused HH-MM[-n].parquet name in dir.
func nextPath(dir string, now time.Time) (string, error) {
	base := now.Format("15-04")
	path := filepath.Join(dir, base+".parquet")
	for i := 1; ; i++ {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return path, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking %s: %w", path, err)
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.parquet", base, i))
	}
}

func (w *Writer) Start() {
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.report(w.Flush())
			case <-w.stopCh:
				w.report(w.Flush())
				return
			}
		}
	}()
}

// Stop flushes remaining buffered records and shuts down the background
// flusher. It returns the error from that final flush, if any.
func (w *Writer) Stop() error {
	close(w.stopCh)
	<-w.done
	// Flush once more: records requeued by a failed final flush are still
	// buffered, and the background goroutine has already exited.
	return w.Flush()
}
