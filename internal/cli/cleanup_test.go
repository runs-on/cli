package cli

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The scope encodings mirror pkg/stickydisk and the cache broker Lambda; the
// vectors below match pkg/stickydisk/stickydisk_test.go so a drift between
// the two implementations fails a test on at least one side.
func TestScopeTagValueMirrorsStickyDiskEncoding(t *testing.T) {
	if got := scopeTagValue("main"); got != "main" {
		t.Errorf("expected clean branch to be unchanged, got %q", got)
	}
	if got := scopeTagValue("refs/pull/151/merge"); got != "refs/pull/151/merge" {
		t.Errorf("expected merge ref to be unchanged, got %q", got)
	}
	// Lossy sanitization appends -<sha256(raw)[:12]>.
	sum := sha256.Sum256([]byte("ma%in"))
	want := "main-" + hex.EncodeToString(sum[:])[:12]
	if got := scopeTagValue("ma%in"); got != want {
		t.Errorf("scopeTagValue(ma%%in) = %q, want %q", got, want)
	}
	if long := scopeTagValue(strings.Repeat("é", 400) + "/branch"); len(long) > 256 {
		t.Errorf("scope tag value exceeds 256 chars: %d", len(long))
	}
}

func TestCacheScopeSegmentMirrorsBrokerEncoding(t *testing.T) {
	// First 16 hex chars of sha256(scope), matching the agent's scopeSegment
	// and the broker Lambda.
	sum := sha256.Sum256([]byte("refs/heads/main"))
	want := hex.EncodeToString(sum[:])[:16]
	if got := cacheScopeSegment("refs/heads/main"); got != want {
		t.Errorf("cacheScopeSegment = %q, want %q", got, want)
	}
}

func TestValidateCleanupJobRequiresSelectedStackRecord(t *testing.T) {
	config := &RunsOnConfig{StackName: "runs-on-preview", Product: "flex"}
	job := parsedGitHubJobURL{RunID: 77, JobID: 88}

	if err := validateCleanupJob(&jobDiagnosticsResponse{}, config, job); err == nil || !strings.Contains(err.Error(), "not found in the selected RunsOn stack") {
		t.Fatalf("expected missing stack record error, got %v", err)
	}
	response := &jobDiagnosticsResponse{
		Product:   "flex",
		StackName: "runs-on-preview",
		Local:     &jobDiagnosticsLocal{WorkflowRunID: 77, WorkflowJobID: 88},
	}
	if err := validateCleanupJob(response, config, job); err != nil {
		t.Fatalf("expected matching selected-stack record, got %v", err)
	}
	response.StackName = "runs-on-other"
	if err := validateCleanupJob(response, config, job); err == nil || !strings.Contains(err.Error(), "resolved stack") {
		t.Fatalf("expected stack mismatch error, got %v", err)
	}
}

func TestSnapshotScopeForRef(t *testing.T) {
	// Fleet scopes branches by short name, everything else by full ref.
	for ref, want := range map[string]string{
		"refs/heads/main":      "main",
		"refs/heads/feature/x": "feature/x",
		"refs/tags/v1.2.3":     "refs/tags/v1.2.3",
		"refs/pull/42/merge":   "refs/pull/42/merge",
	} {
		if got := snapshotScopeForRef(ref); got != want {
			t.Errorf("snapshotScopeForRef(%s) = %s, want %s", ref, got, want)
		}
	}
}

