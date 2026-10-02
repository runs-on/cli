package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/fis"
	"github.com/aws/aws-sdk-go-v2/service/fis/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
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

// fakeFIS reports a fixed experiment status and, like the real clients, fails
// calls made with a cancelled context. It also serves the IAM calls for the
// FIS role, which exists unless noRole is set. It cancels the command's
// context during the call named cancelDuring, as a Ctrl-C would while that
// request is in flight, after AWS has acted on the request.
type fakeFIS struct {
	status            types.ExperimentStatus
	noRole            bool
	cancelDuring      string
	cancel            context.CancelFunc
	roleWithoutPolicy bool
	createdTemplates  []string
	startedTemplates  []string
	deletedTemplates  []string
}

func (f *fakeFIS) respond(ctx context.Context, call string) error {
	if call == f.cancelDuring {
		f.cancel()
	}
	return ctx.Err()
}

func (f *fakeFIS) CreateExperimentTemplate(ctx context.Context, _ *fis.CreateExperimentTemplateInput, _ ...func(*fis.Options)) (*fis.CreateExperimentTemplateOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.createdTemplates = append(f.createdTemplates, "EXT1")
	if err := f.respond(ctx, "CreateExperimentTemplate"); err != nil {
		return nil, err
	}
	return &fis.CreateExperimentTemplateOutput{ExperimentTemplate: &types.ExperimentTemplate{Id: aws.String("EXT1")}}, nil
}

func (f *fakeFIS) StartExperiment(ctx context.Context, params *fis.StartExperimentInput, _ ...func(*fis.Options)) (*fis.StartExperimentOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.startedTemplates = append(f.startedTemplates, aws.ToString(params.ExperimentTemplateId))
	if err := f.respond(ctx, "StartExperiment"); err != nil {
		return nil, err
	}
	return &fis.StartExperimentOutput{Experiment: &types.Experiment{
		Id:                   aws.String("EXP1"),
		ExperimentTemplateId: params.ExperimentTemplateId,
	}}, nil
}

func (f *fakeFIS) GetExperiment(ctx context.Context, params *fis.GetExperimentInput, _ ...func(*fis.Options)) (*fis.GetExperimentOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &fis.GetExperimentOutput{Experiment: &types.Experiment{
		Id:    params.Id,
		State: &types.ExperimentState{Status: f.status},
	}}, nil
}

func (f *fakeFIS) DeleteExperimentTemplate(ctx context.Context, params *fis.DeleteExperimentTemplateInput, _ ...func(*fis.Options)) (*fis.DeleteExperimentTemplateOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.deletedTemplates = append(f.deletedTemplates, aws.ToString(params.Id))
	return &fis.DeleteExperimentTemplateOutput{}, nil
}

func (f *fakeFIS) GetRole(ctx context.Context, _ *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.noRole {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	return &iam.GetRoleOutput{}, nil
}

func (f *fakeFIS) CreateRole(ctx context.Context, params *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.noRole = false
	f.roleWithoutPolicy = true
	if err := f.respond(ctx, "CreateRole"); err != nil {
		return nil, err
	}
	return &iam.CreateRoleOutput{Role: &iamtypes.Role{RoleName: params.RoleName, Arn: aws.String("arn:aws:iam::123456789012:role/" + fisRoleName)}}, nil
}

func (f *fakeFIS) PutRolePolicy(ctx context.Context, _ *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.roleWithoutPolicy = false
	return &iam.PutRolePolicyOutput{}, nil
}

func TestCreateSpotInterruptionCancelledTracksWhatFISCreated(t *testing.T) {
	tests := []struct {
		name         string
		noRole       bool
		cancelDuring string
		wantStarted  bool
	}{
		{name: "while creating the FIS role", noRole: true, cancelDuring: "CreateRole"},
		{name: "while creating the template", cancelDuring: "CreateExperimentTemplate"},
		{name: "while starting the experiment", cancelDuring: "StartExperiment", wantStarted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &fakeFIS{noRole: tt.noRole, cancelDuring: tt.cancelDuring, cancel: cancel}

			experiment, err := createSpotInterruption(ctx, client, client, "123456789012", []string{"i-1"}, time.Second, "us-east-1", discardLogger)

			if client.roleWithoutPolicy {
				t.Errorf("FIS role was created without its policy")
			}
			if started := len(client.startedTemplates) > 0; started != tt.wantStarted {
				t.Fatalf("experiment started = %v, want %v", started, tt.wantStarted)
			}
			if tt.wantStarted {
				if err != nil || experiment == nil || aws.ToString(experiment.Id) != "EXP1" {
					t.Errorf("createSpotInterruption() = %+v, %v; want the started experiment EXP1", experiment, err)
				}
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("createSpotInterruption() error = %v, want context.Canceled", err)
			}
			if !slices.Equal(client.deletedTemplates, client.createdTemplates) {
				t.Errorf("deleted templates = %v, want the created %v", client.deletedTemplates, client.createdTemplates)
			}
		})
	}
}

func TestMonitorExperimentCancelledDeletesTemplate(t *testing.T) {
	const delay = 30 * time.Second
	tests := []struct {
		name           string
		status         types.ExperimentStatus
		cancelAfter    time.Duration
		wantNoticeSent bool
	}{
		{name: "while the experiment runs", status: types.ExperimentStatusRunning, cancelAfter: 42 * time.Second},
		// FIS may report Completed once it has scheduled the notice.
		{name: "before the notice is due", status: types.ExperimentStatusCompleted, cancelAfter: 20 * time.Second},
		{name: "while waiting for the shutdown", status: types.ExperimentStatusCompleted, cancelAfter: 42 * time.Second, wantNoticeSent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				client := &fakeFIS{status: tt.status}
				startedAt := time.Now()
				experiment := &types.Experiment{
					Id:                   aws.String("EXP1"),
					ExperimentTemplateId: aws.String("EXT1"),
					StartTime:            aws.Time(startedAt),
					State:                &types.ExperimentState{Status: types.ExperimentStatusInitiating},
				}

				type result struct {
					shutdownAt time.Time
					err        error
				}
				done := make(chan result, 1)
				go func() {
					shutdownAt, err := monitorExperiment(ctx, client, experiment, delay, io.Discard, discardLogger)
					done <- result{shutdownAt, err}
				}()
				time.Sleep(tt.cancelAfter)
				cancel()
				cancelledAt := time.Now()
				got := <-done

				if !errors.Is(got.err, context.Canceled) {
					t.Errorf("monitorExperiment() error = %v, want context.Canceled", got.err)
				}
				if waited := time.Since(cancelledAt); waited > 0 {
					t.Errorf("monitorExperiment() returned %v after cancellation, want immediately", waited)
				}
				if noticeSent := !got.shutdownAt.IsZero(); noticeSent != tt.wantNoticeSent {
					t.Errorf("monitorExperiment() reported notice sent = %v, want %v", noticeSent, tt.wantNoticeSent)
				}
				if earliest := startedAt.Add(delay + spotNoticeLead); tt.wantNoticeSent && got.shutdownAt.Before(earliest) {
					t.Errorf("monitorExperiment() shutdown at %v, want no earlier than %v", got.shutdownAt, earliest)
				}
				if want := []string{"EXT1"}; !slices.Equal(client.deletedTemplates, want) {
					t.Errorf("deleted templates = %v, want %v", client.deletedTemplates, want)
				}
			})
		})
	}
}
