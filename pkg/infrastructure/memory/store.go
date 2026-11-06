package memory

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/motoki317/noctiluca/pkg/domain"
)

const CoreLimit = 8 * 1024

type Config struct {
	Path   string              `mapstructure:"path"`
	Limits domain.MemoryLimits `mapstructure:"limits"`
}
type store struct {
	dir       string
	snapshots string
	limits    domain.MemoryLimits
	mu        sync.Mutex
}

func New(ctx context.Context, config Config) (domain.MemoryStore, error) {
	if config.Path == "" {
		return nil, errors.New("memory.path is required")
	}
	dir, err := filepath.Abs(config.Path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	limits := config.Limits
	if limits.CoreBytes == 0 {
		limits.CoreBytes = CoreLimit
	}
	if limits.FileBytes == 0 {
		limits.FileBytes = 64 * 1024
	}
	if limits.TotalBytes == 0 {
		limits.TotalBytes = 1024 * 1024
	}
	if limits.FileCount == 0 {
		limits.FileCount = 128
	}
	if limits.CoreBytes < 1 || limits.CoreBytes > CoreLimit || limits.FileBytes < 1 || limits.TotalBytes < 1 || limits.FileCount < 1 {
		return nil, errors.New("memory limits must be positive; core_bytes cannot exceed 8192")
	}
	s := &store{dir: dir, limits: limits}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for _, name := range []string{"owner", "topics", "procedures"} {
		if err := safePath(r, name); err != nil {
			return nil, err
		}
		if err := r.MkdirAll(name, 0700); err != nil {
			return nil, err
		}
	}
	if _, err := s.Read(ctx, "core.md"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	info, err := r.Lstat(".git")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("memory .git must not be a symlink")
	}
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := s.git(ctx, "init", "--quiet"); err != nil {
			return nil, err
		}
		files, err := s.List(ctx, "")
		if err != nil {
			return nil, err
		}
		if len(files) > 0 {
			if _, err := s.git(ctx, append([]string{"add", "--"}, files...)...); err != nil {
				return nil, err
			}
		}
		if _, err := s.git(ctx, "commit", "--quiet", "--allow-empty", "--message", "Initialize memory"); err != nil {
			return nil, err
		}
	}
	root, err := s.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(root)) != dir {
		return nil, errors.New("memory must have its own git repository")
	}
	if _, err := s.git(ctx, "rev-parse", "--quiet", "--verify", "HEAD"); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, err
		}
		// An empty baseline must leave the owner's staged files in the index.
		if _, err := s.git(ctx, "commit", "--quiet", "--allow-empty", "--only", "--message", "Initialize memory"); err != nil {
			return nil, err
		}
	}
	common, err := s.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	s.snapshots = filepath.Join(strings.TrimSpace(string(common)), "noctiluca-snapshots")
	// Only this instance's disposable snapshots are swept. Other worktrees belong to the owner.
	if err := os.RemoveAll(s.snapshots); err != nil {
		return nil, err
	}
	if _, err := s.git(ctx, "worktree", "prune"); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.snapshots, 0700); err != nil {
		return nil, err
	}
	return s, nil
}

func ownerPath(name string) bool {
	return name == "owner" || strings.HasPrefix(name, "owner/")
}

func validPath(name string) bool {
	if name == "." || !fs.ValidPath(name) || strings.ContainsRune(name, '\\') || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".git" {
			return false
		}
	}
	return true
}

func safePath(root *os.Root, name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("memory paths must not contain symlinks")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return errors.New("memory parent is not a directory")
		}
		if i == len(parts)-1 && !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("memory path is not a regular file")
		}
	}
	return nil
}

func (s *store) Read(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validPath(name) {
		return "", errors.New("invalid memory path")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := os.OpenRoot(s.dir)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if err := safePath(r, name); err != nil {
		return "", err
	}
	data, err := r.ReadFile(name)
	return string(data), err
}

func (s *store) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" && !validPath(prefix) {
		return nil, errors.New("invalid memory prefix")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(ctx, prefix)
}

