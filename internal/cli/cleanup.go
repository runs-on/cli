package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/cobra"
)

func NewCleanupCmd(stack *Stack) *cobra.Command {
	var includeDefaultBranch bool
	var dryRun bool
	var yes bool

	cmd := &cobra.Command{
		Use:   "cleanup JOB_URL",
		Short: "Delete the caches and sticky-disk snapshots associated with a job's ref",
		Long: `Deletes everything the RunsOn stack cached for the ref a job ran on:

  - classic cache objects       (s3://<cache-bucket>/cache/v1/<org>/<repo>/<ref>/...)
  - isolated cache objects      (s3://<cache-bucket>/scoped-cache/<ownerID>/<repoID>/<scope>/...)
  - sticky-disk snapshots       (EBS snapshots tagged for the repo and branch scope)

Pull-request runs clean each associated pull request's refs/pull/N/merge
scope. Other runs clean the run's head ref as both a branch and a tag (the
runs API records only the short name); everything deleted here is
re-creatable cache data, and the plan is shown for confirmation first. Pass
--include-default-branch to also clean the repository default branch (the
lineage branch and PR jobs restore from); pull_request_target and
workflow_run jobs cache under the default branch, so cleaning them requires
that flag.

Job metadata is fetched with the GitHub CLI (gh); run 'gh auth login' first.
Repo-wide user caches under cache/repo/<org>/<repo> are not touched: they are
not scoped to a ref.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobRef, err := requireGitHubJobURL(args[0])
			if err != nil {
				return err
			}
			config, err := stack.getStackOutputs(cmd)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			diagnostics, err := newJobDiagnosticsResolver(config).Resolve(ctx, args[0])
			if err != nil {
				return err
			}
			if err := validateCleanupJob(diagnostics, config, jobRef); err != nil {
				return err
			}
			target, err := resolveCleanupTarget(ctx, ghCLIRunner{}, jobRef, includeDefaultBranch)
			if err != nil {
				return err
			}

			cleaner := &stackCleaner{
				s3:          s3.NewFromConfig(config.AWSConfig),
				ec2:         ec2.NewFromConfig(config.AWSConfig),
				cacheBucket: config.CacheBucket,
				stackName:   config.StackName,
				out:         cmd.OutOrStdout(),
			}

			plan, err := cleaner.plan(ctx, target)
			if err != nil {
				return err
			}
			plan.print(cmd.OutOrStdout(), target)
			if plan.empty() {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to clean up.")
				return nil
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "Dry run: nothing was deleted.")
				return nil
			}
			if !yes && !confirm(cmd, fmt.Sprintf("Delete %d cache object(s) and %d snapshot(s)?", plan.objectCount(), len(plan.snapshots))) {
				fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
				return nil
			}
			return cleaner.execute(ctx, plan)
		},
	}

	cmd.Flags().BoolVar(&includeDefaultBranch, "include-default-branch", false, "also delete caches and snapshots for the repository default branch")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be deleted without deleting anything")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func validateCleanupJob(diagnostics *jobDiagnosticsResponse, config *RunsOnConfig, job parsedGitHubJobURL) error {
	if diagnostics == nil || diagnostics.Local == nil {
		return fmt.Errorf("job %d was not found in the selected RunsOn stack %q", job.JobID, config.StackName)
	}
	if diagnostics.Local.WorkflowJobID != 0 && diagnostics.Local.WorkflowJobID != job.JobID {
		return fmt.Errorf("selected stack returned workflow job %d while validating job %d", diagnostics.Local.WorkflowJobID, job.JobID)
	}
	if diagnostics.Local.WorkflowRunID != 0 && diagnostics.Local.WorkflowRunID != job.RunID {
		return fmt.Errorf("selected stack returned workflow run %d while validating run %d", diagnostics.Local.WorkflowRunID, job.RunID)
	}
	return diagnostics.validateStackProduct(config.Product, config.StackName, job.JobID)
}

// ghAPIRunner fetches GitHub API paths; implemented by the gh CLI and faked
// in tests. Missing resources return an error wrapping errGHNotFound so
// callers can distinguish definitive absence from transient failures.
type ghAPIRunner interface {
	Get(ctx context.Context, host, path string) ([]byte, error)
}

var errGHNotFound = fmt.Errorf("not found")

type ghCLIRunner struct{}

func (ghCLIRunner) Get(ctx context.Context, host, path string) ([]byte, error) {
	args := []string{"api"}
	if host = strings.TrimSpace(host); host != "" && !strings.EqualFold(host, "github.com") {
		args = append(args, "--hostname", host)
	}
	args = append(args, path)
	output, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		if _, lookupErr := exec.LookPath("gh"); lookupErr != nil {
			return nil, fmt.Errorf("cleanup requires the GitHub CLI; install gh and run `gh auth login` with repository read access")
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		if strings.Contains(message, "HTTP 404") {
			return nil, fmt.Errorf("%s: %w", path, errGHNotFound)
		}
		return nil, fmt.Errorf("GitHub CLI could not fetch %s; run `gh auth login` with repository read access: %s", path, message)
	}
	return output, nil
}

// resolveCleanupTarget derives the refs to clean from the job's workflow run
// and repository metadata. Everything cleaned here is re-creatable cache data
// with automatic expiry (S3 lifecycle rules, snapshot housekeeping), so
// resolution prefers cleaning slightly too much over refusing: the plan is
// printed and confirmed before anything is deleted.
func resolveCleanupTarget(ctx context.Context, github ghAPIRunner, job parsedGitHubJobURL, includeDefaultBranch bool) (cleanupTarget, error) {
	var repo struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"owner"`
		DefaultBranch string `json:"default_branch"`
	}
	repoBody, err := github.Get(ctx, job.Host, fmt.Sprintf("repos/%s/%s", job.Owner, job.Repo))
	if err != nil {
		return cleanupTarget{}, err
	}
	if err := json.Unmarshal(repoBody, &repo); err != nil {
		return cleanupTarget{}, fmt.Errorf("decode repository metadata: %w", err)
	}
	// GitHub resolves repo slugs case-insensitively, but cache keys and
	// snapshot tags are written with the canonical casing, so the canonical
	// slug is used. A repo renamed or transferred since the run keeps
	// historical data under its old slug and owner ID; that data expires on
	// its own via the bucket lifecycle and snapshot housekeeping.
	var warnings []string
	urlSlug := job.Owner + "/" + job.Repo
	repoSlug := urlSlug
	if repo.Owner.Login != "" && repo.Name != "" {
		repoSlug = repo.Owner.Login + "/" + repo.Name
	}
	if !strings.EqualFold(repoSlug, urlSlug) {
		warnings = append(warnings, fmt.Sprintf("repository is now %s (URL says %s, likely renamed/transferred): historical data under the old slug is left to expire on its own", repoSlug, urlSlug))
	}

	var run struct {
		HeadBranch   string `json:"head_branch"`
		HeadSHA      string `json:"head_sha"`
		Event        string `json:"event"`
		PullRequests []struct {
			Number int64 `json:"number"`
		} `json:"pull_requests"`
	}
	runBody, err := github.Get(ctx, job.Host, fmt.Sprintf("repos/%s/%s/actions/runs/%d", job.Owner, job.Repo, job.RunID))
	if err != nil {
		return cleanupTarget{}, err
	}
	if err := json.Unmarshal(runBody, &run); err != nil {
		return cleanupTarget{}, fmt.Errorf("decode workflow run metadata: %w", err)
	}

	// Collect the cache refs the run wrote under and both products'
	// sticky-disk snapshot scopes: both products scope every pull_request*
	// snapshot to refs/pull/N/merge; for other runs Flex tags snapshots with
	// the raw short head ref name while Fleet uses the branch short name or
	// the full tag ref.
	var refs runRefs
	isPullRequestTarget := run.Event == "pull_request_target"
	isPullRequest := strings.HasPrefix(run.Event, "pull_request") && !isPullRequestTarget
	var pullNumbers []int64
	if isPullRequest || isPullRequestTarget {
		for _, pr := range run.PullRequests {
			if pr.Number > 0 {
				pullNumbers = append(pullNumbers, pr.Number)
			}
		}
		if len(pullNumbers) == 0 && run.HeadSHA != "" {
			// Fork PR runs report an empty pull_requests list; recover the PR
			// numbers from the head commit instead.
			var pulls []struct {
				Number int64 `json:"number"`
			}
			pullsBody, err := github.Get(ctx, job.Host, fmt.Sprintf("repos/%s/%s/commits/%s/pulls", job.Owner, job.Repo, run.HeadSHA))
			if err != nil {
				return cleanupTarget{}, err
			}
			if err := json.Unmarshal(pullsBody, &pulls); err != nil {
				return cleanupTarget{}, fmt.Errorf("decode associated pull requests: %w", err)
			}
			for _, pr := range pulls {
				if pr.Number > 0 {
					pullNumbers = append(pullNumbers, pr.Number)
				}
			}
		}
	}
	switch {
	case isPullRequestTarget:
		// pull_request_target runs execute on (and cache under) the
		// repository default branch ref (the PR base branch before
		// 2025-12-08) — a shared lineage cleaned only behind the explicit
		// opt-in. Their sticky disks are scoped to the PR merge ref, so
		// include those scopes; the default-branch refs are added by the
		// flag block below.
		if !includeDefaultBranch {
			return cleanupTarget{}, fmt.Errorf("%s run %d cached under the repository default branch (or the PR base branch for pre-2025-12-08 runs); pass --include-default-branch to clean that shared lineage", run.Event, job.RunID)
		}
		for _, number := range pullNumbers {
			refs.addBranchScope(mergeRef(number))
		}
	case run.Event == "workflow_run":
		// workflow_run also executes on the repository default branch ref
		// regardless of the upstream run's branch: same opt-in. Flex scopes
		// these snapshots by the upstream head branch name, which the runs
		// API does record.
		if !includeDefaultBranch {
			return cleanupTarget{}, fmt.Errorf("%s run %d cached under the repository default branch; pass --include-default-branch to clean that shared lineage", run.Event, job.RunID)
		}
		refs.addBranchScope(run.HeadBranch)
	case isPullRequest:
		if len(pullNumbers) == 0 {
			return cleanupTarget{}, fmt.Errorf("could not determine the pull request behind %s run %d", run.Event, job.RunID)
		}
		// A `closed` run after the merge writes under the merged-into base
		// branch instead of the merge ref; that lineage is usually the
		// default branch, cleanable with --include-default-branch.
		for _, number := range pullNumbers {
			refs.addRef(mergeRef(number))
		}
	case run.HeadBranch != "":
		// The runs API records only the short head ref name: branch- and
		// tag-triggered runs are indistinguishable, so both forms are
		// cleaned. Over-deleting a same-named tag or branch costs one cache
		// rebuild; the plan is confirmed first.
		refs.addRef("refs/heads/" + run.HeadBranch)
		refs.addRef("refs/tags/" + run.HeadBranch)
	}
	if includeDefaultBranch && repo.DefaultBranch != "" {
		refs.addRef("refs/heads/" + repo.DefaultBranch)
	}

	if len(refs.cacheRefs) == 0 {
		return cleanupTarget{}, fmt.Errorf("could not determine any ref to clean for run %d (%s event)", job.RunID, run.Event)
	}

	nestedRefs, err := listNestedRefs(ctx, github, job, refs.cacheRefs)
	if err != nil {
		return cleanupTarget{}, err
	}

	return cleanupTarget{
		RepoSlug:   repoSlug,
		OwnerID:    repo.Owner.ID,
		RepoID:     repo.ID,
		CacheRefs:  refs.cacheRefs,
		Branches:   refs.branches,
		NestedRefs: nestedRefs,
		Warnings:   warnings,
	}, nil
}

