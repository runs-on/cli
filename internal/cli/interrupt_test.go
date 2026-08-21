package cli

import (
	"encoding/json"
	"testing"
)

func TestTrustPolicyAllowsFISServiceToAssumeRole(t *testing.T) {
	var policy struct {
		Statement []struct {
			Effect    string `json:"Effect"`
			Principal struct {
				Service string `json:"Service"`
			} `json:"Principal"`
			Action string `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(trustPolicy), &policy); err != nil {
		t.Fatalf("trust policy is not a valid single-service policy: %v", err)
	}
	if len(policy.Statement) != 1 {
		t.Fatalf("trust policy has %d statements, want 1", len(policy.Statement))
	}
	statement := policy.Statement[0]
	if statement.Effect != "Allow" {
		t.Errorf("trust policy effect = %q, want Allow", statement.Effect)
	}
	if statement.Principal.Service != "fis.amazonaws.com" {
		t.Errorf("trust policy service principal = %q, want fis.amazonaws.com", statement.Principal.Service)
	}
	if statement.Action != "sts:AssumeRole" {
		t.Errorf("trust policy action = %q, want sts:AssumeRole", statement.Action)
	}
}
