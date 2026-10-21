// SPDX-License-Identifier: AGPL-3.0-or-later

package self

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Xustalis/OpenPanda/internal/updater"
)

// versionStampRe bounds what may travel into -ldflags from a manifest. A
// version is a display string, never a flag list.
var versionStampRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)

// StageTree builds a staged-release-shaped tree from the worktree at wt:
// {dst}/bin/panda plus the adapters/ and extensions/voice/ resource dirs the
// apply step syncs. The binary is stamped with the patch set's version and
// Provenance=self so `panda version`, the wire provenance, and the trust layer
// all see the build for what it is — a locally produced artifact, never a
// signed release — and which self build is running survives the swap.
func (m *Manager) StageTree(ctx context.Context, wt, dst, version string) error {
	version = strings.TrimSpace(version)
	if version != "" && !versionStampRe.MatchString(version) {
		return fmt.Errorf("version stamp %q is not a plain version string", version)
	}
	binDir := filepath.Join(dst, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(binDir, "panda")
	if strings.HasSuffix(os.Args[0], ".exe") {
		out += ".exe"
	}
	ldflags := "-X github.com/Xustalis/OpenPanda/internal/version.Provenance=self"
	if version != "" {
		ldflags += " -X github.com/Xustalis/OpenPanda/internal/version.Version=" + version
	}
	args := []string{"build", "-trimpath", "-ldflags", ldflags}
	// A lite node must stay lite: footprint table and the updater's asset-name
	// selection both key on the build tag (design §5.2).
	if updater.LiteBuild() {
		args = append(args, "-tags", "lite")
	}
	args = append(args, "-o", out, "./cmd/panda")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = wt
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %s", combined)
	}
	for _, dir := range []string{"adapters", "extensions/voice"} {
		src := filepath.Join(wt, filepath.FromSlash(dir))
		if _, err := os.Stat(src); err != nil {
			continue // a tree legitimately lacking a resource dir builds fine
		}
		if err := copyDir(src, filepath.Join(dst, filepath.FromSlash(dir))); err != nil {
			return fmt.Errorf("stage %s: %w", dir, err)
		}
	}
	return nil
}

// NextVersion derives the target stamp for a fresh patch set: the base
// release version plus a +self.<n> build counter, where n counts this node's
// already-merged sets + 1. CompareVersion ignores +meta, so the stamp sorts
// on its base release and an official update still surfaces as available.
func (m *Manager) NextVersion(base string) string {
	n := 1
	if sets, err := m.Queue(); err == nil {
		for _, s := range sets {
			if s.State == StateMerged {
				n++
			}
		}
	}
	// A base that already carries +self.N keeps counting from it.
	if i := strings.Index(base, "+self."); i >= 0 {
		var seen int
		if _, err := fmt.Sscanf(base[i:], "+self.%d", &seen); err == nil && seen >= n {
			n = seen + 1
		}
		base = base[:i]
	}
	return fmt.Sprintf("%s+self.%d", base, n)
}
