package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// This file mirrors the cache/snapshot scope encodings from the monorepo
// (pkg/stickydisk/stickydisk.go scopeTagValue, pkg/ec2meta/tag.go
// SanitizeTagValue, pkg/agent/cache/utils.go scopeSegment). The CLI module is
// standalone (mirrored downstream), so the encodings are duplicated here and
// MUST stay byte-identical with their monorepo sources.

// Sticky-disk snapshot tag keys (pkg/stickydisk/stickydisk.go).
const (
	tagStickyDisk      = "runs-on-stickydisk"
	tagStickyDiskRepo  = "runs-on-stickydisk-repo"
	tagStickyDiskScope = "runs-on-stickydisk-scope"
	tagStackName       = "runs-on-stack-name"
)

var tagValueSanitizer = regexp.MustCompile(`[^a-zA-Z0-9+=.,_:/@\s-]+`)

// sanitizeTagValue mirrors ec2meta.SanitizeTagValue.
func sanitizeTagValue(value string) string {
	sanitized := tagValueSanitizer.ReplaceAllString(value, "")
	sanitized = strings.TrimSpace(sanitized)
	if len(sanitized) > 256 {
		sanitized = sanitized[:256]
	}
	return sanitized
}

// scopeTagValue mirrors stickydisk.scopeTagValue: branch names are stored as
// EC2 tag values, with a sha256 suffix appended whenever sanitization was
// lossy so distinct branches can never collide on the same tag value.
func scopeTagValue(branch string) string {
	sanitized := sanitizeTagValue(branch)
	if sanitized == branch {
		return sanitized
	}
	sum := sha256.Sum256([]byte(branch))
	suffix := "-" + hex.EncodeToString(sum[:])[:12]
	if len(sanitized)+len(suffix) > 256 {
		sanitized = sanitized[:256-len(suffix)]
	}
	return sanitized + suffix
}

// cacheScopeSegment mirrors the scoped-cache path segment used by the agent
// and the cache credential broker Lambda: first 16 hex chars of sha256(scope).
func cacheScopeSegment(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])[:16]
}

// cleanupTarget describes everything the cleanup command derives from a job:
// which refs (GitHub cache scopes) and branch names to clean, for which repo.
type cleanupTarget struct {
	// RepoSlug is the canonical "owner/repo" spelling whose keys/tags to
	// clean (cache keys and snapshot tags are written with it).
	RepoSlug string
	// OwnerID is the numeric owner ID embedded in scoped-cache keys.
	OwnerID   int64
	RepoID    int64
	CacheRefs []string // GitHub cache scopes, e.g. refs/heads/main, refs/pull/5/merge
	Branches  []string // sticky-disk snapshot scopes, e.g. main, refs/pull/5/merge
	// NestedRefs maps a cache ref to the live ref suffixes nested under it
	// (branch "release" → ["2.x"]); their keys share the ref's raw S3 prefix
	// and are excluded from its cleanup.
	NestedRefs map[string][]string
	// Warnings are surfaced to the user before the plan.
	Warnings []string
}

// v1CachePrefix pairs a classic cache object prefix with the ref it belongs
// to, so listings can exclude nested refs sharing the raw prefix.
type v1CachePrefix struct {
	Prefix string
	Ref    string
}

// v1CachePrefixes returns the classic cache object prefixes
// (cache/v1/<org>/<repo>/<ref>/, see pkg/agent/cache/utils.go).
func (t cleanupTarget) v1CachePrefixes() []v1CachePrefix {
	prefixes := make([]v1CachePrefix, 0, len(t.CacheRefs))
	for _, ref := range t.CacheRefs {
		prefixes = append(prefixes, v1CachePrefix{Prefix: fmt.Sprintf("cache/v1/%s/%s/", t.RepoSlug, ref), Ref: ref})
	}
	return prefixes
}

// snapshotRepoTagValue returns the runs-on-stickydisk-repo tag value to match.
func (t cleanupTarget) snapshotRepoTagValue() string {
	return sanitizeTagValue(t.RepoSlug)
}

// scopedCachePrefixes returns the isolated-cache object prefixes
// (scoped-cache/<ownerID>/<repoID>/<sha256(ref)[:16]>/, matching the cache
// credential broker Lambda's prefixesFor). Missing numeric IDs (e.g. gh
// output drift) skip scoped prefixes rather than constructing a wrong path.
func (t cleanupTarget) scopedCachePrefixes() []string {
	if t.RepoID == 0 || t.OwnerID == 0 {
		return nil
	}
	prefixes := make([]string, 0, len(t.CacheRefs))
	for _, ref := range t.CacheRefs {
		prefixes = append(prefixes, fmt.Sprintf("scoped-cache/%d/%d/%s/", t.OwnerID, t.RepoID, cacheScopeSegment(ref)))
	}
	return prefixes
}

// belongsToV1Prefix reports whether an object key under a v1 cache prefix
// belongs to that exact ref. Cache clients may use arbitrary version strings,
// so the only safe discriminator is the set of LIVE nested refs enumerated
// from GitHub; their shared raw-prefix key spaces are excluded explicitly.
func belongsToV1Prefix(key, prefix string, nestedSuffixes []string) bool {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || rest == "" || !strings.Contains(rest, "/") {
		return false
	}
	for _, suffix := range nestedSuffixes {
		if strings.HasPrefix(rest, suffix+"/") {
			return false
		}
	}
	return true
}

// snapshotScopeValues returns the runs-on-stickydisk-scope tag values to
// match: Flex scopes snapshots by plain branch name, Fleet keeps pull
// merge refs verbatim (pkg/stickydisk/scope.go).
func (t cleanupTarget) snapshotScopeValues() []string {
	values := make([]string, 0, len(t.Branches))
	for _, branch := range t.Branches {
		values = append(values, scopeTagValue(branch))
	}
	return values
}

// snapshotScopeForRef maps a full git ref to the Fleet sticky-disk snapshot
// scope: branches are scoped by short name, everything else (tags, merge
// refs) keeps the full ref (pkg/stickydisk/scope.go BranchFromWorkflowRef).
// Flex scopes differ for non-branch runs — it uses the webhook's HeadBranch
// unchanged for non-PR runs and refs/pull/N/merge for every pull_request*
// event (pkg/flex/provisioning/launch.go stickyDiskBranchScope) — so callers
// add both products' scopes.
func snapshotScopeForRef(ref string) string {
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return branch
	}
	return ref
}

// runRefs accumulates the cache refs and snapshot scopes to clean, with
// order-preserving deduplication.
type runRefs struct {
	cacheRefs []string
	branches  []string
}

func appendUnique(list []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return list
	}
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

// addCacheRef records a GitHub cache scope (a full ref).
func (r *runRefs) addCacheRef(ref string) {
	r.cacheRefs = appendUnique(r.cacheRefs, ref)
}

// addBranchScope records a sticky-disk snapshot scope value.
func (r *runRefs) addBranchScope(scope string) {
	r.branches = appendUnique(r.branches, scope)
}

// addRef records a full ref as both a cache scope and its Fleet snapshot scope.
func (r *runRefs) addRef(ref string) {
	r.addCacheRef(ref)
	r.addBranchScope(snapshotScopeForRef(ref))
}

func mergeRef(number int64) string {
	return fmt.Sprintf("refs/pull/%d/merge", number)
}
