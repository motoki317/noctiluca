package memory_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/motoki317/noctiluca/pkg/usecase/consolidate"
	"github.com/stretchr/testify/require"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}
func setup(t *testing.T) (domain.MemoryStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
	s, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	return s, dir
}

func TestSeedDefaultProcedures(t *testing.T) {
	dir := t.TempDir()
	_, err := memory.New(t.Context(), memory.Config{Path: dir, Defaults: consolidate.Procedures})
	require.NoError(t, err)
	for _, name := range []string{"procedures/consolidate.md", "procedures/hillclimb.md"} {
		want, err := consolidate.Procedures.ReadFile(name)
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, string(want), git(t, dir, "show", "HEAD:"+name))
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
	require.Empty(t, git(t, dir, "status", "--porcelain"))
	head := git(t, dir, "rev-parse", "HEAD")
	_, err = memory.New(t.Context(), memory.Config{Path: dir, Defaults: consolidate.Procedures})
	require.NoError(t, err)
	require.Equal(t, head, git(t, dir, "rev-parse", "HEAD"))

	require.NoError(t, os.Remove(filepath.Join(dir, "procedures/hillclimb.md")))
	_, err = memory.New(t.Context(), memory.Config{Path: dir, Defaults: consolidate.Procedures})
	require.NoError(t, err)
	require.Empty(t, git(t, dir, "status", "--porcelain"))
}

func TestSeedingPreservesExistingFilesAndIndex(t *testing.T) {
	_, dir := setup(t)
	for _, name := range []string{"procedures/consolidate.md", "topics/other.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("staged"), 0640))
		git(t, dir, "add", "--", name)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("unstaged"), 0640))
	}
	before, err := os.Stat(filepath.Join(dir, "procedures/consolidate.md"))
	require.NoError(t, err)
	_, err = memory.New(t.Context(), memory.Config{Path: dir, Defaults: consolidate.Procedures})
	require.NoError(t, err)
	require.Equal(t, "2\n", git(t, dir, "rev-list", "--count", "HEAD"))
	require.Equal(t, "procedures/hillclimb.md\n", git(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
	for _, name := range []string{"procedures/consolidate.md", "topics/other.md"} {
		require.Equal(t, "staged", git(t, dir, "show", ":"+name))
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, "unstaged", string(data))
	}
	after, err := os.Stat(filepath.Join(dir, "procedures/consolidate.md"))
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime())
	require.Equal(t, before.Mode(), after.Mode())
}

func TestSeedingCanRestartAfterGitFailure(t *testing.T) {
	for _, lock := range []string{"index.lock", "HEAD.lock"} {
		for _, indexed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/indexed=%t", lock, indexed), func(t *testing.T) {
				_, dir := setup(t)
				name := "procedures/consolidate.md"
				if indexed {
					require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("staged"), 0600))
					git(t, dir, "add", "--", name)
					require.NoError(t, os.Remove(filepath.Join(dir, name)))
				}
				before := git(t, dir, "ls-files", "--stage")
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", lock), nil, 0600))
				_, err := memory.New(t.Context(), memory.Config{Path: dir, Defaults: consolidate.Procedures})
				require.Error(t, err)
				require.NoFileExists(t, filepath.Join(dir, name))
				require.Equal(t, before, git(t, dir, "ls-files", "--stage"))
				require.Equal(t, "1\n", git(t, dir, "rev-list", "--count", "HEAD"))
				require.NoError(t, os.Remove(filepath.Join(dir, ".git", lock)))
				_, err = memory.New(t.Context(), memory.Config{Path: dir, Defaults: consolidate.Procedures})
				require.NoError(t, err)
				require.Empty(t, git(t, dir, "status", "--porcelain"))
			})
		}
	}
}

func TestReadPaths(t *testing.T) {
	s, dir := setup(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "owner/persona.md"), []byte("persona"), 0600))
	text, err := s.Read(t.Context(), "owner/persona.md")
	require.NoError(t, err)
	require.Equal(t, "persona", text)
	for _, name := range []string{"../outside", "/outside", "owner/../core.md", ".git/config", "topics/.git/config", ".", "topics/a\nb", "topics/a\tb", "topics/a\x7fb", "topics/a\u0085b", "topics/a\\b"} {
		_, err := s.Read(t.Context(), name)
		require.Error(t, err, name)
		_, err = s.List(t.Context(), name)
		require.Error(t, err, name)
	}
	require.NoError(t, os.Symlink("../owner/persona.md", filepath.Join(dir, "topics/link.md")))
	_, err = s.Read(t.Context(), "topics/link.md")
	require.Error(t, err)
	names, err := s.List(t.Context(), "topics")
	require.NoError(t, err)
	require.Empty(t, names)
}

func TestInitExistingFilesAndReopen(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	root := filepath.Join(dir, "memory")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "owner"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "owner/persona.md"), []byte("persona\n日本語 [x]\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "outside.txt"), []byte("included"), 0600))
	s, err := memory.New(t.Context(), memory.Config{Path: root})
	require.NoError(t, err)
	require.Equal(t, root+"\n", git(t, root, "rev-parse", "--show-toplevel"))
	require.Equal(t, "outside.txt\nowner/persona.md\n", git(t, root, "ls-tree", "-r", "--name-only", "HEAD"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "topics/untracked.md"), []byte("日本語 untracked"), 0600))
	text, err := s.Read(t.Context(), "topics/untracked.md")
	require.NoError(t, err)
	require.Equal(t, "日本語 untracked", text)
	s, err = memory.New(t.Context(), memory.Config{Path: root})
	require.NoError(t, err)
	names, err := s.List(t.Context(), "owner")
	require.NoError(t, err)
	require.Equal(t, []string{"owner/persona.md"}, names)
	names, err = s.List(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, []string{"outside.txt", "owner/persona.md", "topics/untracked.md"}, names)
	require.Equal(t, "?? topics/untracked.md\n", git(t, root, "status", "--porcelain", "--untracked-files=all"))
}

func TestUnbornRepositoryPreservesStagedFiles(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "core.md"), []byte("staged"), 0600))
	git(t, dir, "add", "core.md")
	_, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	require.Empty(t, git(t, dir, "ls-tree", "--name-only", "HEAD"))
	require.Equal(t, "A  core.md\n", git(t, dir, "status", "--porcelain"))
}

func TestStartupPreservesGitWorktrees(t *testing.T) {
	_, dir := setup(t)
	oldSnapshot := filepath.Join(dir, ".git/noctiluca-snapshots/old")
	git(t, dir, "worktree", "add", "--detach", oldSnapshot, "HEAD")
	missing := filepath.Join(t.TempDir(), "missing")
	git(t, dir, "worktree", "add", "--detach", missing, "HEAD")
	require.NoError(t, os.RemoveAll(missing))
	before := git(t, dir, "worktree", "list", "--porcelain")
	_, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	require.DirExists(t, oldSnapshot)
	require.Equal(t, before, git(t, dir, "worktree", "list", "--porcelain"))
}
