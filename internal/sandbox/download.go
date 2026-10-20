package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type downloadPayload struct {
	Path string `json:"p"`
	Exp  int64  `json:"e"`
	Nonce string `json:"n"`
}

// downloadSecret derives the signing key from the sandbox token.
func (s *Server) downloadSecret() []byte {
	sum := sha256.Sum256([]byte("download:" + s.cfg.Token))
	return sum[:]
}

func (s *Server) signDownload(rel string, expiry time.Time) string {
	payload := downloadPayload{Path: rel, Exp: expiry.Unix(), Nonce: randomHex(8)}
	raw, _ := json.Marshal(payload)
	enc := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.downloadSecret())
	mac.Write([]byte(enc))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return s.cfg.PublicURL + "/f/" + enc + "." + sig
}

// verifyDownload validates a capability link and returns the absolute path.
func (s *Server) verifyDownload(token string) (string, string, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", errors.New("malformed link")
	}
	mac := hmac.New(sha256.New, s.downloadSecret())
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return "", "", errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", errors.New("malformed link")
	}
	var p downloadPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", errors.New("malformed link")
	}
	if time.Now().UTC().Unix() > p.Exp {
		return "", "", errors.New("link expired")
	}
	full, rel, err := s.resolvePath(p.Path)
	if err != nil {
		return "", "", err
	}
	return full, rel, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}
