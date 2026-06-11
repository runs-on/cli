package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

const awsManagedEBSAlias = "alias/aws/ebs"

var requiredFlexRoleKMSActions = []string{
	"kms:Decrypt",
	"kms:DescribeKey",
	"kms:Encrypt",
	"kms:GenerateDataKey",
	"kms:GenerateDataKeyWithoutPlaintext",
	"kms:ReEncryptFrom",
	"kms:ReEncryptTo",
}

type doctorEC2API interface {
	GetEbsEncryptionByDefault(ctx context.Context, params *ec2.GetEbsEncryptionByDefaultInput, optFns ...func(*ec2.Options)) (*ec2.GetEbsEncryptionByDefaultOutput, error)
	GetEbsDefaultKmsKeyId(ctx context.Context, params *ec2.GetEbsDefaultKmsKeyIdInput, optFns ...func(*ec2.Options)) (*ec2.GetEbsDefaultKmsKeyIdOutput, error)
}

type doctorKMSAPI interface {
	DescribeKey(ctx context.Context, params *kms.DescribeKeyInput, optFns ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
}

type doctorIAMAPI interface {
	GetRole(ctx context.Context, params *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	ListRolePolicies(ctx context.Context, params *iam.ListRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	GetRolePolicy(ctx context.Context, params *iam.GetRolePolicyInput, optFns ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	ListAttachedRolePolicies(ctx context.Context, params *iam.ListAttachedRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	GetPolicy(ctx context.Context, params *iam.GetPolicyInput, optFns ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	GetPolicyVersion(ctx context.Context, params *iam.GetPolicyVersionInput, optFns ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error)
}

type ebsEncryptionKeyInfo struct {
	DisplayName string
	KeyARN      string
	KeyID       string
	Source      string
}

type iamPolicyDocument struct {
	Statement []iamPolicyStatement `json:"Statement"`
}

type iamPolicyStatement struct {
	Effect    string                            `json:"Effect"`
	Action    any                               `json:"Action"`
	Resource  any                               `json:"Resource"`
	Condition map[string]map[string]interface{} `json:"Condition"`
}

func flexRoleName(stackName string) string {
	return fmt.Sprintf("%s-flex-role", strings.TrimSpace(stackName))
}

func loadFlexStackConfigSecret(ctx context.Context, client stackConfigSecretAPI, stackName string) (stackConfigSecretValue, error) {
	if client == nil {
		return stackConfigSecretValue{}, fmt.Errorf("stack config client is required")
	}
	stackName = strings.TrimSpace(stackName)
	if stackName == "" {
		return stackConfigSecretValue{}, fmt.Errorf("stack name is required")
	}

	output, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(stackConfigSecretID(stackName)),
	})
	if err != nil {
		return stackConfigSecretValue{}, fmt.Errorf("load stack config secret: %w", err)
	}
	if output.SecretString == nil || strings.TrimSpace(*output.SecretString) == "" {
		return stackConfigSecretValue{}, fmt.Errorf("stack config secret is empty")
	}

	var secret stackConfigSecretValue
	if err := json.Unmarshal([]byte(*output.SecretString), &secret); err != nil {
		return stackConfigSecretValue{}, fmt.Errorf("parse stack config: %w", err)
	}
	return secret, nil
}

func resolveEBSEncryptionKey(ctx context.Context, ec2Client doctorEC2API, kmsClient doctorKMSAPI, configuredKey string) (*ebsEncryptionKeyInfo, error) {
	configuredKey = strings.TrimSpace(configuredKey)
	if configuredKey != "" {
		info, err := describeKMSKey(ctx, kmsClient, configuredKey)
		if err != nil {
			return nil, err
		}
		info.Source = "stack-config EbsEncryptionKey"
		return info, nil
	}

	if ec2Client == nil {
		return nil, fmt.Errorf("ec2 client is required")
	}
	if kmsClient == nil {
		return nil, fmt.Errorf("kms client is required")
	}

	enabledOutput, err := ec2Client.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{})
	if err != nil {
		return nil, fmt.Errorf("get EBS encryption by default: %w", err)
	}
	if !aws.ToBool(enabledOutput.EbsEncryptionByDefault) {
		return nil, fmt.Errorf("EbsEncryptionKey is not set in stack-config and account EBS default encryption is disabled")
	}

	defaultKeyOutput, err := ec2Client.GetEbsDefaultKmsKeyId(ctx, &ec2.GetEbsDefaultKmsKeyIdInput{})
	if err != nil {
		return nil, fmt.Errorf("get EBS default KMS key: %w", err)
	}
	defaultKeyID := strings.TrimSpace(aws.ToString(defaultKeyOutput.KmsKeyId))
	if defaultKeyID == "" {
		return nil, fmt.Errorf("account EBS default encryption is enabled but no default KMS key was returned")
	}

	info, err := describeKMSKey(ctx, kmsClient, defaultKeyID)
	if err != nil {
		return nil, err
	}
	info.Source = "account EBS default encryption"
	return info, nil
}

func describeKMSKey(ctx context.Context, kmsClient doctorKMSAPI, keyRef string) (*ebsEncryptionKeyInfo, error) {
	if kmsClient == nil {
		return nil, fmt.Errorf("kms client is required")
	}
	keyRef = strings.TrimSpace(keyRef)
	if keyRef == "" {
		return nil, fmt.Errorf("kms key reference is required")
	}

	output, err := kmsClient.DescribeKey(ctx, &kms.DescribeKeyInput{
		KeyId: aws.String(keyRef),
	})
	if err != nil {
		return nil, fmt.Errorf("describe kms key %q: %w", keyRef, err)
	}
	if output.KeyMetadata == nil {
		return nil, fmt.Errorf("describe kms key %q returned no metadata", keyRef)
	}

	info := &ebsEncryptionKeyInfo{
		DisplayName: aws.ToString(output.KeyMetadata.Arn),
		KeyARN:      aws.ToString(output.KeyMetadata.Arn),
		KeyID:       aws.ToString(output.KeyMetadata.KeyId),
	}
	if info.KeyARN == "" || info.KeyID == "" {
		return nil, fmt.Errorf("describe kms key %q returned incomplete metadata", keyRef)
	}

	managedOutput, err := kmsClient.DescribeKey(ctx, &kms.DescribeKeyInput{
		KeyId: aws.String(awsManagedEBSAlias),
	})
	if err == nil && managedOutput.KeyMetadata != nil {
		if aws.ToString(managedOutput.KeyMetadata.KeyId) == info.KeyID {
			info.DisplayName = awsManagedEBSAlias
		}
	}

	return info, nil
}

func collectRolePolicyDocuments(ctx context.Context, iamClient doctorIAMAPI, roleName string) ([]string, error) {
	if iamClient == nil {
		return nil, fmt.Errorf("iam client is required")
	}
	roleName = strings.TrimSpace(roleName)
	if roleName == "" {
		return nil, fmt.Errorf("role name is required")
	}

	if _, err := iamClient.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)}); err != nil {
		return nil, fmt.Errorf("get role %q: %w", roleName, err)
	}

	var documents []string

	inlineOutput, err := iamClient.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		return nil, fmt.Errorf("list inline policies for role %q: %w", roleName, err)
	}
	for _, policyName := range inlineOutput.PolicyNames {
		policyOutput, err := iamClient.GetRolePolicy(ctx, &iam.GetRolePolicyInput{
			RoleName:   aws.String(roleName),
			PolicyName: aws.String(policyName),
		})
		if err != nil {
			return nil, fmt.Errorf("get inline policy %q for role %q: %w", policyName, roleName, err)
		}
		document, err := decodeIAMPolicyDocument(aws.ToString(policyOutput.PolicyDocument))
		if err != nil {
			return nil, fmt.Errorf("decode inline policy %q for role %q: %w", policyName, roleName, err)
		}
		documents = append(documents, document)
	}

	attachedOutput, err := iamClient.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		return nil, fmt.Errorf("list attached policies for role %q: %w", roleName, err)
	}
	for _, attachedPolicy := range attachedOutput.AttachedPolicies {
		policyARN := strings.TrimSpace(aws.ToString(attachedPolicy.PolicyArn))
		if policyARN == "" {
			continue
		}
		policyOutput, err := iamClient.GetPolicy(ctx, &iam.GetPolicyInput{
			PolicyArn: aws.String(policyARN),
		})
		if err != nil {
			return nil, fmt.Errorf("get policy %q: %w", policyARN, err)
		}
		versionID := strings.TrimSpace(aws.ToString(policyOutput.Policy.DefaultVersionId))
		if versionID == "" {
			continue
		}
		versionOutput, err := iamClient.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{
			PolicyArn: aws.String(policyARN),
			VersionId: aws.String(versionID),
		})
		if err != nil {
			return nil, fmt.Errorf("get policy version %q for %q: %w", versionID, policyARN, err)
		}
		document, err := decodeIAMPolicyDocument(aws.ToString(versionOutput.PolicyVersion.Document))
		if err != nil {
			return nil, fmt.Errorf("decode policy %q: %w", policyARN, err)
		}
		documents = append(documents, document)
	}

	return documents, nil
}

