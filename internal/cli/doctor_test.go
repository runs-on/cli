package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func TestParseDoctorECSServiceARN(t *testing.T) {
	clusterName, serviceName, ok := parseDoctorECSServiceARN("arn:aws:ecs:us-east-1:123456789012:service/runs-on-preview-v3/runs-on-worker")
	if !ok {
		t.Fatal("expected ECS service ARN to parse")
	}
	if clusterName != "runs-on-preview-v3" {
		t.Fatalf("expected cluster name runs-on-preview-v3, got %q", clusterName)
	}
	if serviceName != "runs-on-worker" {
		t.Fatalf("expected service name runs-on-worker, got %q", serviceName)
	}
}

func TestNormalizeDoctorServiceURL(t *testing.T) {
	if got := normalizeDoctorServiceURL("example.execute-api.us-east-1.amazonaws.com/prod"); got != "https://example.execute-api.us-east-1.amazonaws.com/prod" {
		t.Fatalf("unexpected normalized service URL %q", got)
	}
}

func TestDoctorReadinessURL(t *testing.T) {
	if got := doctorReadinessURL("example.execute-api.us-east-1.amazonaws.com/prod"); got != "https://example.execute-api.us-east-1.amazonaws.com/prod/readyz" {
		t.Fatalf("unexpected readiness URL %q", got)
	}
}

func TestStackDoctorGetServiceURLUsesStackConfig(t *testing.T) {
	doctor := NewStackDoctor(&RunsOnConfig{IngressURL: "example.execute-api.us-east-1.amazonaws.com/prod"}, io.Discard)

	serviceURL, err := doctor.getServiceURL()
	if err != nil {
		t.Fatalf("getServiceURL returned error: %v", err)
	}
	if serviceURL != "https://example.execute-api.us-east-1.amazonaws.com/prod" {
		t.Fatalf("unexpected service URL %q", serviceURL)
	}
}

func TestStackDoctorGetServiceURLErrorsWithoutIngress(t *testing.T) {
	doctor := NewStackDoctor(&RunsOnConfig{}, io.Discard)

	if _, err := doctor.getServiceURL(); err == nil {
		t.Fatal("expected getServiceURL to fail when ingress URL is missing")
	}
}

// The exported ZIP holds checks.json and the application log. checks.json
// records every check, the failed and skipped ones included, and a failed
// check fails the command.
func TestStackDoctorRunExportsChecksAndLogs(t *testing.T) {
	tests := []struct {
		name       string
		product    runsOnProduct
		readiness  string // the Flex /readyz body; Fleet has no endpoint
		wantErr    string
		wantChecks []string
	}{
		{
			name:    "fleet skips the HTTP checks",
			product: productFleet,
			wantErr: "1 check failed: Service running",
			wantChecks: []string{
				"Service running: fail",
				"Service endpoint accessible: skip",
				"Service readiness: skip",
				"Application logs fetched: pass",
				"Service logs fetched: skip",
			},
		},
		{
			name:      "flex without a configured GitHub app",
			product:   productFlex,
			readiness: `{"app_tag":"v3.4.0","github_app_configured":false}`,
			wantErr:   "2 checks failed: Service running, Service readiness",
			wantChecks: []string{
				"Service running: fail",
				"Service endpoint accessible: pass",
				"Service readiness: fail",
				"Application logs fetched: pass",
				"Service logs fetched: skip",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir()) // the ZIP is written to the working dir

			config := &RunsOnConfig{StackName: "runs-on", Product: tt.product, ServiceLogGroupName: "/aws/ecs/runs-on/service"}
			var httpClient *http.Client
			if tt.readiness != "" {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/readyz" {
						fmt.Fprint(w, tt.readiness)
					}
				}))
				defer server.Close()
				config.IngressURL = server.URL
				httpClient = server.Client()
			}
			doctor := NewStackDoctor(config, io.Discard)
			if httpClient != nil {
				doctor.httpClient = httpClient
			}
			doctor.tagging = &mockTaggedResourcesClient{getResources: func(context.Context, *resourcegroupstaggingapi.GetResourcesInput, ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error) {
				return nil, errors.New("denied")
			}}
			doctor.cwl = &mockCloudWatchLogsClient{events: runLogEvents()}

			if err := doctor.Run(context.Background(), time.Hour); err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Run error = %v, want %q", err, tt.wantErr)
			}

			zips, err := filepath.Glob("roc-doctor-*.zip")
			if err != nil || len(zips) != 1 {
				t.Fatalf("exported ZIPs = %v (err %v), want one", zips, err)
			}
			files := readZipFiles(t, zips[0])
			if names := sortedZipFileNames(files); !slices.Equal(names, []string{"checks.json", "logs/application.log"}) {
				t.Fatalf("ZIP entries = %v", names)
			}
			if got := strings.Count(files["logs/application.log"], " [run-stream] "); got != 2 {
				t.Fatalf("application log = %q, want both events", files["logs/application.log"])
			}
			var result DoctorResult
			if err := json.Unmarshal([]byte(files["checks.json"]), &result); err != nil {
				t.Fatalf("decode checks.json: %v", err)
			}
			var checks []string
			for _, check := range result.Checks {
				checks = append(checks, check.Name+": "+check.Status)
			}
			if !slices.Equal(checks, tt.wantChecks) {
				t.Fatalf("checks.json checks = %v, want %v", checks, tt.wantChecks)
			}
		})
	}
}

// Status values are the checks.json contract; skipped checks do not fail the
// command.
func TestDoctorResultFailedChecksError(t *testing.T) {
	tests := []struct {
		name    string
		checks  []DoctorCheck
		wantErr string
	}{
		{
			name: "pass and skip",
			checks: []DoctorCheck{
				{Name: "Service running", Status: "pass"},
				{Name: "Service readiness", Status: "skip"},
			},
		},
		{
			name: "one failure",
			checks: []DoctorCheck{
				{Name: "Service running", Status: "fail"},
				{Name: "Logs fetched", Status: "skip"},
			},
			wantErr: "1 check failed: Service running",
		},
		{
			name: "several failures",
			checks: []DoctorCheck{
				{Name: "Service running", Status: "fail"},
				{Name: "Service endpoint accessible", Status: "pass"},
				{Name: "Service readiness", Status: "fail"},
			},
			wantErr: "2 checks failed: Service running, Service readiness",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&DoctorResult{Checks: tt.checks}).failedChecksError()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("expected error %q, got %v", tt.wantErr, err)
			}
		})
	}
}
