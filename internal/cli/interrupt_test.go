package cli

import (
	"encoding/json"
	"testing"
)

// TestTrustPolicyIsValidForIAM guards against the regression where the FIS
// trust policy nested the service principal as [["fis.amazonaws.com"]], which
// IAM rejects with MalformedPolicyDocument. The Principal.Service must be a
// plain string (or flat array) of service principals.
func TestTrustPolicyIsValidForIAM(t *testing.T) {
	var doc struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string `json:"Effect"`
			Action    string `json:"Action"`
			Principal struct {
				Service json.RawMessage `json:"Service"`
			} `json:"Principal"`
		} `json:"Statement"`
	}

	if err := json.Unmarshal([]byte(trustPolicy), &doc); err != nil {
		t.Fatalf("trustPolicy is not valid JSON: %v", err)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(doc.Statement))
	}

	svc := doc.Statement[0].Principal.Service
	// Must decode either as a string or a flat []string — never a nested array.
	var asString string
	if err := json.Unmarshal(svc, &asString); err == nil {
		if asString != "fis.amazonaws.com" {
			t.Fatalf("unexpected service principal: %q", asString)
		}
		return
	}
	var asSlice []string
	if err := json.Unmarshal(svc, &asSlice); err != nil {
		t.Fatalf("Principal.Service must be a string or flat []string, got: %s", string(svc))
	}
	if len(asSlice) != 1 || asSlice[0] != "fis.amazonaws.com" {
		t.Fatalf("unexpected service principals: %v", asSlice)
	}
}
