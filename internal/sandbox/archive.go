package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type archiveResponse struct {
	OK     bool   `json:"ok"`
	Remote string `json:"remote"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Error  string `json:"error"`
}

// archive streams the workspace to the pool (or to a local directory).
func (s *Server) archive(reason string) (*ArchiveInfo, error) {
	switch s.cfg.ArchiveMode {
	case "off":
		return &ArchiveInfo{At: nowUTC(), Reason: reason}, nil
	case "local":
		return s.archiveLocal(reason)
	default:
		return s.archivePush(reason)
	}
}

func (s *Server) archiveLocal(reason string) (*ArchiveInfo, error) {
	name := fmt.Sprintf("%s_%s.tar.gz", nowUTC().Format("20060102T150405Z"), reason)
	path := filepath.Join(s.cfg.LocalArchiveDir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	if err := writeTarGz(s.cfg.Root, mw); err != nil {
		os.Remove(path)
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return &ArchiveInfo{
		At:     nowUTC(),
		Reason: reason,
		Remote: path,
		Bytes:  st.Size(),
		SHA256: hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

func (s *Server) archivePush(reason string) (*ArchiveInfo, error) {
	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		err := writeTarGz(s.cfg.Root, gz)
		if err == nil {
			err = gz.Close()
		} else {
			gz.Close()
		}
		pw.CloseWithError(err)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ArchiveTimeout)
	defer cancel()
	endpoint := strings.TrimRight(s.cfg.ArchiveURL, "/") + "/v1/archive"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+s.cfg.ArchiveToken)
	req.Header.Set("X-Sandbox-Id", s.cfg.SandboxID)
	req.Header.Set("X-Owner-Id", s.cfg.OwnerID)
	req.Header.Set("X-Reason", reason)
	req.Header.Set("X-Filename", fmt.Sprintf("%s_%s.tar.gz", nowUTC().Format("20060102T150405Z"), reason))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		return nil, fmt.Errorf("push archive: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		pr.CloseWithError(errors.New("archive rejected"))
		return nil, fmt.Errorf("pool rejected the archive: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out archiveResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("bad archive response: %w", err)
	}
	if !out.OK {
		return nil, fmt.Errorf("pool archive failed: %s", out.Error)
	}
	return &ArchiveInfo{
		At:     nowUTC(),
		Reason: reason,
		Remote: out.Remote,
		Bytes:  out.Bytes,
		SHA256: out.SHA256,
	}, nil
}

// writeTarGz writes the workspace content as a gzipped tar stream.
func writeTarGz(root string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // the file vanished while we walked: skip it
			}
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				return nil
			}
			return ierr
		}
		name := "workspace/" + filepath.ToSlash(rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			hdr := &tar.Header{
				Name:     name,
				Mode:     0777,
				Typeflag: tar.TypeSymlink,
				Linkname: target,
				ModTime:  info.ModTime(),
			}
			return tw.WriteHeader(hdr)
		case info.IsDir():
			hdr := &tar.Header{
				Name:     name + "/",
				Typeflag: tar.TypeDir,
				Mode:     int64(info.Mode().Perm()),
				ModTime:  info.ModTime(),
			}
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			hdr := &tar.Header{
				Name:     name,
				Typeflag: tar.TypeReg,
				Mode:     int64(info.Mode().Perm()),
				Size:     info.Size(),
				ModTime:  info.ModTime(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, oerr := os.Open(path)
			if oerr != nil {
				return oerr
			}
			_, cerr := io.CopyN(tw, f, info.Size())
			f.Close()
			if cerr != nil {
				if errors.Is(cerr, io.EOF) {
					return fmt.Errorf("file changed while archiving: %s", rel)
				}
				return cerr
			}
			return nil
		default:
			return nil // sockets, devices and fifos are not worth archiving
		}
	})
	if err != nil {
		tw.Close()
		gz.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}
