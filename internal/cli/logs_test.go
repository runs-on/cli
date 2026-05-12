package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestApplicationFilterPatternUsesJobIDByDefault(t *testing.T) {
	facts := &workflowJobFactsProvider{}
	facts.set(&workflowJobFacts{CurrentInstanceID: "i-123"})

	filterPattern, err := jobApplicationFilterPattern("42", facts, nil)
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if !strings.Contains(filterPattern, `$.job_id = "42"`) {
		t.Fatalf("expected job ID filter, got %q", filterPattern)
	}
	if !strings.Contains(filterPattern, `$.message = "*i-123*"`) {
		t.Fatalf("expected instance ID message filter, got %q", filterPattern)
	}
}

func TestApplicationFilterPatternUsesRunIDWhenRequested(t *testing.T) {
	facts := &workflowJobFactsProvider{}
	facts.set(&workflowJobFacts{
		RunID:             1234,
		CurrentInstanceID: "i-123",
	})

	filterPattern, err := jobApplicationFilterPattern("42", facts, []string{"run"})
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if !strings.Contains(filterPattern, `$.run_id = "1234"`) {
		t.Fatalf("expected run ID filter, got %q", filterPattern)
	}
	if strings.Contains(filterPattern, `$.job_id = "42"`) {
		t.Fatalf("did not expect job ID filter in run-scoped pattern, got %q", filterPattern)
	}
	if strings.Contains(filterPattern, `$.message = "*i-123*"`) {
		t.Fatalf("did not expect instance ID message filter in run-scoped pattern, got %q", filterPattern)
	}
}

func TestApplicationLogGroupIdentifierPrefersServiceLogGroup(t *testing.T) {
	outputs := &StackOutputs{
		ServiceLogGroupName: "/aws/ecs/runs-on-preview-v3/flexd",
	}

	logGroup, err := outputs.applicationLogGroupIdentifier()
	if err != nil {
		t.Fatalf("applicationLogGroupIdentifier returned error: %v", err)
	}
	if logGroup != outputs.ServiceLogGroupName {
		t.Fatalf("expected service log group %q, got %q", outputs.ServiceLogGroupName, logGroup)
	}
}

func TestApplicationLogGroupIdentifierRequiresServiceLogGroup(t *testing.T) {
	if _, err := (&StackOutputs{}).applicationLogGroupIdentifier(); err == nil {
		t.Fatal("expected applicationLogGroupIdentifier to fail without a service log group")
	}
}

func TestRefreshJobLookupHandlesMissingRow(t *testing.T) {
	facts := &workflowJobFactsProvider{
		jobs: &mockWorkflowJobsClient{
			getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{}, nil
			},
		},
		tableName: "runs-on-workflow-jobs",
		jobID:     "42",
		facts: &workflowJobFacts{
			CurrentInstanceID: "i-existing",
			RunID:             1234,
		},
	}

	facts.refresh(context.Background())
	if got := facts.currentInstanceID(); got != "" {
		t.Fatalf("expected empty instance ID, got %q", got)
	}
	if got := facts.runID(); got != 0 {
		t.Fatalf("expected run ID to be cleared, got %d", got)
	}
}

func TestStreamedLogSessionUsesExpectedJobAndStackFilters(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	jobsClient := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
					JobID:     42,
					RunID:     1234,
					CreatedAt: &createdAt,
					ActiveAttempt: &struct {
						InstanceID string `dynamodbav:"instance_id"`
					}{InstanceID: "i-active"},
				}),
			}, nil
		},
	}
	cwl := &mockCloudWatchLogsClient{}
	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/flexd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := &workflowJobFactsProvider{
		jobs:      jobsClient,
		tableName: "workflow-jobs",
		jobID:     "42",
	}

	if err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}

	var sawInstance, sawApplication bool
	for _, input := range cwl.inputs {
		switch {
		case aws.ToString(input.LogStreamNamePrefix) == "i-active/":
			sawInstance = true
		case strings.Contains(aws.ToString(input.FilterPattern), `$.job_id = "42"`):
			sawApplication = true
		}
	}
	if !sawInstance || !sawApplication {
		t.Fatalf("expected job stream to register instance and application filters, got %#v", cwl.inputs)
	}

	cwl.inputs = nil
	applicationStreamer := &applicationLogStreamer{
		cwl:     cwl,
		outputs: streamer.outputs,
	}
	if err := applicationStreamer.Stream(context.Background(), &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("application Stream returned error: %v", err)
	}
	if len(cwl.inputs) != 1 {
		t.Fatalf("expected one stack application log fetch, got %d", len(cwl.inputs))
	}
	if got := aws.ToString(cwl.inputs[0].FilterPattern); got != "" {
		t.Fatalf("expected stack application logs to use empty filter, got %q", got)
	}
}

func TestNoColorFlagIsLogCommandOnly(t *testing.T) {
	rootHelp := rootCommandHelp(t, "--help")
	if strings.Contains(rootHelp, "--no-color") {
		t.Fatalf("did not expect root help to advertise --no-color:\n%s", rootHelp)
	}

	logsHelp := rootCommandHelp(t, "logs", "--help")
	assertHelpContains(t, logsHelp, "--no-color")
	assertHelpContains(t, logsHelp, "Disable color output for streamed logs")

	stackLogsHelp := rootCommandHelp(t, "stack", "logs", "--help")
	assertHelpContains(t, stackLogsHelp, "--no-color")
	assertHelpContains(t, stackLogsHelp, "Disable color output for streamed logs")
}

func rootCommandHelp(t *testing.T, args ...string) string {
	t.Helper()

	cmd := NewRootCmd(&Stack{})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("command help failed for args %v: %v", args, err)
	}
	return output.String()
}

func assertHelpContains(t *testing.T, help, want string) {
	t.Helper()
	if !strings.Contains(help, want) {
		t.Fatalf("expected help to contain %q:\n%s", want, help)
	}
}