// listNestedRefs enumerates live refs nested under each cleaned branch/tag
// ref (branch "release" vs "release/2.x"). Their classic cache keys share
// the shorter ref's raw S3 prefix, so the planner excludes their key spaces
// from its listing.
func listNestedRefs(ctx context.Context, github ghAPIRunner, job parsedGitHubJobURL, cacheRefs []string) (map[string][]string, error) {
	nested := map[string][]string{}
	for _, cacheRef := range cacheRefs {
		kind := ""
		name := ""
		if branch, ok := strings.CutPrefix(cacheRef, "refs/heads/"); ok {
			kind, name = "heads", branch
		} else if tag, ok := strings.CutPrefix(cacheRef, "refs/tags/"); ok {
			kind, name = "tags", tag
		} else {
			continue // merge refs cannot nest
		}
		body, err := github.Get(ctx, job.Host, fmt.Sprintf("repos/%s/%s/git/matching-refs/%s/%s/", job.Owner, job.Repo, kind, escapeRefPath(name)))
		if err != nil {
			if errors.Is(err, errGHNotFound) {
				continue
			}
			return nil, fmt.Errorf("list refs nested under %s: %w", cacheRef, err)
		}
		var matches []struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal(body, &matches); err != nil {
			return nil, fmt.Errorf("decode nested refs for %s: %w", cacheRef, err)
		}
		for _, match := range matches {
			if suffix, ok := strings.CutPrefix(match.Ref, cacheRef+"/"); ok && suffix != "" {
				nested[cacheRef] = append(nested[cacheRef], suffix)
			}
		}
	}
	return nested, nil
}

