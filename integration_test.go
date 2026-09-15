package ducktel_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collectlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectmetricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collecttracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/receiver"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func startTestServer(t *testing.T) (string, *writer.Writer, func()) {
	t.Helper()

	dataDir, err := os.MkdirTemp("", "ducktel-test-*")
	if err != nil {
		t.Fatal(err)
	}

	w := writer.New(dataDir, 1*time.Hour, 1000)
	w.Start()

	port := 14318
	r := receiver.New(port, w)
	go r.Start()

	addr := fmt.Sprintf("http://localhost:%d", port)
	for i := 0; i < 20; i++ {
		resp, err := http.Get(addr + "/")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cleanup := func() {
		r.Stop(context.Background())
		if err := w.Stop(); err != nil {
			t.Errorf("final flush: %v", err)
		}
		os.RemoveAll(dataDir)
	}

	return addr, w, cleanup
}

func testResource() *resourcev1.Resource {
	return &resourcev1.Resource{
		Attributes: []*commonv1.KeyValue{
			{Key: "service.name", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "test-service"}}},
			{Key: "host.name", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "localhost"}}},
		},
	}
}

func sendProto(t *testing.T, addr, path string, msg proto.Message) {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	resp, err := http.Post(addr+path, "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("sending to %s: %v", path, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned status %d", path, resp.StatusCode)
	}
}

func TestTraces(t *testing.T) {
	addr, w, cleanup := startTestServer(t)
	defer cleanup()
	dataDir := w.DataDir()

	now := uint64(time.Now().UnixNano())
	sendProto(t, addr, "/v1/traces", &collecttracev1.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{
			{
				Resource: testResource(),
				ScopeSpans: []*tracev1.ScopeSpans{
					{
						Spans: []*tracev1.Span{
							{
								TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
								SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
								Name:              "test-span",
								Kind:              tracev1.Span_SPAN_KIND_SERVER,
								StartTimeUnixNano: now - 100_000_000,
								EndTimeUnixNano:   now,
								Status:            &tracev1.Status{Code: tracev1.Status_STATUS_CODE_OK},
								Attributes: []*commonv1.KeyValue{
									{Key: "http.method", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "GET"}}},
								},
							},
						},
					},
				},
			},
		},
	})

	w.Flush()

	engine, err := query.Open(dataDir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	// Verify trace stored
	results, _, err := engine.Query("SELECT span_name, status_code, duration_ms, resource_attributes FROM traces")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 span, got %d", len(results))
	}
	if results[0]["span_name"] != "test-span" {
		t.Errorf("span_name = %v", results[0]["span_name"])
	}
	// Verify resource attributes preserved
	ra, _ := results[0]["resource_attributes"].(string)
	if ra == "" || ra == "{}" {
		t.Errorf("resource_attributes should not be empty, got %v", ra)
	}
}

func TestLogs(t *testing.T) {
	addr, w, cleanup := startTestServer(t)
	defer cleanup()
	dataDir := w.DataDir()

	now := uint64(time.Now().UnixNano())
	sendProto(t, addr, "/v1/logs", &collectlogsv1.ExportLogsServiceRequest{
		ResourceLogs: []*logsv1.ResourceLogs{
			{
				Resource: testResource(),
				ScopeLogs: []*logsv1.ScopeLogs{
					{
						LogRecords: []*logsv1.LogRecord{
							{
								TimeUnixNano:         now,
								ObservedTimeUnixNano: now,
								SeverityNumber:       logsv1.SeverityNumber_SEVERITY_NUMBER_ERROR,
								SeverityText:         "ERROR",
								Body:                 &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "something went wrong"}},
								TraceId:              []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
								SpanId:               []byte{1, 2, 3, 4, 5, 6, 7, 8},
								Attributes: []*commonv1.KeyValue{
									{Key: "error.type", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "RuntimeError"}}},
								},
							},
							{
								TimeUnixNano:         now + 1000,
								ObservedTimeUnixNano: now + 1000,
								SeverityNumber:       logsv1.SeverityNumber_SEVERITY_NUMBER_INFO,
								SeverityText:         "INFO",
								Body:                 &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "request processed"}},
							},
						},
					},
				},
			},
		},
	})

	w.Flush()

	engine, err := query.Open(dataDir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	// Verify logs stored
	results, _, err := engine.Query("SELECT service_name, severity_text, body, trace_id FROM logs ORDER BY timestamp")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(results))
	}
	if results[0]["severity_text"] != "ERROR" {
		t.Errorf("first log severity = %v, want ERROR", results[0]["severity_text"])
	}
	if results[0]["body"] != "something went wrong" {
		t.Errorf("first log body = %v", results[0]["body"])
	}
	if results[0]["service_name"] != "test-service" {
		t.Errorf("service_name = %v", results[0]["service_name"])
	}
	if results[1]["severity_text"] != "INFO" {
		t.Errorf("second log severity = %v, want INFO", results[1]["severity_text"])
	}
}

