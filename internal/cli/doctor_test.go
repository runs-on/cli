package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestStackDoctorSkipsHTTPHealthChecksForFleet(t *testing.T) {
	var out bytes.Buffer
	doctor := NewStackDoctor(&RunsOnConfig{Product: "fleet"}, &out)

	doctor.checkHTTPHealth(context.Background())

	wantOut := "Checking service endpoint... ⏭️ (Skipped - Fleet does not expose a public service endpoint)\n" +
		"Checking service readiness... ⏭️ (Skipped - Fleet does not expose a public readiness endpoint)\n"
	if out.String() != wantOut {
		t.Fatalf("unexpected output\ngot:\n%s\nwant:\n%s", out.String(), wantOut)
	}
	if len(doctor.result.Checks) != 2 {
		t.Fatalf("expected two skipped checks, got %+v", doctor.result.Checks)
	}
	for _, check := range doctor.result.Checks {
		if check.Status != "skip" {
			t.Fatalf("expected skipped status, got %+v", check)
		}
	}
	if doctor.result.Checks[0].Name != "Service endpoint accessible" {
		t.Fatalf("unexpected first check %+v", doctor.result.Checks[0])
	}
	if doctor.result.Checks[1].Name != "Service readiness" {
		t.Fatalf("unexpected second check %+v", doctor.result.Checks[1])
	}
}

// The exported ZIP holds checks.json and the application log. checks.json
// records every check, the failed and skipped ones included.
func TestStackDoctorRunExportsChecksAndLogs(t *testing.T) {
	t.Chdir(t.TempDir()) // the ZIP is written to the working dir

	doctor := NewStackDoctor(&RunsOnConfig{StackName: "runs-on", Product: productFleet, ServiceLogGroupName: "/aws/ecs/runs-on/fleetd"}, io.Discard)
	doctor.tagging = &mockTaggedResourcesClient{getResources: func(context.Context, *resourcegroupstaggingapi.GetResourcesInput, ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error) {
		return nil, errors.New("denied")
	}}
	doctor.cwl = &mockCloudWatchLogsClient{events: runLogEvents()}

	if err := doctor.Run(context.Background(), time.Hour); err == nil || err.Error() != "1 check failed: Service running" {
		t.Fatalf("Run error = %v, want the failed service check", err)
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
	want := []string{
		"Service running: fail",
		"Service endpoint accessible: skip",
		"Service readiness: skip",
		"Application logs fetched: pass",
		"Service logs fetched: skip",
	}
	if !slices.Equal(checks, want) {
		t.Fatalf("checks.json checks = %v, want %v", checks, want)
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
