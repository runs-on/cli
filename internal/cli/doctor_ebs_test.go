package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

func TestFlexRoleName(t *testing.T) {
	t.Parallel()

	if got := flexRoleName("runs-on"); got != "runs-on-flex-role" {
		t.Fatalf("unexpected role name %q", got)
	}
}

func TestMissingFlexRoleKMSPermissionsDetectsMissingActions(t *testing.T) {
	t.Parallel()

	keyARN := "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd"
	policy := `{
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"kms:Decrypt",
				"kms:DescribeKey",
				"kms:Encrypt",
				"kms:GenerateDataKey",
				"kms:GenerateDataKeyWithoutPlaintext",
				"kms:ReEncryptFrom",
				"kms:ReEncryptTo"
			],
			"Resource": "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd"
		}]
	}`

	missing, err := missingFlexRoleKMSPermissions([]string{policy}, keyARN)
	if err != nil {
		t.Fatalf("missingFlexRoleKMSPermissions returned error: %v", err)
	}
	if len(missing) != 1 || missing[0] != "kms:CreateGrant" {
		t.Fatalf("expected only kms:CreateGrant to be missing, got %v", missing)
	}
}

func TestMissingFlexRoleKMSPermissionsAcceptsCompletePolicy(t *testing.T) {
	t.Parallel()

	keyARN := "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd"
	policy := `{
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"kms:Decrypt",
					"kms:DescribeKey",
					"kms:Encrypt",
					"kms:GenerateDataKey",
					"kms:GenerateDataKeyWithoutPlaintext",
					"kms:ReEncryptFrom",
					"kms:ReEncryptTo"
				],
				"Resource": "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd"
			},
			{
				"Effect": "Allow",
				"Action": ["kms:CreateGrant"],
				"Resource": "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd",
				"Condition": {
					"Bool": {
						"kms:GrantIsForAWSResource": true
					}
				}
			}
		]
	}`

	missing, err := missingFlexRoleKMSPermissions([]string{policy}, keyARN)
	if err != nil {
		t.Fatalf("missingFlexRoleKMSPermissions returned error: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("expected no missing permissions, got %v", missing)
	}
}

func TestMissingFlexRoleKMSPermissionsMatchesWildcardResource(t *testing.T) {
	t.Parallel()

	keyARN := "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd"
	policy := `{
		"Statement": [{
			"Effect": "Allow",
			"Action": "kms:*",
			"Resource": "arn:aws:kms:us-west-2:411491144243:key/*"
		}]
	}`

	missing, err := missingFlexRoleKMSPermissions([]string{policy}, keyARN)
	if err != nil {
		t.Fatalf("missingFlexRoleKMSPermissions returned error: %v", err)
	}
	if len(missing) != 1 || missing[0] != "kms:CreateGrant" {
		t.Fatalf("expected kms:CreateGrant to require explicit grant condition, got %v", missing)
	}
}

func TestResolveEBSEncryptionKeyUsesConfiguredKey(t *testing.T) {
	t.Parallel()

	keyARN := "arn:aws:kms:us-east-1:123456789012:key/abc-123"
	kmsClient := &mockDoctorKMSClient{
		describeKey: func(_ context.Context, input *kms.DescribeKeyInput, _ ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
			switch aws.ToString(input.KeyId) {
			case "configured-key":
				return &kms.DescribeKeyOutput{
					KeyMetadata: &kmstypes.KeyMetadata{
						Arn:   aws.String(keyARN),
						KeyId: aws.String("abc-123"),
					},
				}, nil
			case awsManagedEBSAlias:
				return &kms.DescribeKeyOutput{
					KeyMetadata: &kmstypes.KeyMetadata{
						KeyId: aws.String("different-key"),
					},
				}, nil
			default:
				t.Fatalf("unexpected key id %q", aws.ToString(input.KeyId))
				return nil, nil
			}
		},
	}

	info, err := resolveEBSEncryptionKey(context.Background(), nil, kmsClient, "configured-key")
	if err != nil {
		t.Fatalf("resolveEBSEncryptionKey returned error: %v", err)
	}
	if info.KeyARN != keyARN {
		t.Fatalf("unexpected key ARN %q", info.KeyARN)
	}
	if info.Source != "stack-config EbsEncryptionKey" {
		t.Fatalf("unexpected source %q", info.Source)
	}
}

func TestResolveEBSEncryptionKeyUsesAccountDefault(t *testing.T) {
	t.Parallel()

	keyARN := "arn:aws:kms:us-east-1:123456789012:key/aws-managed"
	ec2Client := &mockDoctorEC2Client{
		getEbsEncryptionByDefault: func(context.Context, *ec2.GetEbsEncryptionByDefaultInput, ...func(*ec2.Options)) (*ec2.GetEbsEncryptionByDefaultOutput, error) {
			return &ec2.GetEbsEncryptionByDefaultOutput{EbsEncryptionByDefault: aws.Bool(true)}, nil
		},
		getEbsDefaultKmsKeyId: func(context.Context, *ec2.GetEbsDefaultKmsKeyIdInput, ...func(*ec2.Options)) (*ec2.GetEbsDefaultKmsKeyIdOutput, error) {
			return &ec2.GetEbsDefaultKmsKeyIdOutput{KmsKeyId: aws.String(awsManagedEBSAlias)}, nil
		},
	}
	kmsClient := &mockDoctorKMSClient{
		describeKey: func(_ context.Context, input *kms.DescribeKeyInput, _ ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
			return &kms.DescribeKeyOutput{
				KeyMetadata: &kmstypes.KeyMetadata{
					Arn:   aws.String(keyARN),
					KeyId: aws.String("aws-managed"),
				},
			}, nil
		},
	}

	info, err := resolveEBSEncryptionKey(context.Background(), ec2Client, kmsClient, "")
	if err != nil {
		t.Fatalf("resolveEBSEncryptionKey returned error: %v", err)
	}
	if info.DisplayName != awsManagedEBSAlias {
		t.Fatalf("expected display name %q, got %q", awsManagedEBSAlias, info.DisplayName)
	}
	if info.Source != "account EBS default encryption" {
		t.Fatalf("unexpected source %q", info.Source)
	}
}

func TestStackDoctorSkipsEBSCheckForFleet(t *testing.T) {
	t.Parallel()

	doctor := NewStackDoctor(&RunsOnConfig{Product: "fleet", StackName: "runs-on"})
	if err := doctor.checkEBSEncryptionKMS(context.Background()); err != nil {
		t.Fatalf("checkEBSEncryptionKMS returned error: %v", err)
	}
	if len(doctor.result.Checks) != 1 {
		t.Fatalf("expected one skipped check, got %+v", doctor.result.Checks)
	}
	if doctor.result.Checks[0].Status != "⏭️" {
		t.Fatalf("expected skipped status, got %+v", doctor.result.Checks[0])
	}
}

type mockDoctorEC2Client struct {
	getEbsEncryptionByDefault func(context.Context, *ec2.GetEbsEncryptionByDefaultInput, ...func(*ec2.Options)) (*ec2.GetEbsEncryptionByDefaultOutput, error)
	getEbsDefaultKmsKeyId     func(context.Context, *ec2.GetEbsDefaultKmsKeyIdInput, ...func(*ec2.Options)) (*ec2.GetEbsDefaultKmsKeyIdOutput, error)
}

func (m *mockDoctorEC2Client) GetEbsEncryptionByDefault(ctx context.Context, input *ec2.GetEbsEncryptionByDefaultInput, optFns ...func(*ec2.Options)) (*ec2.GetEbsEncryptionByDefaultOutput, error) {
	return m.getEbsEncryptionByDefault(ctx, input, optFns...)
}

func (m *mockDoctorEC2Client) GetEbsDefaultKmsKeyId(ctx context.Context, input *ec2.GetEbsDefaultKmsKeyIdInput, optFns ...func(*ec2.Options)) (*ec2.GetEbsDefaultKmsKeyIdOutput, error) {
	return m.getEbsDefaultKmsKeyId(ctx, input, optFns...)
}

type mockDoctorKMSClient struct {
	describeKey func(context.Context, *kms.DescribeKeyInput, ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
}

func (m *mockDoctorKMSClient) DescribeKey(ctx context.Context, input *kms.DescribeKeyInput, optFns ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
	return m.describeKey(ctx, input, optFns...)
}

func TestCollectRolePolicyDocumentsIncludesInlineAndManagedPolicies(t *testing.T) {
	t.Parallel()

	roleName := "runs-on-flex-role"
	iamClient := &mockDoctorIAMClient{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: aws.String(roleName)}}, nil
		},
		listRolePolicies: func(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
			return &iam.ListRolePoliciesOutput{PolicyNames: []string{"inline-policy"}}, nil
		},
		getRolePolicy: func(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
			return &iam.GetRolePolicyOutput{PolicyDocument: aws.String(`{"Statement":[]}`)}, nil
		},
		listAttachedRolePolicies: func(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
			return &iam.ListAttachedRolePoliciesOutput{
				AttachedPolicies: []iamtypes.AttachedPolicy{{PolicyArn: aws.String("arn:aws:iam::123456789012:policy/managed-policy")}},
			}, nil
		},
		getPolicy: func(context.Context, *iam.GetPolicyInput, ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
			return &iam.GetPolicyOutput{
				Policy: &iamtypes.Policy{DefaultVersionId: aws.String("v1")},
			}, nil
		},
		getPolicyVersion: func(context.Context, *iam.GetPolicyVersionInput, ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
			return &iam.GetPolicyVersionOutput{
				PolicyVersion: &iamtypes.PolicyVersion{Document: aws.String(`{"Statement":[]}`)},
			}, nil
		},
	}

	documents, err := collectRolePolicyDocuments(context.Background(), iamClient, roleName)
	if err != nil {
		t.Fatalf("collectRolePolicyDocuments returned error: %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("expected two policy documents, got %d", len(documents))
	}
}