// escapeRefPath escapes a ref name for use inside a GitHub API URL path,
// keeping "/" as a segment separator: git ref characters like "#" would
// otherwise truncate or reroute the request.
func escapeRefPath(name string) string {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

type cleanupS3API interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

type cleanupEC2API interface {
	DescribeSnapshots(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
	DeleteSnapshot(ctx context.Context, params *ec2.DeleteSnapshotInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)
}

type stackCleaner struct {
	s3          cleanupS3API
	ec2         cleanupEC2API
	cacheBucket string
	stackName   string
	out         io.Writer
}

type prefixObjects struct {
	Prefix string
	// Objects are current keys only. On buckets with versioning enabled the
	// lifecycle rules expire noncurrent versions and stray delete markers
	// within a day, so key-level deletes are enough.
	Objects []s3types.ObjectIdentifier
}

type snapshotInfo struct {
	ID    string
	Scope string
	// Lineage is the runs-on-stickydisk-prefix tag (v1/<name>/<platform>/<arch>).
	Lineage   string
	StartTime string
}

type cleanupPlan struct {
	prefixes  []prefixObjects
	snapshots []snapshotInfo
}

func (p cleanupPlan) objectCount() int {
	count := 0
	for _, prefix := range p.prefixes {
		count += len(prefix.Objects)
	}
	return count
}

func (p cleanupPlan) empty() bool {
	return p.objectCount() == 0 && len(p.snapshots) == 0
}

func (p cleanupPlan) print(out io.Writer, target cleanupTarget) {
	for _, warning := range target.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	fmt.Fprintf(out, "Refs: %s\n", strings.Join(target.CacheRefs, ", "))
	for _, prefix := range p.prefixes {
		fmt.Fprintf(out, "  %4d cache object(s) under %s\n", len(prefix.Objects), prefix.Prefix)
	}
	for _, snapshot := range p.snapshots {
		fmt.Fprintf(out, "  snapshot %s (lineage %s, scope %s, started %s)\n", snapshot.ID, snapshot.Lineage, snapshot.Scope, snapshot.StartTime)
	}
}

// plan lists everything that would be deleted, without deleting.
func (c *stackCleaner) plan(ctx context.Context, target cleanupTarget) (cleanupPlan, error) {
	var plan cleanupPlan

	if c.cacheBucket == "" {
		fmt.Fprintf(c.out, "Warning: stack %q did not expose its cache bucket; skipping S3 cache cleanup (upgrade the stack, or clean the bucket manually).\n", c.stackName)
	} else {
		// Classic v1 keys embed the ref unescaped, so a short ref's raw
		// prefix also matches nested refs (branch "release" vs
		// "release/2.x"): their live suffixes are excluded from the listing.
		// Scoped-cache prefixes end in a fixed-length hash and cannot nest.
		for _, v1 := range target.v1CachePrefixes() {
			nestedSuffixes := target.NestedRefs[v1.Ref]
			if err := c.planPrefix(ctx, &plan, v1.Prefix, func(key string) bool { return belongsToV1Prefix(key, v1.Prefix, nestedSuffixes) }); err != nil {
				return plan, err
			}
		}
		for _, prefix := range target.scopedCachePrefixes() {
			if err := c.planPrefix(ctx, &plan, prefix, nil); err != nil {
				return plan, err
			}
		}
	}

	snapshots, err := c.listSnapshots(ctx, target)
	if err != nil {
		return plan, err
	}
	plan.snapshots = snapshots
	return plan, nil
}

// planPrefix lists a prefix (optionally filtering keys) and records it in
// the plan when non-empty.
func (c *stackCleaner) planPrefix(ctx context.Context, plan *cleanupPlan, prefix string, keep func(string) bool) error {
	objects, err := c.listObjects(ctx, prefix)
	if err != nil {
		return err
	}
	if keep != nil {
		filtered := objects[:0]
		for _, object := range objects {
			if keep(aws.ToString(object.Key)) {
				filtered = append(filtered, object)
			}
		}
		objects = filtered
	}
	if len(objects) > 0 {
		plan.prefixes = append(plan.prefixes, prefixObjects{Prefix: prefix, Objects: objects})
	}
	return nil
}

func (c *stackCleaner) listObjects(ctx context.Context, prefix string) ([]s3types.ObjectIdentifier, error) {
	var objects []s3types.ObjectIdentifier
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(c.cacheBucket),
		Prefix: aws.String(prefix),
	}
	for {
		output, err := c.s3.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", c.cacheBucket, prefix, err)
		}
		for _, object := range output.Contents {
			objects = append(objects, s3types.ObjectIdentifier{Key: object.Key})
		}
		if !aws.ToBool(output.IsTruncated) {
			break
		}
		input.ContinuationToken = output.NextContinuationToken
	}
	return objects, nil
}

