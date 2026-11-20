package memory_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/stretchr/testify/require"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
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

func TestGitIgnoresInheritedEnvironment(t *testing.T) {
	_, decoy := setup(t)
	config := filepath.Join(decoy, ".git", "config")
	before, err := os.ReadFile(config)
	require.NoError(t, err)
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))

	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	git(t, dir, "remote", "add", "backup", t.TempDir())
	require.DirExists(t, filepath.Join(dir, ".git"))
	after, err := os.ReadFile(config)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestHasRemote(t *testing.T) {
	s, dir := setup(t)
	hasRemote, err := s.HasRemote(t.Context())
	require.NoError(t, err)
	require.False(t, hasRemote)
	git(t, dir, "remote", "add", "backup", t.TempDir())
	hasRemote, err = s.HasRemote(t.Context())
	require.NoError(t, err)
	require.True(t, hasRemote)
}

func TestHasRemoteWithoutURL(t *testing.T) {
	s, dir := setup(t)
	git(t, dir, "config", "remote.backup.prune", "true")
	hasRemote, err := s.HasRemote(t.Context())
	require.NoError(t, err)
	require.False(t, hasRemote)
}

func TestInitEmptyMemory(t *testing.T) {
	dir := t.TempDir()
	_, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	require.Equal(t, "Initialize memory\n", git(t, dir, "log", "--format=%s"))
	require.Empty(t, git(t, dir, "ls-tree", "--name-only", "HEAD"))
	_, err = os.Lstat(filepath.Join(dir, "procedures"))
	require.ErrorIs(t, err, os.ErrNotExist)
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
	require.NoError(t, os.Mkdir(filepath.Join(dir, "topics"), 0700))
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
	require.NoError(t, os.Mkdir(filepath.Join(root, "topics"), 0700))
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