func TestCleanupTargetPrefixes(t *testing.T) {
	target := cleanupTarget{
		RepoSlug: "acme/widgets", OwnerID: 101, RepoID: 202,
		CacheRefs: []string{"refs/heads/main"},
	}
	if got := target.v1CachePrefixes(); len(got) != 1 || got[0].Prefix != "cache/v1/acme/widgets/refs/heads/main/" || got[0].Ref != "refs/heads/main" {
		t.Errorf("v1CachePrefixes = %v", got)
	}
	segment := cacheScopeSegment("refs/heads/main")
	// Tokens without repository_owner_id (GHE.com) write under owner 0, so
	// both layouts are cleaned.
	want := []string{
		fmt.Sprintf("scoped-cache/101/202/%s/", segment),
		fmt.Sprintf("scoped-cache/0/202/%s/", segment),
	}
	if got := target.scopedCachePrefixes(); !slices.Equal(got, want) {
		t.Errorf("scopedCachePrefixes = %v, want %v", got, want)
	}
	target.OwnerID = 0
	if got := target.scopedCachePrefixes(); !slices.Equal(got, want[1:]) {
		t.Errorf("scopedCachePrefixes without owner ID = %v, want %v", got, want[1:])
	}
	// A missing repository ID (e.g. gh output drift) must skip scoped
	// prefixes rather than constructing a wrong path.
	target.RepoID = 0
	if got := target.scopedCachePrefixes(); got != nil {
		t.Errorf("expected no scoped prefixes without a repository ID, got %v", got)
	}
}

type fakeGH struct {
	responses map[string]string
}

func (f fakeGH) Get(_ context.Context, _, path string) ([]byte, error) {
	body, ok := f.responses[path]
	if !ok {
		return nil, fmt.Errorf("no response for %s: %w", path, errGHNotFound)
	}
	return []byte(body), nil
}

