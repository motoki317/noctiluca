package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/motoki317/noctiluca/pkg/domain"
)

type Config struct {
	Path string
}
type store struct {
	dir string
}

func New(ctx context.Context, config Config) (domain.MemoryStore, error) {
	if config.Path == "" {
		return nil, errors.New("memory.dir is required")
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
	s := &store{dir: dir}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := safePath(r, "owner"); err != nil {
		return nil, err
	}
	if err := r.MkdirAll("owner", 0700); err != nil {
		return nil, err
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
		// --only keeps the owner's staged files out of the empty first commit.
		if _, err := s.git(ctx, "commit", "--quiet", "--allow-empty", "--only", "--message", "Initialize memory"); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *store) Dir() string { return s.dir }

func (s *store) HasRemote(ctx context.Context) (bool, error) {
	_, err := s.git(ctx, "config", "--get-regexp", `^remote\..*\.url$`)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
