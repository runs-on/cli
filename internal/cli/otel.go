package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
)

const defaultReplayEndpoint = "http://localhost:4318"

// maxCaptureLine bounds one captured export request; the runner's collector
// batches at most a few thousand items per request.
const maxCaptureLine = 64 << 20

func NewOtelCmd(stack *Stack) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "otel",
		Short: "Work with the OpenTelemetry data RunsOn runners capture",
	}
	cmd.AddCommand(newOtelReplayCmd(stack))
	return cmd
}

func newOtelReplayCmd(stack *Stack) *cobra.Command {
	var (
		endpoint string
		headers  []string
		signals  []string
	)

	cmd := &cobra.Command{
		Use:   "replay JOB_URL",
		Short: "Send a job's captured traces, logs and metrics to an OTLP endpoint",
		Long: `Send a job's captured traces, logs and metrics to an OTLP/HTTP endpoint.

Every RunsOn runner keeps a local capture of the telemetry its collector
receives: the RunsOn agent's spans, your own spans and logs sent to
127.0.0.1:4318, host metrics, and the bootstrap log. The capture is uploaded to
the stack's cache bucket when the instance shuts down. This command sends it,
unchanged, to any OTLP/HTTP endpoint: a local SigNoz, Jaeger or Grafana, or your
production backend.

Data keeps its original timestamps: set the backend's time range to the job's.
Control-plane spans (scheduling, instance launch, GitHub steps on Flex) are not
part of the runner capture; they reach the backend configured on the stack.`,
		Example: `  roc otel replay https://github.com/my-org/my-repo/actions/runs/123/job/456
  roc otel replay JOB_URL --endpoint https://ingest.example.com --header "signoz-ingestion-key=..."
  roc otel replay JOB_URL --signals traces`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			job, err := parseGitHubJobURL(args[0])
			if err != nil {
				return err
			}
			target, err := newReplayTarget(endpoint, headers, os.Getenv)
			if err != nil {
				return err
			}
			selected, err := replaySignals(signals)
			if err != nil {
				return err
			}

			config, err := stack.discoverResources(cmd)
			if err != nil {
				return err
			}
			if err := config.validateJobLookup(); err != nil {
				return err
			}
			if config.CacheBucket == "" {
				return fmt.Errorf("cache bucket not found for stack %q", config.StackName)
			}
			if config.InstanceRoleID == "" {
				return fmt.Errorf("stack %q does not record its runner role ID; upgrade it to capture runner telemetry", config.StackName)
			}

			replayer := &captureReplayer{
				s3:     s3.NewFromConfig(config.AWSConfig),
				bucket: config.CacheBucket,
				roleID: config.InstanceRoleID,
				http:   &http.Client{Timeout: 30 * time.Second},
				target: target,
				out:    cmd.OutOrStdout(),
			}
			return replayer.replayJob(cmd.Context(), newJobDiagnosticsResolver(config), config, job, selected)
		},
	}

	cmd.Flags().StringVar(&endpoint, "endpoint", "", "OTLP/HTTP base URL (default: $OTEL_EXPORTER_OTLP_ENDPOINT, else "+defaultReplayEndpoint+")")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "Header to send, as key=value; repeatable (default: $OTEL_EXPORTER_OTLP_HEADERS)")
	cmd.Flags().StringSliceVar(&signals, "signals", []string{"traces", "logs", "metrics"}, "Signals to replay")
	return cmd
}

type replayTarget struct {
	endpoint string
	headers  map[string]string
}

