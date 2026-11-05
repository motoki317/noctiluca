package memory_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
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
func actor(t *testing.T, actor domain.Actor) context.Context {
	return domain.WithToolSession(t.Context(), domain.ToolSession{Key: "test", Actor: actor})
}
func setup(t *testing.T) (domain.MemoryStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
	s, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	return s, dir
}

func TestWritesCommitsAndIsolation(t *testing.T) {
	s, dir := setup(t)
	ctx := actor(t, domain.ActorMember)
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
	member := actor(t, domain.ActorMember)
	owner := actor(t, domain.ActorOwner)
	require.ErrorContains(t, s.Write(member, "owner/persona.md", "persona", "Try owner write"), "owner permission")
	require.Error(t, s.Write(t.Context(), "topics/unknown.md", "unknown", "Missing actor"))
	require.NoError(t, s.Write(owner, "owner/persona.md", "persona", "Set persona"))
	text, err := s.Read(member, "owner/persona.md")
	require.NoError(t, err)
	require.Equal(t, "persona", text)
	for _, name := range []string{"../outside", "/outside", "owner/../core.md", ".git/config", "topics/.git/config", "misc.md", "topics/a\nb"} {
		require.Error(t, s.Write(owner, name, "bad", "Invalid path"), name)
		_, err := s.Read(owner, name)
		require.Error(t, err, name)
	}
	require.NoError(t, os.Symlink("../owner/persona.md", filepath.Join(dir, "topics/link.md")))
	require.Error(t, s.Write(member, "topics/link.md", "changed", "Symlink bypass"))
	_, err = s.Read(member, "topics/link.md")
	require.Error(t, err)
	require.NoError(t, s.Write(owner, "core.md", strings.Repeat("x", 8192), "Fill core"))
	head := git(t, dir, "rev-parse", "HEAD")
	require.ErrorContains(t, s.Write(owner, "core.md", strings.Repeat("x", 8193), "Overflow core"), "merge or delete")
	require.Error(t, s.Write(owner, "core.md", strings.Repeat("語", 2731), "Multibyte overflow"))
	require.Equal(t, head, git(t, dir, "rev-parse", "HEAD"))
	text, err = s.Read(member, "core.md")
	require.NoError(t, err)
	require.Len(t, text, 8192)
}

func TestInitExistingFilesSearchAndReopen(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	root := filepath.Join(dir, "memory")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "owner"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "owner/persona.md"), []byte("persona\n日本語 [x]\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "outside.txt"), []byte("excluded"), 0600))
	s, err := memory.New(t.Context(), memory.Config{Path: root})
	require.NoError(t, err)
	require.Equal(t, root+"\n", git(t, root, "rev-parse", "--show-toplevel"))
	require.Equal(t, "owner/persona.md\n", git(t, root, "ls-tree", "-r", "--name-only", "HEAD"))
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
	require.NoError(t, s.Write(actor(t, domain.ActorOwner), "owner/persona.md", "updated", "Update preferences"))
}

func TestConcurrentWritesAndCancellation(t *testing.T) {
	s, dir := setup(t)
	ctx := actor(t, domain.ActorMember)
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
