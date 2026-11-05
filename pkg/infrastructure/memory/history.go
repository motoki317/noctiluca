package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (s *store) Revision(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision(ctx, "HEAD")
}

func (s *store) revision(ctx context.Context, revision string) (string, error) {
	if revision == "" {
		return "", errors.New("memory revision is required")
	}
	out, err := s.git(ctx, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

func (s *store) Sizes(ctx context.Context) (domain.MemorySizes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.sizes(ctx)
	if err != nil {
		return result, err
	}
	result.Revision, err = s.revision(ctx, "HEAD")
	return result, err
}

func (s *store) sizes(ctx context.Context) (domain.MemorySizes, error) {
	result := domain.MemorySizes{Limits: s.limits, Files: []domain.MemoryFileSize{}}
	names, err := s.list(ctx, "")
	if err != nil {
		return result, err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	for _, name := range names {
		if err := safePath(root, name); err != nil {
			return result, err
		}
		stat, err := root.Stat(name)
		if err != nil {
			return result, err
		}
		result.Files = append(result.Files, domain.MemoryFileSize{Path: name, Bytes: stat.Size()})
		result.TotalBytes += stat.Size()
		if name == "core.md" {
			result.CoreBytes = stat.Size()
		}
	}
	result.FileCount = len(result.Files)
	return result, nil
}

func (s *store) checkWrite(ctx context.Context, name string, bytes int64) error {
	current, err := s.sizes(ctx)
	if err != nil {
		return err
	}
	total, count, old := current.TotalBytes+bytes, current.FileCount+1, int64(0)
	for _, file := range current.Files {
		if file.Path == name {
			old = file.Bytes
			total -= old
			count--
			break
		}
	}
	// A store above its cap must still permit one-item reductions toward the limit.
	if (bytes > s.limits.FileBytes && bytes >= old) || (total > s.limits.TotalBytes && total >= current.TotalBytes) || (count > s.limits.FileCount && count > current.FileCount) {
		return errors.New("memory size cap exceeded; merge or delete content before writing")
	}
	return nil
}

func (s *store) Snapshot(ctx context.Context, revision string) (domain.MemorySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(ctx, revision)
}

func (s *store) snapshot(ctx context.Context, revision string) (domain.MemorySnapshot, error) {
	commit, err := s.revision(ctx, revision)
	if err != nil {
		return domain.MemorySnapshot{}, err
	}
	dir, err := os.MkdirTemp(s.snapshots, "snapshot-")
	if err != nil {
		return domain.MemorySnapshot{}, err
	}
	if _, err := s.git(ctx, "worktree", "add", "--quiet", "--detach", dir, commit); err != nil {
		_ = os.RemoveAll(dir)
		return domain.MemorySnapshot{}, err
	}
	var once sync.Once
	var closeErr error
	close := func() error {
		once.Do(func() { _, closeErr = s.git(context.WithoutCancel(ctx), "worktree", "remove", "--force", dir) })
		return closeErr
	}
	memory := &store{dir: dir, snapshots: s.snapshots, limits: s.limits}
	return domain.MemorySnapshot{Store: memory, Dir: dir, Close: close}, nil
}

func (s *store) clean(ctx context.Context) error {
	status, err := s.git(ctx, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) > 0 {
		return errors.New("memory has uncommitted edits; commit or resolve them first")
	}
	return nil
}

func (s *store) Revert(ctx context.Context, revision string) (result string, resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.clean(ctx); err != nil {
		return "", err
	}
	commit, err := s.revision(ctx, revision)
	if err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "merge-base", "--is-ancestor", commit, "HEAD"); err != nil {
		return "", errors.New("revert requires a commit in the current memory history")
	}
	names, err := s.git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "-z", commit)
	if err != nil {
		return "", err
	}
	for _, name := range strings.Split(strings.TrimSuffix(string(names), "\x00"), "\x00") {
		if !validPath(name) {
			return "", errors.New("commit changes paths outside managed memory")
		}
	}
	snapshot, err := s.snapshot(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close()) }()
	candidate := snapshot.Store.(*store)
	if _, err := candidate.git(ctx, "revert", "--no-edit", commit); err != nil {
		return "", errors.New("memory revert conflicts; no live files changed")
	}
	// Rollback restores prior knowledge even when a failed shrink or a lower cap leaves it oversized.
	next, err := candidate.revision(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "merge", "--ff-only", "--no-edit", "--no-overwrite-ignore", next); err != nil {
		return "", fmt.Errorf("apply memory revert: %w", err)
	}
	return next, nil
}

func (s *store) Delete(ctx context.Context, name, message string) (revision string, resultErr error) {
	actor, ok := domain.ToolSessionFromContext(ctx)
	if !ok || (actor.Actor != domain.ActorOwner && actor.Actor != domain.ActorMember) || (ownerPath(name) && actor.Actor != domain.ActorOwner) {
		return "", errors.New("memory delete requires permission for the path")
	}
	if !validPath(name) || strings.TrimSpace(message) == "" || strings.ContainsRune(message, 0) {
		return "", errors.New("valid memory path and commit message required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.clean(ctx); err != nil {
		return "", err
	}
	snapshot, err := s.snapshot(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close()) }()
	candidate := snapshot.Store.(*store)
	if _, err := candidate.git(ctx, "rm", "--", name); err != nil {
		return "", err
	}
	if _, err := candidate.git(ctx, "commit", "--quiet", "--message", message); err != nil {
		return "", err
	}
	next, err := candidate.revision(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	_, err = s.git(ctx, "merge", "--ff-only", "--no-edit", "--no-overwrite-ignore", next)
	return next, err
}
