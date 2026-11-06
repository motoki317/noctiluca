package memory_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/motoki317/noctiluca/pkg/domain"
	"github.com/motoki317/noctiluca/pkg/infrastructure/memory"
	"github.com/stretchr/testify/require"
)

func TestMemoryHistoryRevertAndWorktree(t *testing.T) {
	store, dir := setup(t)
	history := store.(domain.MemoryHistory)
	owner := actor(t, domain.ActorOwner)
	before, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.Write(owner, "topics/change.md", "candidate", "Try one delta"))
	delta, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.Write(owner, "topics/other.md", "preserve", "Keep independent work"))
	snapshot, err := history.Snapshot(t.Context(), before)
	require.NoError(t, err)
	require.DirExists(t, snapshot.Dir)
	require.NotEqual(t, dir, snapshot.Dir)
	_, err = snapshot.Store.Read(t.Context(), "topics/change.md")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, snapshot.Close())
	require.NoError(t, snapshot.Close())
	require.NoDirExists(t, snapshot.Dir)
	reverted, err := history.Revert(owner, delta)
	require.NoError(t, err)
	require.NotEqual(t, delta, reverted)
	require.NoFileExists(t, filepath.Join(dir, "topics/change.md"))
	text, err := store.Read(t.Context(), "topics/other.md")
	require.NoError(t, err)
	require.Equal(t, "preserve", text)
	require.Equal(t, 1, strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree "))
	_, err = history.Delete(owner, "topics/other.md", "Forget obsolete item")
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dir, "topics/other.md"))
}

func TestUnbornRepositoryPreservesStagedFiles(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "core.md"), []byte("staged"), 0600))
	git(t, dir, "add", "core.md")
	store, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	_, err = store.(domain.MemoryHistory).Revision(t.Context())
	require.NoError(t, err)
	require.Empty(t, git(t, dir, "ls-tree", "--name-only", "HEAD"))
	require.Equal(t, "A  core.md\n", git(t, dir, "status", "--porcelain"))
}

func TestRevertConflictLeavesLiveMemoryUntouched(t *testing.T) {
	store, dir := setup(t)
	history := store.(domain.MemoryHistory)
	owner := actor(t, domain.ActorOwner)
	require.NoError(t, store.Write(owner, "core.md", "original", "Initial rules"))
	require.NoError(t, store.Write(owner, "core.md", "candidate", "Try a delta"))
	delta, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.Write(owner, "core.md", "later edit", "Keep later work"))
	head, err := history.Revision(t.Context())
	require.NoError(t, err)
	_, err = history.Revert(owner, delta)
	require.ErrorContains(t, err, "conflicts")
	after, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.Equal(t, head, after)
	text, err := store.Read(t.Context(), "core.md")
	require.NoError(t, err)
	require.Equal(t, "later edit", text)
	require.Empty(t, git(t, dir, "status", "--porcelain"))
	require.Equal(t, 1, strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree "))
}

func TestMemoryCapsAndSizeReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	caps := domain.MemoryLimits{CoreBytes: 8, FileBytes: 10, TotalBytes: 15, FileCount: 2}
	store, err := memory.New(t.Context(), memory.Config{Path: dir, Limits: caps})
	require.NoError(t, err)
	owner := actor(t, domain.ActorOwner)
	require.ErrorContains(t, store.Write(owner, "core.md", strings.Repeat("x", 9), "Too much core"), "8 bytes")
	require.NoError(t, store.Write(owner, "core.md", strings.Repeat("x", 8), "At the core cap"))
	require.ErrorContains(t, store.Write(owner, "topics/a.md", strings.Repeat("x", 11), "Too large"), "cap")
	require.ErrorContains(t, store.Write(owner, "topics/a.md", strings.Repeat("x", 8), "Too much total"), "cap")
	require.NoError(t, store.Write(owner, "topics/a.md", strings.Repeat("x", 7), "At total cap"))
	require.ErrorContains(t, store.Write(owner, "topics/b.md", "", "Too many files"), "cap")
	sizes, err := store.(domain.MemoryHistory).Sizes(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 8, sizes.CoreBytes)
	require.EqualValues(t, 15, sizes.TotalBytes)
	require.Equal(t, 2, sizes.FileCount)
	require.Len(t, sizes.Files, 2)
	require.Equal(t, caps, sizes.Limits)
	require.NoError(t, store.Write(owner, "topics/a.md", "small", "Shrink"))
	_, err = store.(domain.MemoryHistory).Delete(owner, "topics/a.md", "Forget")
	require.NoError(t, err)
	require.NoError(t, store.Write(owner, "topics/b.md", "new", "Replace forgotten item"))
}

