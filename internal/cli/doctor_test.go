package cli

import "testing"

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
	doctor := NewStackDoctor(&RunsOnConfig{IngressURL: "example.execute-api.us-east-1.amazonaws.com/prod"})

	serviceURL, err := doctor.getServiceURL()
	if err != nil {
		t.Fatalf("getServiceURL returned error: %v", err)
	}
	if serviceURL != "https://example.execute-api.us-east-1.amazonaws.com/prod" {
		t.Fatalf("unexpected service URL %q", serviceURL)
	}
}

func TestStackDoctorGetServiceURLErrorsWithoutIngress(t *testing.T) {
	doctor := NewStackDoctor(&RunsOnConfig{})

	if _, err := doctor.getServiceURL(); err == nil {
		t.Fatal("expected getServiceURL to fail when ingress URL is missing")
	}
}

func TestStackDoctorSkipsHTTPHealthChecksForFleet(t *testing.T) {
	doctor := NewStackDoctor(&RunsOnConfig{Product: "fleet"})

	doctor.checkHTTPHealth()

	if len(doctor.result.Checks) != 2 {
		t.Fatalf("expected two skipped checks, got %+v", doctor.result.Checks)
	}
	for _, check := range doctor.result.Checks {
		if check.Status != "⏭️" {
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