func TestMetrics(t *testing.T) {
	addr, w, cleanup := startTestServer(t)
	defer cleanup()
	dataDir := w.DataDir()

	now := uint64(time.Now().UnixNano())
	sendProto(t, addr, "/v1/metrics", &collectmetricsv1.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricsv1.ResourceMetrics{
			{
				Resource: testResource(),
				ScopeMetrics: []*metricsv1.ScopeMetrics{
					{
						Metrics: []*metricsv1.Metric{
							{
								Name:        "http.request.duration",
								Description: "Duration of HTTP requests",
								Unit:        "ms",
								Data: &metricsv1.Metric_Histogram{
									Histogram: &metricsv1.Histogram{
										AggregationTemporality: metricsv1.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
										DataPoints: []*metricsv1.HistogramDataPoint{
											{
												TimeUnixNano:   now,
												Count:          10,
												Sum:            ptrFloat64(1500.0),
												Min:            ptrFloat64(50.0),
												Max:            ptrFloat64(500.0),
												BucketCounts:   []uint64{2, 5, 2, 1},
												ExplicitBounds: []float64{100, 200, 400},
												Attributes: []*commonv1.KeyValue{
													{Key: "http.method", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "POST"}}},
												},
											},
										},
									},
								},
							},
							{
								Name: "system.cpu.utilization",
								Unit: "1",
								Data: &metricsv1.Metric_Gauge{
									Gauge: &metricsv1.Gauge{
										DataPoints: []*metricsv1.NumberDataPoint{
											{
												TimeUnixNano: now,
												Value:        &metricsv1.NumberDataPoint_AsDouble{AsDouble: 0.75},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})

	w.Flush()

	engine, err := query.Open(dataDir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()

	// Verify metrics stored
	results, _, err := engine.Query("SELECT metric_name, metric_type, service_name, value_double, count, sum FROM metrics ORDER BY metric_name")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 metric points, got %d", len(results))
	}

	// Histogram
	if results[0]["metric_name"] != "http.request.duration" {
		t.Errorf("metric_name = %v", results[0]["metric_name"])
	}
	if results[0]["metric_type"] != "histogram" {
		t.Errorf("metric_type = %v", results[0]["metric_type"])
	}

	// Gauge
	if results[1]["metric_name"] != "system.cpu.utilization" {
		t.Errorf("metric_name = %v", results[1]["metric_name"])
	}
	if results[1]["metric_type"] != "gauge" {
		t.Errorf("metric_type = %v", results[1]["metric_type"])
	}
	if results[1]["value_double"] != 0.75 {
		t.Errorf("gauge value = %v, want 0.75", results[1]["value_double"])
	}
}

func ptrFloat64(f float64) *float64 {
	return &f
}

// --- writer durability regression tests ---

func testSpan() writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: "t", SpanID: "s", ServiceName: "svc", SpanName: "op",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 1,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: "{}", Events: "[]", Links: "[]",
	}
}

func spanCount(t *testing.T, dir string) int {
	t.Helper()
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening query engine: %v", err)
	}
	defer engine.Close()
	rows, _, err := engine.Query("SELECT count(*) AS c FROM traces")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return int(rows[0]["c"].(int64))
}

// blockedDir makes writes to a signal fail by putting a regular file where the
// signal directory needs to be, returning a func that unblocks it.
func blockedDir(t *testing.T, dir, signal string) func() {
	t.Helper()
	path := filepath.Join(dir, signal)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() { os.Remove(path) }
}

// TestFlushFailureRetriesInsteadOfDropping covers a data-loss bug: Flush took
// the buffers and cleared them before writing, so a failed write discarded
// everything buffered. Records must survive and be written by a later flush.
func TestFlushFailureRetriesInsteadOfDropping(t *testing.T) {
	dir := t.TempDir()
	unblock := blockedDir(t, dir, "traces")

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{testSpan(), testSpan(), testSpan()})

	if err := w.Flush(); err == nil {
		t.Fatal("expected the first flush to fail")
	}

	unblock()
	if err := w.Flush(); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if got := spanCount(t, dir); got != 3 {
		t.Errorf("after a failed flush and a retry, %d spans stored, want 3", got)
	}
}

// TestFlushIsAtomic verifies writes land via rename, leaving no partial or
// temporary file behind. A truncated .parquet is silently reported as zero
// rows by DuckDB, so it would hide data loss rather than surface it.
func TestFlushIsAtomic(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{testSpan()})
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		files = append(files, filepath.Base(p))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	if len(files) != 1 {
		t.Errorf("expected exactly 1 committed file, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if strings.HasPrefix(f, ".") {
			t.Errorf("temporary file left behind: %s", f)
		}
	}
}

// TestFlushErrorsAreReported verifies background flush failures reach OnError.
// Previously Add() and the ticker discarded Flush's error, so a writer that
// could not write anything looked healthy indefinitely.
func TestFlushErrorsAreReported(t *testing.T) {
	dir := t.TempDir()
	blockedDir(t, dir, "traces")

	w := writer.New(dir, 10*time.Millisecond, 1000)
	errs := make(chan error, 4)
	w.OnError(func(err error) { errs <- err })
	w.Add([]writer.TraceSpan{testSpan()})
	w.Start()
	defer w.Stop()

	select {
	case <-errs:
	case <-time.After(2 * time.Second):
		t.Error("no flush error was reported for a persistently failing writer")
	}
}

// TestStopReturnsFinalFlushError verifies a failed shutdown flush is not
// swallowed, since Stop is the last chance to notice lost telemetry.
func TestStopReturnsFinalFlushError(t *testing.T) {
	dir := t.TempDir()
	blockedDir(t, dir, "traces")

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{testSpan()})
	w.Start()

	if err := w.Stop(); err == nil {
		t.Error("Stop() returned nil despite an unflushable buffer")
	}
}