func TestResolveCleanupTarget(t *testing.T) {
	t.Parallel()

	const defaultRepo = `{"id": 202, "owner": {"id": 101}, "default_branch": "main"}`
	tests := []struct {
		name           string
		repo           string // repository response; defaultRepo when empty
		run            string // workflow run 77 response
		extra          map[string]string
		includeDefault bool
		wantErr        string
		wantCacheRefs  string
		wantBranches   string
		wantSlug       string // "acme/widgets" when empty
		wantWarning    string // substring of the single warning; none when empty
		wantNested     map[string]string
	}{
		{
			name:           "pull request plus default branch opt-in",
			run:            `{"head_branch": "feature/x", "head_sha": "abc", "event": "pull_request", "pull_requests": [{"number": 42}]}`,
			includeDefault: true,
			wantCacheRefs:  "refs/pull/42/merge,refs/heads/main",
			wantBranches:   "refs/pull/42/merge,main",
		},
		{
			// Each associated PR's merge scope is re-creatable per-PR cache data.
			name:          "run associated with several pull requests",
			run:           `{"head_branch": "feature/x", "head_sha": "abc", "event": "pull_request", "pull_requests": [{"number": 7}, {"number": 8}]}`,
			wantCacheRefs: "refs/pull/7/merge,refs/pull/8/merge",
			wantBranches:  "refs/pull/7/merge,refs/pull/8/merge",
		},
		{
			// Fork PR runs report an empty pull_requests list: the PR numbers
			// are recovered from the head commit, and the fork's head branch
			// name must not leak into the refs.
			name:          "fork pull request recovers PRs from head commit",
			run:           `{"head_branch": "main", "head_sha": "abc123", "event": "pull_request", "pull_requests": []}`,
			extra:         map[string]string{"repos/acme/widgets/commits/abc123/pulls": `[{"number": 7}, {"number": 8}]`},
			wantCacheRefs: "refs/pull/7/merge,refs/pull/8/merge",
			wantBranches:  "refs/pull/7/merge,refs/pull/8/merge",
		},
		{
			name:           "pull request without a resolvable PR",
			run:            `{"head_branch": "feature/x", "head_sha": "abc", "event": "pull_request", "pull_requests": []}`,
			extra:          map[string]string{"repos/acme/widgets/commits/abc/pulls": `[]`},
			includeDefault: true,
			wantErr:        "could not determine the pull request",
		},
		{
			// pull_request_target caches under the default branch (the PR base
			// branch before 2025-12-08), a shared lineage behind the opt-in.
			name:    "pull_request_target refused without opt-in",
			run:     `{"head_branch": "main", "head_sha": "abc", "event": "pull_request_target", "pull_requests": [{"number": 42}]}`,
			wantErr: "--include-default-branch",
		},
		{
			// With the opt-in, the run's Flex merge-ref sticky-disk scopes too.
			name:           "pull_request_target with opt-in",
			run:            `{"head_branch": "main", "head_sha": "abc", "event": "pull_request_target", "pull_requests": [{"number": 42}]}`,
			includeDefault: true,
			wantCacheRefs:  "refs/heads/main",
			wantBranches:   "refs/pull/42/merge,main",
		},
		{
			// workflow_run executes on the default branch regardless of the
			// upstream branch.
			name:    "workflow_run refused without opt-in",
			run:     `{"head_branch": "feature/x", "head_sha": "abc", "event": "workflow_run", "pull_requests": []}`,
			wantErr: "--include-default-branch",
		},
		{
			// With the opt-in, the upstream head branch Flex uses as scope too.
			name:           "workflow_run with opt-in",
			run:            `{"head_branch": "feature/x", "head_sha": "abc", "event": "workflow_run", "pull_requests": []}`,
			includeDefault: true,
			wantCacheRefs:  "refs/heads/main",
			wantBranches:   "feature/x,main",
		},
		{
			// The runs API records only the short name, so non-PR runs clean
			// it as both a branch and a tag (Flex: raw short name; Fleet:
			// branch short name / full tag ref). A case-only slug difference
			// uses the canonical spelling silently.
			name:          "push cleans branch and tag under canonical slug",
			repo:          `{"id": 202, "name": "Widgets", "owner": {"id": 101, "login": "Acme"}, "default_branch": "main"}`,
			run:           `{"head_branch": "feature/x", "head_sha": "abc", "event": "push", "pull_requests": []}`,
			wantCacheRefs: "refs/heads/feature/x,refs/tags/feature/x",
			wantBranches:  "feature/x,refs/tags/feature/x",
			wantSlug:      "Acme/Widgets",
		},
		{
			// Historical data under the old slug is left to expire; the user
			// is told so.
			name:          "renamed repository warns",
			repo:          `{"id": 202, "name": "gadgets", "owner": {"id": 101, "login": "acme"}, "default_branch": "main"}`,
			run:           `{"head_branch": "feature/x", "head_sha": "abc", "event": "push", "pull_requests": []}`,
			wantCacheRefs: "refs/heads/feature/x,refs/tags/feature/x",
			wantBranches:  "feature/x,refs/tags/feature/x",
			wantSlug:      "acme/gadgets",
			wantWarning:   "renamed",
		},
		{
			// A "#" in the ref is escaped before the nested-ref probe; only
			// the escaped path answers, so an unescaped lookup loses the
			// nested ref.
			name: "ref with URL-reserved characters is escaped",
			run:  `{"head_branch": "feature#123", "head_sha": "abc", "event": "push", "pull_requests": []}`,
			extra: map[string]string{
				"repos/acme/widgets/git/matching-refs/heads/feature%23123/": `[{"ref": "refs/heads/feature#123/sub"}]`,
				"repos/acme/widgets/git/matching-refs/tags/feature%23123/":  `[]`,
			},
			wantCacheRefs: "refs/heads/feature#123,refs/tags/feature#123",
			wantBranches:  "feature#123,refs/tags/feature#123",
			wantNested:    map[string]string{"refs/heads/feature#123": "sub"},
		},
		{
			// Live refs nested under a cleaned ref are enumerated so the
			// planner can exclude their key spaces from the shared raw prefix.
			name:          "nested refs are enumerated",
			run:           `{"head_branch": "release", "head_sha": "abc", "event": "push", "pull_requests": []}`,
			extra:         map[string]string{"repos/acme/widgets/git/matching-refs/heads/release/": `[{"ref": "refs/heads/release/2.x"}, {"ref": "refs/heads/release/3.x"}]`},
			wantCacheRefs: "refs/heads/release,refs/tags/release",
			wantBranches:  "release,refs/tags/release",
			wantNested:    map[string]string{"refs/heads/release": "2.x,3.x"},
		},
	}

	job := parsedGitHubJobURL{Host: "github.com", Owner: "acme", Repo: "widgets", RunID: 77, JobID: 88}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			responses := map[string]string{
				"repos/acme/widgets":                 cmp.Or(tt.repo, defaultRepo),
				"repos/acme/widgets/actions/runs/77": tt.run,
			}
			maps.Copy(responses, tt.extra)

			target, err := resolveCleanupTarget(context.Background(), fakeGH{responses: responses}, job, tt.includeDefault)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if target.OwnerID != 101 || target.RepoID != 202 {
				t.Errorf("numeric IDs = %d/%d, want 101/202", target.OwnerID, target.RepoID)
			}
			if got := strings.Join(target.CacheRefs, ","); got != tt.wantCacheRefs {
				t.Errorf("CacheRefs = %s, want %s", got, tt.wantCacheRefs)
			}
			if got := strings.Join(target.Branches, ","); got != tt.wantBranches {
				t.Errorf("Branches = %s, want %s", got, tt.wantBranches)
			}
			if want := cmp.Or(tt.wantSlug, "acme/widgets"); target.RepoSlug != want {
				t.Errorf("RepoSlug = %s, want %s", target.RepoSlug, want)
			}
			if tt.wantWarning == "" && len(target.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", target.Warnings)
			}
			if tt.wantWarning != "" && (len(target.Warnings) != 1 || !strings.Contains(target.Warnings[0], tt.wantWarning)) {
				t.Errorf("Warnings = %v, want one containing %q", target.Warnings, tt.wantWarning)
			}
			gotNested := map[string]string{}
			for ref, suffixes := range target.NestedRefs {
				gotNested[ref] = strings.Join(suffixes, ",")
			}
			if !maps.Equal(gotNested, tt.wantNested) {
				t.Errorf("NestedRefs = %v, want %v", target.NestedRefs, tt.wantNested)
			}
		})
	}
}