// listSnapshots finds this stack's sticky-disk snapshots for the target repo
// and branch scopes (tag scheme: pkg/stickydisk/stickydisk.go).
func (c *stackCleaner) listSnapshots(ctx context.Context, target cleanupTarget) ([]snapshotInfo, error) {
	scopeValues := target.snapshotScopeValues()
	if len(scopeValues) == 0 {
		return nil, nil
	}
	input := &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + tagStackName), Values: []string{c.stackName}},
			{Name: aws.String("tag:" + tagStickyDisk), Values: []string{"true"}},
			{Name: aws.String("tag:" + tagStickyDiskRepo), Values: []string{target.snapshotRepoTagValue()}},
			{Name: aws.String("tag:" + tagStickyDiskScope), Values: scopeValues},
		},
	}

	var snapshots []snapshotInfo
	for {
		output, err := c.ec2.DescribeSnapshots(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("describe sticky-disk snapshots: %w", err)
		}
		for _, snapshot := range output.Snapshots {
			info := snapshotInfo{ID: aws.ToString(snapshot.SnapshotId)}
			if snapshot.StartTime != nil {
				info.StartTime = snapshot.StartTime.UTC().Format("2006-01-02T15:04:05Z")
			}
			for _, tag := range snapshot.Tags {
				switch aws.ToString(tag.Key) {
				case tagStickyDiskScope:
					info.Scope = aws.ToString(tag.Value)
				case "runs-on-stickydisk-prefix":
					info.Lineage = aws.ToString(tag.Value)
				}
			}
			snapshots = append(snapshots, info)
		}
		if aws.ToString(output.NextToken) == "" {
			break
		}
		input.NextToken = output.NextToken
	}
	return snapshots, nil
}

