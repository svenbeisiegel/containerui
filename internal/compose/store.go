package compose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// FileName is the name of a stored compose file inside its project folder.
const FileName = "compose.yaml"

// Store keeps the compose files deployed through the UI, one folder per
// project: <Dir>/<project>/compose.yaml. The folder is the compose project
// directory, so relative paths in the file (bind mounts, env_file, .env)
// resolve inside it.
type Store struct {
	Dir string // absolute
}

// ProjectDir returns the folder of a project. The name is validated, so it
// cannot leave Dir.
func (s Store) ProjectDir(project string) (string, error) {
	if err := runtime.ValidateProjectName(project); err != nil {
		return "", err
	}
	if !filepath.IsAbs(s.Dir) {
		return "", fmt.Errorf("compose store %q is not an absolute path", s.Dir)
	}
	return filepath.Join(s.Dir, project), nil
}

// Path returns the compose file of a project.
func (s Store) Path(project string) (string, error) {
	dir, err := s.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Has reports whether a compose file is stored for the project.
func (s Store) Has(project string) bool {
	p, err := s.Path(project)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Read returns the stored compose file (an os.ErrNotExist error if none).
func (s Store) Read(project string) ([]byte, error) {
	p, err := s.Path(project)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// Write stores the compose file atomically and returns its path.
func (s Store) Write(project string, data []byte) (string, error) {
	dir, err := s.ProjectDir(project)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".compose-*.yaml")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	path := filepath.Join(dir, FileName)
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return "", werr
	}
	return path, nil
}

// WriteTemp writes data next to where the project's compose file is
// stored, so the compose tool resolves relative paths the same way, and
// returns its path and a cleanup function that removes it again (and the
// project folder if WriteTemp created it).
func (s Store) WriteTemp(project string, data []byte) (string, func(), error) {
	dir, err := s.ProjectDir(project)
	if err != nil {
		return "", nil, err
	}
	_, statErr := os.Stat(dir)
	created := errors.Is(statErr, os.ErrNotExist)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	tmp, err := os.CreateTemp(dir, ".validate-*.yaml")
	if err == nil {
		_, err = tmp.Write(data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
	}
	cleanup := func() {
		if tmp != nil {
			_ = os.Remove(tmp.Name())
		}
		if created {
			_ = os.Remove(dir) // only succeeds if empty
		}
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return tmp.Name(), cleanup, nil
}

// Delete removes the stored compose file, and the project folder if
// nothing else is left in it.
func (s Store) Delete(project string) error {
	p, err := s.Path(project)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	_ = os.Remove(filepath.Dir(p))
	return nil
}

// List returns the projects with a stored compose file, sorted.
func (s Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && runtime.ValidateProjectName(e.Name()) == nil && s.Has(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