func TestBelongsToV1Prefix(t *testing.T) {
	version := strings.Repeat("ab", 32)
	prefix := "cache/v1/acme/widgets/refs/heads/release/"
	if !belongsToV1Prefix(prefix+version+"/key", prefix, nil) {
		t.Error("expected direct child with version segment to belong")
	}
	// Keys of the NESTED ref refs/heads/release/2.x share the raw prefix but
	// must not be deleted with the shorter ref.
	if belongsToV1Prefix(prefix+"2.x/"+version+"/key", prefix, []string{"2.x"}) {
		t.Error("expected nested ref key to be excluded")
	}
	if !belongsToV1Prefix(prefix+"v1/key", prefix, nil) {
		t.Error("expected arbitrary cache version to belong")
	}
	// A live nested ref named like a version hash passes the shape check but
	// is excluded via the enumerated nested-ref suffixes.
	hexRef := strings.Repeat("cd", 32)
	if belongsToV1Prefix(prefix+hexRef+"/"+version+"/key", prefix, []string{hexRef}) {
		t.Error("expected hex-named nested ref key to be excluded")
	}
	if !belongsToV1Prefix(prefix+version+"/key", prefix, []string{hexRef}) {
		t.Error("expected own key to survive nested-ref exclusion")
	}
}

type fakeCleanupS3 struct {
	objects map[string][]string // prefix -> keys
	deleted []string
}

func (f *fakeCleanupS3) ListObjectsV2(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	var contents []s3types.Object
	for _, key := range f.objects[aws.ToString(params.Prefix)] {
		contents = append(contents, s3types.Object{Key: aws.String(key)})
	}
	return &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(false)}, nil
}

func (f *fakeCleanupS3) DeleteObjects(_ context.Context, params *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	for _, object := range params.Delete.Objects {
		f.deleted = append(f.deleted, aws.ToString(object.Key))
	}
	return &s3.DeleteObjectsOutput{}, nil
}

type fakeCleanupEC2 struct {
	snapshots    []ec2types.Snapshot
	deleted      []string
	scopeFilters []string
}