func newReplayTarget(endpoint string, headerFlags []string, getenv func(string) string) (replayTarget, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	if endpoint == "" {
		endpoint = defaultReplayEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return replayTarget{}, fmt.Errorf("--endpoint must be an http(s) URL, got %q", endpoint)
	}

	headers := map[string]string{}
	if len(headerFlags) == 0 {
		// OTEL_EXPORTER_OTLP_HEADERS is key=value pairs, comma separated, with
		// URL-encoded values.
		for pair := range strings.SplitSeq(getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
			key, value, ok := strings.Cut(pair, "=")
			if !ok || strings.TrimSpace(key) == "" {
				continue
			}
			// PathUnescape, as the OpenTelemetry SDKs and the RunsOn stack do:
			// QueryUnescape would turn a "+" in a base64 token into a space.
			if decoded, err := url.PathUnescape(strings.TrimSpace(value)); err == nil {
				value = decoded
			}
			headers[strings.TrimSpace(key)] = value
		}
	}
	for _, flag := range headerFlags {
		key, value, ok := strings.Cut(flag, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return replayTarget{}, fmt.Errorf("--header must be key=value, got %q", flag)
		}
		headers[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return replayTarget{endpoint: strings.TrimRight(endpoint, "/"), headers: headers}, nil
}

func replaySignals(signals []string) (map[string]bool, error) {
	selected := map[string]bool{}
	for _, signal := range signals {
		signal = strings.ToLower(strings.TrimSpace(signal))
		if !slices.Contains([]string{"traces", "logs", "metrics"}, signal) {
			return nil, fmt.Errorf("unknown signal %q (want traces, logs or metrics)", signal)
		}
		selected[signal] = true
	}
	return selected, nil
}

type captureReplayer struct {
	s3     metricsS3API
	bucket string
	roleID string
	http   *http.Client
	target replayTarget
	out    io.Writer
}

func (r *captureReplayer) replayJob(ctx context.Context, resolver *jobDiagnosticsResolver, config *RunsOnConfig, job parsedGitHubJobURL, signals map[string]bool) error {
	request := buildJobDiagnosticsRequest(job)
	diagnostics, err := resolver.resolveRequest(ctx, request)
	if err != nil {
		return err
	}
	if err := diagnostics.validateStackProduct(config.Product, config.StackName, job.JobID); err != nil {
		return err
	}
	facts := diagnostics.jobFacts(job)
	if !facts.Found {
		return fmt.Errorf("job %s not found", job.URL)
	}
	if len(facts.InstanceIDs) == 0 {
		return fmt.Errorf("job %s has no runner instance to replay", job.URL)
	}

	objects, err := listTelemetryCapture(ctx, r.s3, r.bucket, r.roleID, facts.InstanceIDs)
	if err != nil {
		return fmt.Errorf("list telemetry capture: %w", err)
	}
	return r.replayObjects(ctx, objects, signals)
}

func (r *captureReplayer) replayObjects(ctx context.Context, objects []telemetryCaptureObject, signals map[string]bool) error {
	var (
		replayed int
		failures []error
		spans    = map[string]int{} // trace ID -> replayed spans
	)
	for _, object := range objects {
		if !signals[object.Signal] {
			continue
		}
		requests, err := r.replayObject(ctx, object, object.Signal, spans)
		replayed += requests
		if err != nil {
			failures = append(failures, fmt.Errorf("%s %s: %w", object.InstanceID, object.Name, err))
			continue
		}
		fmt.Fprintf(r.out, "%s %s: %d requests\n", object.InstanceID, object.Name, requests)
	}

	if replayed == 0 && len(failures) == 0 {
		return fmt.Errorf("no telemetry capture found for this job; it needs a RunsOn version that uploads the capture, and an instance that shut down cleanly")
	}
	fmt.Fprintf(r.out, "Replayed %d OTLP requests to %s\n", replayed, r.target.endpoint)
	// The job's own trace has the most spans; early boot spans can start
	// traces of their own.
	traceIDs := slices.Collect(maps.Keys(spans))
	slices.SortFunc(traceIDs, func(a, b string) int {
		if spans[a] != spans[b] {
			return spans[b] - spans[a]
		}
		return strings.Compare(a, b)
	})
	for _, id := range traceIDs {
		fmt.Fprintf(r.out, "trace %s (%d spans)\n", id, spans[id])
	}
	return errors.Join(failures...)
}

// replayObject posts each line of one capture file, unchanged, to the
// signal's OTLP/HTTP path, and counts the spans it sent per trace.
func (r *captureReplayer) replayObject(ctx context.Context, object telemetryCaptureObject, signal string, spans map[string]int) (int, error) {
	output, err := r.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(object.Key)})
	if err != nil {
		return 0, fmt.Errorf("fetch: %w", err)
	}
	defer output.Body.Close()

	scanner := bufio.NewScanner(output.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), maxCaptureLine)
	requests := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := r.post(ctx, signal, line); err != nil {
			return requests, fmt.Errorf("request %d: %w", requests+1, err)
		}
		requests++
		if signal == "traces" {
			countTraceSpans(spans, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return requests, fmt.Errorf("read: %w", err)
	}
	return requests, nil
}

func (r *captureReplayer) post(ctx context.Context, signal string, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.target.endpoint+"/v1/"+signal, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range r.target.headers {
		request.Header.Set(key, value)
	}
	response, err := r.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	detail, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", request.URL, err)
	}
	if response.StatusCode/100 != 2 {
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return fmt.Errorf("%s returned %s: %s", request.URL, response.Status, strings.TrimSpace(string(detail)))
	}
	return partialSuccessError(detail)
}

// partialSuccessError reports the items an OTLP endpoint accepted the request
// for but rejected: OTLP/HTTP answers 200 with a partialSuccess body.
func partialSuccessError(body []byte) error {
	var response struct {
		PartialSuccess map[string]json.RawMessage `json:"partialSuccess"`
	}
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &response) != nil {
		return nil
	}
	var message string
	_ = json.Unmarshal(response.PartialSuccess["errorMessage"], &message)
	for _, field := range []string{"rejectedSpans", "rejectedLogRecords", "rejectedDataPoints"} {
		raw, ok := response.PartialSuccess[field]
		if !ok {
			continue
		}
		// int64 fields are JSON strings in OTLP/JSON; some servers send numbers.
		count := strings.Trim(string(raw), `"`)
		if count != "" && count != "0" {
			return fmt.Errorf("endpoint rejected %s %s: %s", count, strings.TrimPrefix(field, "rejected"), message)
		}
	}
	return nil
}

// countTraceSpans counts the spans of one OTLP JSON traces request per trace.
func countTraceSpans(spans map[string]int, line []byte) {
	var request struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID string `json:"traceId"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if json.Unmarshal(line, &request) != nil {
		return
	}
	for _, resourceSpans := range request.ResourceSpans {
		for _, scopeSpans := range resourceSpans.ScopeSpans {
			for _, span := range scopeSpans.Spans {
				if span.TraceID != "" {
					spans[span.TraceID]++
				}
			}
		}
	}
}
