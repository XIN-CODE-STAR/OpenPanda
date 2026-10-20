// SPDX-License-Identifier: AGPL-3.0-or-later

package queue

import "strings"

// Scope keys give a shared project worktree sub-path granularity: instead of
// every project task serializing on one monolithic "project:<name>" key, a
// task that declares the paths it will touch locks "scope:<name>/<path>"
// leaves — disjoint scopes run in parallel, overlapping ones serialize, and
// the root scope "scope:<name>" (held by any task that did NOT narrow its
// claim) still excludes every descendant, exactly like a database intent
// lock. The model mirrors the per-sender-slot idea: coordination at the
// granularity of what is actually contended, never a global broadcast.
//
// Conflict between two scope keys is prefix-overlap in EITHER direction:
// "scope:p/a" blocks "scope:p", "scope:p/a/b" and itself, but not
// "scope:p/ab" (sibling, different file) nor "scope:q/a" (different
// project). Only keys under the "scope:" namespace take this rule; every
// other key type keeps exact-match semantics.
const scopeKeyPrefix = "scope:"

// lockFold folds a lock-key component to its case-insensitive identity.
// Scope leaves and pinned work dirs name filesystem paths, and the
// filesystems most users run on (APFS, NTFS) treat "Docs" and "docs" as one
// directory — a byte-precise key would let two writers of the same tree
// hold disjoint locks. Folding errs toward serialization: on a
// case-sensitive volume the odd colliding pair queues where it could have
// run in parallel, a safe direction. (Unicode normalization differs by
// volume and lies outside stdlib-only key math.)
func lockFold(s string) string { return strings.ToLower(s) }

// IsScopeKey reports whether key lives in the scope namespace.
func IsScopeKey(key string) bool {
	return strings.HasPrefix(key, scopeKeyPrefix)
}

// ScopeRootKey returns the whole-project scope key — the claim every task
// holds that did not narrow itself to declared paths. A scoped child never
// carries it (that would re-serialize siblings); an unscoped project task
// always does, so whole-project work still excludes scoped children and
// vice versa.
func ScopeRootKey(project string) string {
	return scopeKeyPrefix + lockFold(project)
}

// ScopeLeafKey returns the scope key for one declared project-relative path.
// An empty path names the root itself.
func ScopeLeafKey(project, path string) string {
	if path == "" {
		return ScopeRootKey(project)
	}
	return scopeKeyPrefix + lockFold(project) + "/" + lockFold(path)
}

// ScopeKeysConflict reports whether two scope keys contend for the same
// subtree: equal, or one a strict ancestor of the other ("/"-bounded, so
// "…/a" never bleeds into "…/ab").
func ScopeKeysConflict(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ScopeAncestors lists a scope key's strict ancestor keys — for
// "scope:p/a/b": [scope:p, scope:p/a]. The acquiring side checks these for
// already-held rows; its own descendants are covered by a LIKE pattern.
func ScopeAncestors(key string) []string {
	if !IsScopeKey(key) {
		return nil
	}
	var out []string
	for i := len(scopeKeyPrefix); ; {
		j := strings.IndexByte(key[i:], '/')
		if j < 0 {
			return out
		}
		out = append(out, key[:i+j])
		i += j + 1
	}
}
