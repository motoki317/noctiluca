package memory_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	s, err := memory.New(t.Context(), memory.Config{Path: dir, Defaults: consolidate.Procedures})
	require.NoError(t, err)
	require.Equal(t, dir, s.Dir())
	require.Equal(t, "3\n", git(t, dir, "rev-list", "--count", "HEAD"))
	for _, name := range []string{"procedures/consolidate.md", "procedures/hillclimb.md"} {
		want, err := consolidate.Procedures.ReadFile(name)
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, string(want), git(t, dir, "show", "HEAD:"+name))
		require.Equal(t, "Add default "+name+"\n", git(t, dir, "log", "-1", "--format=%s", "--", name))
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
	_, err = s.Read(t.Context(), "procedures/hillclimb.md")
	require.ErrorIs(t, err, os.ErrNotExist)
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

func TestWritesCommitsAndIsolation(t *testing.T) {
	s, dir := setup(t)
	ctx := t.Context()
	require.NoError(t, s.Write(ctx, "topics/one.md", "first\n", "Record the first fact"))
	require.NoError(t, s.Write(ctx, "topics/one.md", "first\n", "Confirm the first fact"))
	require.Equal(t, "3\n", git(t, dir, "rev-list", "--count", "HEAD"))
	require.Equal(t, "Confirm the first fact\n\n", git(t, dir, "log", "-1", "--format=%B"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "topics/other.md"), []byte("staged"), 0600))
	git(t, dir, "add", "topics/other.md")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "topics/other.md"), []byte("unstaged"), 0600))
	require.NoError(t, s.Write(ctx, "topics/one.md", "changed\n", "Update the fact"))
	require.Equal(t, "topics/one.md\n", git(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
	require.Equal(t, "staged", git(t, dir, "show", ":topics/other.md"))
	data, err := os.ReadFile(filepath.Join(dir, "topics/other.md"))
	require.NoError(t, err)
	require.Equal(t, "unstaged", string(data))
	require.ErrorContains(t, s.Write(ctx, "topics/other.md", "overwrite", "Reject dirty target"), "uncommitted")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git/index.lock"), nil, 0600))
	require.Error(t, s.Write(ctx, "topics/one.md", "failed", "Cannot acquire git lock"))
	text, err := s.Read(ctx, "topics/one.md")
	require.NoError(t, err)
	require.Equal(t, "changed\n", text)
	require.NoError(t, os.Remove(filepath.Join(dir, ".git/index.lock")))
	require.NoError(t, s.Write(ctx, "topics/one.md", "recovered", "Recover after lock"))
}

func TestPathsPermissionsAndCoreLimit(t *testing.T) {
	s, dir := setup(t)
	ctx := t.Context()
	require.NoError(t, s.Write(t.Context(), "notes/unknown.md", "unknown", "Record a note"))
	require.ErrorContains(t, s.Write(ctx, "owner/persona.md", "persona", "Set persona"), "owner/ is read-only")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "owner/persona.md"), []byte("persona"), 0600))
	text, err := s.Read(ctx, "owner/persona.md")
	require.NoError(t, err)
	require.Equal(t, "persona", text)
	for _, name := range []string{"../outside", "/outside", "owner/../core.md", ".git/config", "topics/.git/config", ".", "topics/a\nb", "topics/a\tb", "topics/a\x7fb", "topics/a\u0085b", "topics/a\\b"} {
		require.Error(t, s.Write(ctx, name, "bad", "Invalid path"), name)
		_, err := s.Read(ctx, name)
		require.Error(t, err, name)
	}
	require.NoError(t, os.Symlink("../owner/persona.md", filepath.Join(dir, "topics/link.md")))
	require.Error(t, s.Write(ctx, "topics/link.md", "changed", "Symlink bypass"))
	_, err = s.Read(ctx, "topics/link.md")
	require.Error(t, err)
	require.NoError(t, s.Write(ctx, "core.md", strings.Repeat("x", 8192), "Fill core"))
	head := git(t, dir, "rev-parse", "HEAD")
	require.ErrorContains(t, s.Write(ctx, "core.md", strings.Repeat("x", 8193), "Overflow core"), "merge or delete")
	require.Error(t, s.Write(ctx, "core.md", strings.Repeat("語", 2731), "Multibyte overflow"))
	require.Equal(t, head, git(t, dir, "rev-parse", "HEAD"))
	text, err = s.Read(ctx, "core.md")
	require.NoError(t, err)
	require.Len(t, text, 8192)
}

func TestInitExistingFilesSearchAndReopen(t *testing.T) {
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
	matches, err := s.Search(t.Context(), "[x]")
	require.NoError(t, err)
	require.Equal(t, []domain.MemoryMatch{{Path: "owner/persona.md", Line: 2, Text: "日本語 [x]"}}, matches)
	require.NoError(t, os.WriteFile(filepath.Join(root, "topics/untracked.md"), []byte("日本語 untracked"), 0600))
	matches, err = s.Search(t.Context(), "日本")
	require.NoError(t, err)
	require.Len(t, matches, 2)
	matches, err = s.Search(t.Context(), "missing")
	require.NoError(t, err)
	require.Empty(t, matches)
	_, err = s.Search(t.Context(), "")
	require.Error(t, err)
	s, err = memory.New(t.Context(), memory.Config{Path: root})
	require.NoError(t, err)
	names, err := s.List(t.Context(), "owner")
	require.NoError(t, err)
	require.Equal(t, []string{"owner/persona.md"}, names)
	require.ErrorContains(t, s.Write(t.Context(), "owner/persona.md", "updated", "Update preferences"), "owner/ is read-only")
}

func TestConcurrentWritesAndCancellation(t *testing.T) {
	s, dir := setup(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			require.NoError(t, s.Write(ctx, "topics/parallel.md", strings.Repeat("x", i), "Concurrent update"))
		})
	}
	wg.Wait()
	require.Equal(t, "9\n", git(t, dir, "rev-list", "--count", "HEAD"))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, s.Write(cancelled, "topics/parallel.md", "cancelled", "Cancelled update"), context.Canceled)
}
