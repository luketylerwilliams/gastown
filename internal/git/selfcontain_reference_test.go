package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut runs a git command in dir and returns its output, failing on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitOut(t, dir, args...)
}

// TestCloneBareWithReferenceAndBranchIsSelfContained reproduces the
// hq-wisp-aih "bad object" class: a single-branch clone pinned to a branch
// and made with --reference to a local repo recorded alternates pointing at
// the reference. When the reference path later disappears, every object
// access that resolved through the alternates file fails with
// "fatal: bad object". The clone must instead be self-contained: objects
// reachable from its refs grafted in, alternates dropped, and still readable
// after the reference is deleted (and after the branch is force-reset to an
// older commit that only the reference carried).
func TestCloneBareWithReferenceAndBranchIsSelfContained(t *testing.T) {
	root := t.TempDir()

	up := filepath.Join(root, "up.git")
	mustGit(t, root, "init", "--bare", up)
	upURL := "file://" + up
	seed := filepath.Join(root, "seed")
	mustGit(t, root, "clone", upURL, seed)
	mustGit(t, seed, "config", "user.email", "t@t")
	mustGit(t, seed, "config", "user.name", "t")
	mustGit(t, seed, "checkout", "-B", "main")
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one"), 0o644)
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-m", "first")
	midHead := strings.TrimSpace(gitOut(t, seed, "rev-parse", "HEAD"))
	mustGit(t, seed, "push", "origin", "main")
	os.WriteFile(filepath.Join(seed, "b.txt"), []byte("two"), 0o644)
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-m", "second")
	mustGit(t, seed, "push", "origin", "main")

	// ref: transient local reference (like a polecat worktree or stale bare
	// repo) holding the branch plus an older commit the remote no longer
	// exposes at the branch head (force-push class).
	ref := filepath.Join(root, "ref.git")
	mustGit(t, root, "init", "--bare", ref)
	mustGit(t, ref, "fetch", upURL, "+refs/heads/main:refs/heads/main")
	mustGit(t, ref, "update-ref", "refs/heads/old", midHead)

	g := NewGitWithDir(up, "")
	dest := filepath.Join(root, "dest.git")
	if err := g.CloneBareWithReferenceAndBranch(upURL, dest, ref, "main"); err != nil {
		t.Fatalf("CloneBareWithReferenceAndBranch: %v", err)
	}

	altFile := filepath.Join(dest, "objects", "info", "alternates")
	if data, err := os.ReadFile(altFile); err == nil {
		t.Fatalf("alternates file still present after clone: %s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("reading alternates: %v", err)
	}

	// Delete the reference and force-reset main to the commit that only the
	// reference carried (this is what produced "fatal: bad object" in the
	// field). Object access must still work.
	if err := os.RemoveAll(ref); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dest, "update-ref", "refs/heads/main", midHead)
	gitOut(t, dest, "rev-list", "--count", "main")
	gitOut(t, dest, "cat-file", "-p", midHead)
}

// TestCloneWithReferenceWithoutBranchUnchanged guards that clones without a
// pinned branch keep the existing shared-objects behavior (no graft,
// alternates preserved).
func TestCloneWithReferenceWithoutBranchUnchanged(t *testing.T) {
	root := t.TempDir()
	up := filepath.Join(root, "up.git")
	mustGit(t, root, "init", "--bare", up)
	seed := filepath.Join(root, "seed")
	mustGit(t, root, "clone", up, seed)
	mustGit(t, seed, "config", "user.email", "t@t")
	mustGit(t, seed, "config", "user.name", "t")
	mustGit(t, seed, "checkout", "-B", "main")
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one"), 0o644)
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-m", "first")
	mustGit(t, seed, "push", "origin", "main")

	ref := filepath.Join(root, "ref.git")
	mustGit(t, root, "init", "--bare", ref)
	mustGit(t, ref, "fetch", up, "+refs/heads/*:refs/heads/*")

	g := NewGitWithDir(up, "")
	dest := filepath.Join(root, "dest2.git")
	if err := g.CloneBareWithReference(up, dest, ref); err != nil {
		t.Fatalf("CloneBareWithReference: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "objects", "info", "alternates")); err != nil {
		t.Fatalf("expected alternates to remain for non-branch reference clone: %v", err)
	}
}
