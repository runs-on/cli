package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestNewReplayTargetFollowsOTLPExporterEnv(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		headers  []string
		env      map[string]string
		want     replayTarget
		wantErr  bool
	}{
		{name: "default", want: replayTarget{endpoint: defaultReplayEndpoint, headers: map[string]string{}}},
		{
			name: "env endpoint and URL-encoded headers",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://ingest.example.com/", "OTEL_EXPORTER_OTLP_HEADERS": "api-key=a%3Db, x-team = ci"},
			want: replayTarget{endpoint: "https://ingest.example.com", headers: map[string]string{"api-key": "a=b", "x-team": "ci"}},
		},
		{
			name:     "flags win over env",
			endpoint: "http://localhost:4318",
			headers:  []string{"api-key=flag"},
			env:      map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://ingest.example.com", "OTEL_EXPORTER_OTLP_HEADERS": "api-key=env"},
			want:     replayTarget{endpoint: "http://localhost:4318", headers: map[string]string{"api-key": "flag"}},
		},
		{name: "gRPC host:port is not an OTLP/HTTP URL", endpoint: "localhost:4317", wantErr: true},
		{name: "header without value separator", headers: []string{"api-key"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newReplayTarget(tt.endpoint, tt.headers, func(key string) string { return tt.env[key] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newReplayTarget() = %+v, want an error", got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("newReplayTarget() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

// Each captured line is one OTLP export request, posted unchanged to its
// signal's path; signals not selected, other instances and names with path
// separators are left out.
func TestReplayPostsCapturedRequestsUnchanged(t *testing.T) {
	traces := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"94ef12e711b0603c0a36239fa21781bd","name":"test suite"}]}]}]}`
	logs := `{"resourceLogs":[]}`
	rotated := `{"resourceSpans":[]}`
	store := &mockMetricsS3Client{objects: map[string]string{
		"telemetry/AROAEXAMPLE:i-active/traces.jsonl":                         traces + "\n\n" + traces + "\n",
		"telemetry/AROAEXAMPLE:i-active/traces-2026-10-05T13-24-11.000.jsonl": rotated + "\n",
		"telemetry/AROAEXAMPLE:i-active/logs.jsonl":                           logs + "\n",
		"telemetry/AROAEXAMPLE:i-active/metrics.jsonl":                        `{"resourceMetrics":[]}` + "\n",
		"telemetry/AROAEXAMPLE:i-other/traces.jsonl":                          traces + "\n",
		// A job can write here; a name that would escape the archive is no capture.
		`telemetry/AROAEXAMPLE:i-active/logs-\..\..\escaped.jsonl`: logs + "\n",
	}}

	var (
		mu       sync.Mutex
		received []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		received = append(received, r.URL.Path+" "+r.Header.Get("Content-Type")+" "+r.Header.Get("api-key")+" "+string(body))
	}))
	defer server.Close()

	var out strings.Builder
	replayer := &captureReplayer{
		s3:     store,
		bucket: "cache",
		http:   server.Client(),
		target: replayTarget{endpoint: server.URL, headers: map[string]string{"api-key": "k"}},
		out:    &out,
	}
	objects, err := listTelemetryCapture(context.Background(), store, "cache", "AROAEXAMPLE", []string{"i-active", "i-active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := replayer.replayObjects(context.Background(), objects, map[string]bool{"traces": true, "logs": true}); err != nil {
		t.Fatalf("replayObjects() error = %v", err)
	}

	// The rotated segment is older than the current file, so it goes first.
	want := []string{
		"/v1/traces application/json k " + rotated,
		"/v1/traces application/json k " + traces,
		"/v1/traces application/json k " + traces,
		"/v1/logs application/json k " + logs,
	}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("received\n%q\nwant\n%q", received, want)
	}
	if !strings.Contains(out.String(), "trace 94ef12e711b0603c0a36239fa21781bd (2 spans)") {
		t.Fatalf("output does not name the replayed trace:\n%s", out.String())
	}
}

func TestReplayReportsEndpointRejection(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "error status", status: http.StatusUnauthorized, body: "unauthorized", want: "401"},
		{name: "partial success", status: http.StatusOK, body: `{"partialSuccess":{"rejectedSpans":"2","errorMessage":"too old"}}`, want: "rejected 2 Spans: too old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockMetricsS3Client{objects: map[string]string{
				"telemetry/AROAEXAMPLE:i-1/traces.jsonl": "{}\n",
			}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			replayer := &captureReplayer{s3: store, bucket: "cache", http: server.Client(), target: replayTarget{endpoint: server.URL}, out: io.Discard}
			objects, err := listTelemetryCapture(context.Background(), store, "cache", "AROAEXAMPLE", []string{"i-1"})
			if err != nil {
				t.Fatal(err)
			}
			err = replayer.replayObjects(context.Background(), objects, map[string]bool{"traces": true})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("replayObjects() error = %v, want %q", err, tt.want)
			}
		})
	}
}