func (s *store) list(ctx context.Context, prefix string) ([]string, error) {
	r, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var names []string
	err = fs.WalkDir(r.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if entry.IsDir() {
			if validPath(name) {
				return nil
			}
			return fs.SkipDir
		}
		if !validPath(name) || (prefix != "" && !strings.HasPrefix(name, prefix+"/")) {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		names = append(names, name)
		return nil
	})
	return names, err
}

func (s *store) Write(ctx context.Context, name, content, message string) error {
	_, err := s.WriteRevision(ctx, name, content, message)
	return err
}

func (s *store) WriteRevision(ctx context.Context, name, content, message string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(ctx, name, content, message); err != nil {
		return "", err
	}
	return s.revision(context.WithoutCancel(ctx), "HEAD")
}

func (s *store) write(ctx context.Context, name, content, message string) error {
	if !validPath(name) {
		return errors.New("invalid memory path")
	}
	if ownerPath(name) {
		return errors.New("owner/ is read-only; the owner edits these files by hand")
	}
	if name == "core.md" && int64(len(content)) > s.limits.CoreBytes {
		return fmt.Errorf("core.md exceeds %d bytes; merge or delete content before writing", s.limits.CoreBytes)
	}
	if strings.TrimSpace(message) == "" || strings.ContainsRune(message, 0) {
		return errors.New("a commit message is required")
	}
	r, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := safePath(r, name); err != nil {
		return err
	}
	status, err := s.git(ctx, "status", "--porcelain", "--untracked-files=all", "--", name)
	if err != nil {
		return err
	}
	if len(status) > 0 {
		return errors.New("memory file has uncommitted edits; commit or resolve them before writing")
	}
	old, err := r.ReadFile(name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.checkWrite(ctx, name, int64(len(content))); err != nil {
		return err
	}
	if err := r.MkdirAll(path.Dir(name), 0700); err != nil {
		return err
	}
	if err := replace(r, name, []byte(content)); err != nil {
		return err
	}
	rollback := func(cause error) error {
		if exists {
			return errors.Join(cause, replace(r, name, old))
		}
		_, indexErr := s.git(context.WithoutCancel(ctx), "update-index", "--force-remove", "--", name)
		return errors.Join(cause, indexErr, r.Remove(name))
	}
	if !exists {
		if _, err := s.git(ctx, "add", "--intent-to-add", "--", name); err != nil {
			return rollback(err)
		}
	}
	// A path-only commit preserves other staged work, including a partially staged file.
	if _, err := s.git(ctx, "commit", "--quiet", "--only", "--allow-empty", "--cleanup=verbatim", "--message", message, "--", name); err != nil {
		return rollback(err)
	}
	return nil
}

func replace(root *os.Root, name string, data []byte) error {
	tmp := path.Join(path.Dir(name), ".memory-"+rand.Text())
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = file.Write(data)
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

func (s *store) Search(ctx context.Context, query string) ([]domain.MemoryMatch, error) {
	if query == "" || strings.ContainsAny(query, "\x00\r\n") {
		return nil, errors.New("search requires a nonempty single-line query")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.list(ctx, "")
	if err != nil || len(names) == 0 {
		return nil, err
	}
	args := []string{"grep", "--no-index", "--no-exclude-standard", "--no-textconv", "--no-color", "-I", "-n", "-z", "-F", "-e", query, "--"}
	out, err := s.git(ctx, append(args, names...)...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var matches []domain.MemoryMatch
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			return nil, errors.New("invalid git grep output")
		}
		number, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		matches = append(matches, domain.MemoryMatch{Path: parts[0], Line: number, Text: parts[2]})
		if len(matches) == 100 {
			break
		}
	}
	return matches, nil
}

func (s *store) git(ctx context.Context, args ...string) ([]byte, error) {
	options := []string{"--literal-pathspecs", "-c", "user.name=noctiluca", "-c", "user.email=noctiluca@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
	cmd := exec.CommandContext(ctx, "git", append(options, args...)...)
	cmd.Dir = s.dir
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("memory git %s failed: %w", args[0], err)
	}
	return out, nil
}
