package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

var (
	errEscapes  = errors.New("path escapes the workspace")
	errSymlink  = errors.New("symlinks are not allowed in the workspace")
	errNotFile  = errors.New("not a regular file")
	errNotDir   = errors.New("not a directory")
	errTooLarge = errors.New("file is too large")
)

// resolvePath maps a caller supplied path into the sandbox root.
func (s *Server) resolvePath(p string) (full string, rel string, err error) {
	if p == "" {
		p = "."
	}
	if strings.ContainsRune(p, 0) {
		return "", "", errors.New("path contains a null byte")
	}
	if strings.HasPrefix(p, "~") {
		return "", "", errors.New("path must not use ~ expansion")
	}
	root := s.cfg.Root
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(root, p))
	}
	if !inside(root, abs) {
		return "", "", errEscapes
	}
	rel, err = filepath.Rel(root, abs)
	if err != nil {
		return "", "", errEscapes
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return abs, ".", nil
	}
	cur := root
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		st, lerr := os.Lstat(cur)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				break
			}
			return "", "", lerr
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", "", errSymlink
		}
	}
	return abs, rel, nil
}

func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return !filepath.IsAbs(rel)
}

type readResult struct {
	Path      string `json:"path"`
	Encoding  string `json:"encoding"`
	Content   string `json:"content"`
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

func (s *Server) readFile(path string, offset int64, maxBytes int64) (readResult, error) {
	full, rel, err := s.resolvePath(path)
	if err != nil {
		return readResult{}, err
	}
	if rel == "." {
		return readResult{}, errNotFile
	}
	st, err := os.Lstat(full)
	if err != nil {
		return readResult{}, err
	}
	if !st.Mode().IsRegular() {
		return readResult{}, errNotFile
	}
	if maxBytes <= 0 {
		maxBytes = s.cfg.MaxRead
	}
	f, err := os.Open(full)
	if err != nil {
		return readResult{}, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return readResult{}, err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return readResult{}, err
	}
	truncated := int64(len(b)) > maxBytes
	if truncated {
		b = b[:maxBytes]
	}
	res := readResult{Path: rel, Bytes: int64(len(b)), Truncated: truncated}
	if utf8.Valid(b) && !strings.ContainsRune(string(b), 0) {
		res.Encoding = "utf-8"
		res.Content = string(b)
		return res, nil
	}
	res.Encoding = "base64"
	res.Content = base64Encode(b)
	return res, nil
}

type writeResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

func (s *Server) writeFile(path, content string, createDirs bool) (writeResult, error) {
	full, rel, err := s.resolvePath(path)
	if err != nil {
		return writeResult{}, err
	}
	if rel == "." {
		return writeResult{}, errors.New("cannot write the workspace root")
	}
	data := []byte(content)
	if int64(len(data)) > s.cfg.MaxWrite {
		return writeResult{}, fmt.Errorf("content exceeds %d bytes", s.cfg.MaxWrite)
	}
	if err := s.checkDisk(); err != nil {
		return writeResult{}, err
	}
	parent := filepath.Dir(full)
	if createDirs {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return writeResult{}, err
		}
	} else if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return writeResult{}, errors.New("parent directory does not exist")
	}
	// Re-check after creating parents: a directory created here must not be a symlink.
	if _, _, err := s.resolvePath(rel); err != nil {
		return writeResult{}, err
	}
	mode := os.FileMode(0600)
	if st, err := os.Stat(full); err == nil {
		if !st.Mode().IsRegular() {
			return writeResult{}, errors.New("only regular files can be overwritten")
		}
		mode = st.Mode().Perm()
	}
	f, err := os.CreateTemp(parent, ".sandbox-*.tmp")
	if err != nil {
		return writeResult{}, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return writeResult{}, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return writeResult{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return writeResult{}, err
	}
	if err := f.Close(); err != nil {
		return writeResult{}, err
	}
	if err := os.Rename(tmp, full); err != nil {
		return writeResult{}, err
	}
	return writeResult{Path: rel, Bytes: int64(len(data))}, nil
}

type deleteResult struct {
	Path    string `json:"path"`
	Removed int    `json:"removed"`
}

func (s *Server) deletePath(path string, recursive bool) (deleteResult, error) {
	full, rel, err := s.resolvePath(path)
	if err != nil {
		return deleteResult{}, err
	}
	if rel == "." {
		return deleteResult{}, errors.New("refusing to delete the workspace root; use end_work")
	}
	st, err := os.Lstat(full)
	if err != nil {
		return deleteResult{}, err
	}
	if st.IsDir() {
		if !recursive {
			entries, err := os.ReadDir(full)
			if err != nil {
				return deleteResult{}, err
			}
			if len(entries) > 0 {
				return deleteResult{}, errors.New("directory is not empty; pass recursive=true")
			}
		}
		count := 0
		filepath.WalkDir(full, func(_ string, d os.DirEntry, err error) error {
			if err == nil && d != nil {
				count++
			}
			return nil
		})
		if err := os.RemoveAll(full); err != nil {
			return deleteResult{}, err
		}
		return deleteResult{Path: rel, Removed: count}, nil
	}
	if err := os.Remove(full); err != nil {
		return deleteResult{}, err
	}
	return deleteResult{Path: rel, Removed: 1}, nil
}

type fileInfo struct {
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	MimeType string `json:"mime_type"`
	Modified string `json:"modified"`
}

func (s *Server) statFile(path string) (fileInfo, error) {
	full, rel, err := s.resolvePath(path)
	if err != nil {
		return fileInfo{}, err
	}
	if rel == "." {
		return fileInfo{}, errNotFile
	}
	st, err := os.Lstat(full)
	if err != nil {
		return fileInfo{}, err
	}
	if !st.Mode().IsRegular() {
		return fileInfo{}, errNotFile
	}
	return fileInfo{
		Path:     rel,
		Bytes:    st.Size(),
		MimeType: detectMime(full),
		Modified: st.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
	}, nil
}

func detectMime(path string) string {
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		return detectContentType(buf[:n])
	}
	return "application/octet-stream"
}