func (c *stackCleaner) execute(ctx context.Context, plan cleanupPlan) error {
	var failures []string

	for _, prefix := range plan.prefixes {
		deleted, err := c.deleteObjects(ctx, prefix.Objects)
		if err != nil {
			failures = append(failures, err.Error())
		}
		fmt.Fprintf(c.out, "Deleted %d cache object(s) under %s\n", deleted, prefix.Prefix)
	}

	for _, snapshot := range plan.snapshots {
		if _, err := c.ec2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(snapshot.ID)}); err != nil {
			// Pending snapshots (a job just completed) cannot be deleted;
			// surface them for a retry rather than aborting the rest.
			failures = append(failures, fmt.Sprintf("delete snapshot %s: %v", snapshot.ID, err))
			continue
		}
		fmt.Fprintf(c.out, "Deleted snapshot %s (lineage %s, scope %s)\n", snapshot.ID, snapshot.Lineage, snapshot.Scope)
	}

	if len(failures) > 0 {
		return fmt.Errorf("cleanup finished with errors:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}

func (c *stackCleaner) deleteObjects(ctx context.Context, objects []s3types.ObjectIdentifier) (int, error) {
	const batchSize = 1000
	deleted := 0
	var failures []string
	for start := 0; start < len(objects); start += batchSize {
		end := min(start+batchSize, len(objects))
		batch := objects[start:end]
		output, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.cacheBucket),
			Delete: &s3types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("delete cache objects: %v", err))
			continue
		}
		deleted += len(batch) - len(output.Errors)
		for _, deleteError := range output.Errors {
			failures = append(failures, fmt.Sprintf("delete s3://%s/%s: %s", c.cacheBucket, aws.ToString(deleteError.Key), aws.ToString(deleteError.Message)))
		}
	}
	if len(failures) > 0 {
		return deleted, errors.New(strings.Join(failures, "; "))
	}
	return deleted, nil
}

func confirm(cmd *cobra.Command, prompt string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
	reader := bufio.NewReader(cmd.InOrStdin())
	answer, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