func decodeIAMPolicyDocument(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("policy document is empty")
	}
	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		return "", err
	}
	return decoded, nil
}

func missingFlexRoleKMSPermissions(documents []string, keyARN string) ([]string, error) {
	missing := append([]string{}, requiredFlexRoleKMSActions...)
	missing = append(missing, "kms:CreateGrant")

	for _, document := range documents {
		var policy iamPolicyDocument
		if err := json.Unmarshal([]byte(document), &policy); err != nil {
			return nil, fmt.Errorf("parse iam policy document: %w", err)
		}
		for _, statement := range policy.Statement {
			if !strings.EqualFold(strings.TrimSpace(statement.Effect), "Allow") {
				continue
			}
			if !iamStatementMatchesKeyResource(statement.Resource, keyARN) {
				continue
			}

			actions := normalizeIAMPolicyStrings(statement.Action)
			for i := len(missing) - 1; i >= 0; i-- {
				action := missing[i]
				if action == "kms:CreateGrant" {
					if iamActionAllowed(actions, action) && iamCreateGrantConditionMatches(statement.Condition) {
						missing = append(missing[:i], missing[i+1:]...)
					}
					continue
				}
				if iamActionAllowed(actions, action) {
					missing = append(missing[:i], missing[i+1:]...)
				}
			}
		}
	}

	return missing, nil
}

func normalizeIAMPolicyStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{strings.TrimSpace(typed)}
	case []any:
		var values []string
		for _, item := range typed {
			values = append(values, normalizeIAMPolicyStrings(item)...)
		}
		return values
	case []string:
		var values []string
		for _, item := range typed {
			item = strings.TrimSpace(item)
			if item != "" {
				values = append(values, item)
			}
		}
		return values
	default:
		return nil
	}
}

func iamStatementMatchesKeyResource(resourceValue any, keyARN string) bool {
	for _, resource := range normalizeIAMPolicyStrings(resourceValue) {
		if iamResourceMatches(resource, keyARN) {
			return true
		}
	}
	return false
}

func iamResourceMatches(resource, keyARN string) bool {
	resource = strings.TrimSpace(resource)
	keyARN = strings.TrimSpace(keyARN)
	if resource == "" || keyARN == "" {
		return false
	}
	if resource == "*" {
		return true
	}
	if resource == keyARN {
		return true
	}
	if strings.Contains(resource, "*") {
		prefix := strings.SplitN(resource, "*", 2)[0]
		return strings.HasPrefix(keyARN, prefix)
	}
	return false
}

func iamActionAllowed(actions []string, required string) bool {
	for _, action := range actions {
		action = strings.TrimSpace(action)
		if action == "" {
			continue
		}
		if action == "*" || action == "kms:*" {
			return true
		}
		if strings.EqualFold(action, required) {
			return true
		}
		if strings.HasSuffix(action, "*") {
			prefix := strings.TrimSuffix(action, "*")
			if strings.HasPrefix(required, prefix) {
				return true
			}
		}
	}
	return false
}

func iamCreateGrantConditionMatches(conditions map[string]map[string]interface{}) bool {
	if len(conditions) == 0 {
		return false
	}
	boolConditions, ok := conditions["Bool"]
	if !ok {
		return false
	}
	value, ok := boolConditions["kms:GrantIsForAWSResource"]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func (d *StackDoctor) checkEBSEncryptionKMS(ctx context.Context) error {
	if strings.EqualFold(strings.TrimSpace(d.config.Product), "fleet") {
		fmt.Print("Checking EBS encryption KMS permissions...")
		d.skipCheck("EBS encryption KMS permissions", "Skipped - Fleet stacks do not use the flex role")
		return nil
	}

	roleName := flexRoleName(d.config.StackName)
	fmt.Printf("Checking EBS encryption KMS permissions (%s)...", roleName)

	secretsClient := secretsmanager.NewFromConfig(d.cfg)
	secret, err := loadFlexStackConfigSecret(ctx, secretsClient, d.config.StackName)
	if err != nil {
		return d.failCheck("EBS encryption KMS permissions", "Failed to load stack-config secret", err)
	}

	ec2Client := ec2.NewFromConfig(d.cfg)
	kmsClient := kms.NewFromConfig(d.cfg)
	keyInfo, err := resolveEBSEncryptionKey(ctx, ec2Client, kmsClient, secret.EbsEncryptionKey)
	if err != nil {
		return d.failCheck("EBS encryption KMS permissions", "Failed to resolve EBS encryption key", err)
	}

	iamClient := iam.NewFromConfig(d.cfg)
	documents, err := collectRolePolicyDocuments(ctx, iamClient, roleName)
	if err != nil {
		return d.failCheck("EBS encryption KMS permissions", fmt.Sprintf("Failed to load IAM policies for role %s", roleName), err)
	}

	missing, err := missingFlexRoleKMSPermissions(documents, keyInfo.KeyARN)
	if err != nil {
		return d.failCheck("EBS encryption KMS permissions", "Failed to evaluate IAM policy permissions", err)
	}

	result := fmt.Sprintf("key: %s (source: %s)", keyInfo.DisplayName, keyInfo.Source)
	if len(missing) > 0 {
		message := fmt.Sprintf("%s; missing permissions on %s: %s", result, roleName, strings.Join(missing, ", "))
		d.addCheck("EBS encryption KMS permissions", "❌", message, nil)
		d.printCheckResult("❌", message)
		return fmt.Errorf("flex role is missing KMS permissions: %s", strings.Join(missing, ", "))
	}

	d.addCheck("EBS encryption KMS permissions", "✅", fmt.Sprintf("%s; role %s has required KMS permissions", result, roleName), nil)
	d.printCheckResult("✅", fmt.Sprintf("key: %s", keyInfo.DisplayName))
	return nil
}