type mockDoctorIAMClient struct {
	getRole                  func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	listRolePolicies         func(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	getRolePolicy            func(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	listAttachedRolePolicies func(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	getPolicy                func(context.Context, *iam.GetPolicyInput, ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	getPolicyVersion         func(context.Context, *iam.GetPolicyVersionInput, ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error)
}

func (m *mockDoctorIAMClient) GetRole(ctx context.Context, input *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return m.getRole(ctx, input, optFns...)
}

func (m *mockDoctorIAMClient) ListRolePolicies(ctx context.Context, input *iam.ListRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	return m.listRolePolicies(ctx, input, optFns...)
}

func (m *mockDoctorIAMClient) GetRolePolicy(ctx context.Context, input *iam.GetRolePolicyInput, optFns ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	return m.getRolePolicy(ctx, input, optFns...)
}

func (m *mockDoctorIAMClient) ListAttachedRolePolicies(ctx context.Context, input *iam.ListAttachedRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return m.listAttachedRolePolicies(ctx, input, optFns...)
}

func (m *mockDoctorIAMClient) GetPolicy(ctx context.Context, input *iam.GetPolicyInput, optFns ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	return m.getPolicy(ctx, input, optFns...)
}

func (m *mockDoctorIAMClient) GetPolicyVersion(ctx context.Context, input *iam.GetPolicyVersionInput, optFns ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
	return m.getPolicyVersion(ctx, input, optFns...)
}

func TestIAMResourceMatchesSupportsKeyWildcard(t *testing.T) {
	t.Parallel()

	keyARN := "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd"
	if !iamResourceMatches("arn:aws:kms:us-west-2:411491144243:key/*", keyARN) {
		t.Fatal("expected wildcard key resource to match")
	}
	if iamResourceMatches("arn:aws:kms:us-east-1:411491144243:key/*", keyARN) {
		t.Fatal("expected different region wildcard not to match")
	}
}

func TestIAMCreateGrantConditionMatchesStringTrue(t *testing.T) {
	t.Parallel()

	conditions := map[string]map[string]interface{}{
		"Bool": {"kms:GrantIsForAWSResource": "true"},
	}
	if !iamCreateGrantConditionMatches(conditions) {
		t.Fatal("expected string true condition to match")
	}
}

func TestMissingFlexRoleKMSPermissionsRejectsCreateGrantWithoutCondition(t *testing.T) {
	t.Parallel()

	keyARN := "arn:aws:kms:us-west-2:411491144243:key/35207482-6234-45c5-adfc-41184d40e7cd"
	policy := `{
		"Statement": [{
			"Effect": "Allow",
			"Action": ["kms:CreateGrant", "kms:Decrypt"],
			"Resource": "` + keyARN + `"
		}]
	}`

	missing, err := missingFlexRoleKMSPermissions([]string{policy}, keyARN)
	if err != nil {
		t.Fatalf("missingFlexRoleKMSPermissions returned error: %v", err)
	}
	if !containsString(missing, "kms:CreateGrant") {
		t.Fatalf("expected kms:CreateGrant to remain missing without condition, got %v", missing)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