func TestRevertFailedShrinkRestoresOversizedBaseline(t *testing.T) {
	store, dir := setup(t)
	owner := actor(t, domain.ActorOwner)
	require.NoError(t, store.Write(owner, "topics/large.md", "original oversized memory", "Keep useful knowledge"))
	store, err := memory.New(t.Context(), memory.Config{Path: dir, Limits: domain.MemoryLimits{FileBytes: 8, TotalBytes: 16}})
	require.NoError(t, err)
	require.NoError(t, store.Write(owner, "topics/large.md", "short", "Try shrinking"))
	history := store.(domain.MemoryHistory)
	delta, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.Write(owner, "topics/later.md", "later", "Preserve independent edit"))
	_, err = history.Revert(owner, delta)
	require.NoError(t, err)
	text, err := store.Read(t.Context(), "topics/large.md")
	require.NoError(t, err)
	require.Equal(t, "original oversized memory", text)
	require.ErrorContains(t, store.Write(owner, "topics/new.md", "growth", "Reject new growth"), "cap")
}

func TestRevertPreservesIgnoredLocalFile(t *testing.T) {
	store, dir := setup(t)
	owner := actor(t, domain.ActorOwner)
	require.NoError(t, store.Write(owner, "topics/item.md", "tracked", "Add item"))
	history := store.(domain.MemoryHistory)
	_, err := history.Delete(owner, "topics/item.md", "Remove item")
	require.NoError(t, err)
	delta, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "topics"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git/info/exclude"), []byte("topics/item.md\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "topics/item.md"), []byte("local ignored work"), 0600))
	_, err = history.Revert(owner, delta)
	require.Error(t, err)
	head, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.Equal(t, delta, head)
	text, err := store.Read(t.Context(), "topics/item.md")
	require.NoError(t, err)
	require.Equal(t, "local ignored work", text)
	require.Equal(t, 1, strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree "))
}

func TestConcurrentWritesReturnTheirOwnRevision(t *testing.T) {
	store, dir := setup(t)
	history := store.(domain.MemoryHistory)
	owner := actor(t, domain.ActorOwner)
	type written struct {
		Path, Revision string
		Err            error
	}
	results := make(chan written, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			path := fmt.Sprintf("topics/%d.md", i)
			revision, err := history.WriteRevision(owner, path, path, "Record item")
			results <- written{path, revision, err}
		})
	}
	wg.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result.Err)
		require.Equal(t, result.Path+"\n", git(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", result.Revision))
	}
}

func TestOversizedCoreRemainsReadable(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", memory.CoreLimit+1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "core.md"), []byte(content), 0600))
	store, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	text, err := store.Read(t.Context(), "core.md")
	require.NoError(t, err)
	require.Equal(t, content, text)
	history := store.(domain.MemoryHistory)
	sizes, err := history.Sizes(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, len(content), sizes.CoreBytes)
	snapshot, err := history.Snapshot(t.Context(), sizes.Revision)
	require.NoError(t, err)
	text, err = snapshot.Store.Read(t.Context(), "core.md")
	require.NoError(t, err)
	require.Equal(t, content, text)
	require.NoError(t, snapshot.Close())
	require.ErrorContains(t, store.Write(actor(t, domain.ActorOwner), "core.md", content, "Still too large"), "merge or delete")
	require.NoError(t, store.Write(actor(t, domain.ActorOwner), "core.md", "smaller", "Reduce core"))
}