func (f *fakeCleanupEC2) DescribeSnapshots(_ context.Context, params *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	for _, filter := range params.Filters {
		if aws.ToString(filter.Name) == "tag:"+tagStickyDiskScope {
			f.scopeFilters = filter.Values
		}
	}
	return &ec2.DescribeSnapshotsOutput{Snapshots: f.snapshots}, nil
}

func (f *fakeCleanupEC2) DeleteSnapshot(_ context.Context, params *ec2.DeleteSnapshotInput, _ ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(params.SnapshotId))
	return &ec2.DeleteSnapshotOutput{}, nil
}

func TestStackCleanerPlanAndExecute(t *testing.T) {
	target := cleanupTarget{
		RepoSlug: "acme/widgets", OwnerID: 101, RepoID: 202,
		CacheRefs: []string{"refs/heads/main"},
		Branches:  []string{"main"},
		NestedRefs: map[string][]string{
			"refs/heads/main": {"nested"},
		},
	}
	v1Prefix := "cache/v1/acme/widgets/refs/heads/main/"
	scopedPrefix := fmt.Sprintf("scoped-cache/101/202/%s/", cacheScopeSegment("refs/heads/main"))
	startTime := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	// A live nested ref sharing the raw prefix (refs/heads/main/nested) must
	// not be swept up by the shorter ref's cleanup.
	version := strings.Repeat("ab", 32)
	fakeS3 := &fakeCleanupS3{objects: map[string][]string{
		v1Prefix:     {v1Prefix + version + "/key1", v1Prefix + version + "/key2", v1Prefix + "nested/" + version + "/other-refs-key"},
		scopedPrefix: {scopedPrefix + "v/key3"},
	}}
	fakeEC2 := &fakeCleanupEC2{snapshots: []ec2types.Snapshot{{
		SnapshotId: aws.String("snap-123"),
		StartTime:  &startTime,
		Tags: []ec2types.Tag{
			{Key: aws.String(tagStickyDiskScope), Value: aws.String("main")},
			{Key: aws.String("runs-on-stickydisk-prefix"), Value: aws.String("v1/default/linux/x64")},
		},
	}}}

	var out bytes.Buffer
	cleaner := &stackCleaner{s3: fakeS3, ec2: fakeEC2, cacheBucket: "bucket", stackName: "runs-on", out: &out}

	plan, err := cleaner.plan(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if plan.objectCount() != 3 || len(plan.snapshots) != 1 {
		t.Fatalf("plan = %d objects, %d snapshots", plan.objectCount(), len(plan.snapshots))
	}
	if len(fakeEC2.scopeFilters) != 1 || fakeEC2.scopeFilters[0] != "main" {
		t.Errorf("snapshot scope filters = %v", fakeEC2.scopeFilters)
	}

	if err := cleaner.execute(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(fakeS3.deleted) != 3 || strings.Join(fakeS3.deleted, ",") != v1Prefix+version+"/key1,"+v1Prefix+version+"/key2,"+scopedPrefix+"v/key3" {
		t.Errorf("deleted objects = %v", fakeS3.deleted)
	}
	if len(fakeEC2.deleted) != 1 || fakeEC2.deleted[0] != "snap-123" {
		t.Errorf("deleted snapshots = %v", fakeEC2.deleted)
	}
}

// A stack config without a cache bucket must skip S3 (with a warning) but
// still clean snapshots.
func TestStackCleanerWithoutCacheBucket(t *testing.T) {
	var out bytes.Buffer
	cleaner := &stackCleaner{s3: &fakeCleanupS3{}, ec2: &fakeCleanupEC2{}, cacheBucket: "", stackName: "runs-on", out: &out}
	plan, err := cleaner.plan(context.Background(), cleanupTarget{RepoSlug: "a/b", CacheRefs: []string{"refs/heads/main"}, Branches: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.prefixes) != 0 {
		t.Errorf("expected no prefixes without a bucket, got %v", plan.prefixes)
	}
	if !strings.Contains(out.String(), "skipping S3 cache cleanup") {
		t.Errorf("expected a warning, got %q", out.String())
	}
}