func TestOwnerMemoryPaths(t *testing.T) {
	store, dir := setup(t)
	history := store.(domain.MemoryHistory)
	for _, name := range []string{"owner/soul.md", "owner/user.md", "owner/rules.md", "owner/nested/note.md"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("original"), 0600))
	}
	git(t, dir, "add", "owner")
	git(t, dir, "commit", "-m", "Handwritten instructions")
	head, err := history.Revision(t.Context())
	require.NoError(t, err)
	for _, identity := range []domain.Actor{domain.ActorOwner, domain.ActorMember} {
		ctx := actor(t, identity)
		for _, name := range []string{"owner", "owner/soul.md", "owner/user.md", "owner/rules.md", "owner/nested/note.md", "owner/new.md"} {
			require.ErrorContains(t, store.Write(ctx, name, "changed", "Change instruction"), "owner/ is read-only")
			_, err := history.Delete(ctx, name, "Delete instruction")
			require.ErrorContains(t, err, "owner/ is read-only")
		}
		_, err := history.Revert(ctx, head)
		require.ErrorContains(t, err, "changes owner/")
	}
	after, err := history.Revision(t.Context())
	require.NoError(t, err)
	require.Equal(t, head, after)
	require.Empty(t, git(t, dir, "status", "--porcelain"))
	require.Equal(t, 1, strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree "))
}

func TestModelMemoryPaths(t *testing.T) {
	store, _ := setup(t)
	history := store.(domain.MemoryHistory)
	ctx := actor(t, domain.ActorMember)
	for _, name := range []string{"core.md", "procedures/heartbeat.md", "notes/research/item.md", "index.md", "owner-notes.md"} {
		revision, err := history.WriteRevision(ctx, name, "original", "Record memory")
		require.NoError(t, err)
		text, err := store.Read(ctx, name)
		require.NoError(t, err)
		require.Equal(t, "original", text)
		matches, err := store.Search(ctx, "original")
		require.NoError(t, err)
		require.Contains(t, matches, domain.MemoryMatch{Path: name, Line: 1, Text: "original"})
		_, err = history.Delete(ctx, name, "Forget memory")
		require.NoError(t, err)
		_, err = history.Revert(ctx, "HEAD")
		require.NoError(t, err)
		_, err = history.Revert(ctx, revision)
		require.NoError(t, err)
		_, err = store.Read(ctx, name)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.NoError(t, store.Write(ctx, "notes/research/item.md", "note", "Record note"))
	names, err := store.List(ctx, "notes/research")
	require.NoError(t, err)
	require.Equal(t, []string{"notes/research/item.md"}, names)
	sizes, err := history.Sizes(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.MemoryFileSize{{Path: "notes/research/item.md", Bytes: 4}}, sizes.Files)
}

func TestStartupSweepsOnlyOwnedSnapshots(t *testing.T) {
	store, dir := setup(t)
	history := store.(domain.MemoryHistory)
	revision, err := history.Revision(t.Context())
	require.NoError(t, err)
	stale, err := history.Snapshot(t.Context(), revision)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(stale.Dir, filepath.Join(dir, ".git", "noctiluca-snapshots")+string(filepath.Separator)))
	other := filepath.Join(t.TempDir(), "owner-worktree")
	git(t, dir, "worktree", "add", "--detach", other, revision)
	reopened, err := memory.New(t.Context(), memory.Config{Path: dir})
	require.NoError(t, err)
	require.NoDirExists(t, stale.Dir)
	require.DirExists(t, other)
	trees := git(t, dir, "worktree", "list", "--porcelain")
	require.NotContains(t, trees, stale.Dir)
	require.Contains(t, trees, other)
	fresh, err := reopened.(domain.MemoryHistory).Snapshot(t.Context(), revision)
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
}

func TestRevertRefusesLaterRenameIntoOwner(t *testing.T) {
	store, dir := setup(t)
	history := store.(domain.MemoryHistory)
	ctx := actor(t, domain.ActorMember)
	require.NoError(t, store.Write(ctx, "notes/item.md", "original\n", "Record note"))
	delta, err := history.WriteRevision(ctx, "notes/item.md", "changed\n", "Update note")
	require.NoError(t, err)
	git(t, dir, "mv", "notes/item.md", "owner/rules.md")
	git(t, dir, "commit", "-m", "Keep as handwritten rules")
	head, err := history.Revision(ctx)
	require.NoError(t, err)
	_, err = history.Revert(ctx, delta)
	require.ErrorContains(t, err, "changes owner/")
	after, err := history.Revision(ctx)
	require.NoError(t, err)
	require.Equal(t, head, after)
	text, err := store.Read(ctx, "owner/rules.md")
	require.NoError(t, err)
	require.Equal(t, "changed\n", text)
	require.Empty(t, git(t, dir, "status", "--porcelain"))
	require.Equal(t, 1, strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree "))
}
