// Package lfs implements the Git LFS server side of the hub: the batch API
// with the "basic" transfer adapter, object verification and a minimal
// locking API. Objects are stored in the bare repository's own lfs/objects
// directory, the layout git-lfs itself uses, so `git lfs push` can upload
// them from the hub to mirrors unchanged.
package lfs

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxPointerSize bounds the size of a blob that can be an LFS pointer.
const MaxPointerSize = 1024

var oidRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidOID reports whether oid is a lowercase hex SHA-256.
func ValidOID(oid string) bool { return oidRe.MatchString(oid) }

// Pointer is a parsed LFS pointer file.
type Pointer struct {
	OID  string
	Size int64
}

// ParsePointer parses an LFS pointer file. It returns false for any other
// content.
func ParsePointer(b []byte) (Pointer, bool) {
	if len(b) > MaxPointerSize || !bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/v1\n")) {
		return Pointer{}, false
	}
	var p Pointer
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		switch k {
		case "oid":
			p.OID = strings.TrimPrefix(v, "sha256:")
		case "size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return Pointer{}, false
			}
			p.Size = n
		}
	}
	if !ValidOID(p.OID) {
		return Pointer{}, false
	}
	return p, true
}

// ObjectPath is where an object lives inside a bare repository.
func ObjectPath(repoDir, oid string) string {
	return filepath.Join(repoDir, "lfs", "objects", oid[0:2], oid[2:4], oid)
}

// Has reports whether the repository stores oid with the given size
// (size < 0 skips the size check).
func Has(repoDir, oid string, size int64) bool {
	if !ValidOID(oid) {
		return false
	}
	fi, err := os.Stat(ObjectPath(repoDir, oid))
	return err == nil && fi.Mode().IsRegular() && (size < 0 || fi.Size() == size)
}

// HasObjects reports whether the repository holds any LFS object at all.
func HasObjects(repoDir string) bool {
	entries, err := os.ReadDir(filepath.Join(repoDir, "lfs", "objects"))
	return err == nil && len(entries) > 0
}

// ---------------------------------------------------------------- tokens

// Signer issues short-lived tokens that authorize one transfer action, so
// the client does not have to resend its credentials to object URLs.
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner derives a signing key from the server's master key.
func NewSigner(masterKey []byte) *Signer {
	m := hmac.New(sha256.New, masterKey)
	m.Write([]byte("gitrepo lfs action token v1"))
	return &Signer{key: m.Sum(nil), now: time.Now}
}

type claims struct {
	Repo string `json:"r"`
	OID  string `json:"o"`
	Op   string `json:"p"`
	Exp  int64  `json:"e"`
}

const tokenPrefix = "lfs1."

// AnyObject in a token's object field authorizes batch requests for the
// whole repository (issued by git-lfs-authenticate over SSH). Such a token
// is never valid for an individual object transfer.
const AnyObject = "*"

// Sign returns a token for op ("upload" or "download") on repo/oid.
func (s *Signer) Sign(repo, oid, op string, ttl time.Duration) string {
	c, _ := json.Marshal(claims{Repo: repo, OID: oid, Op: op, Exp: s.now().Add(ttl).Unix()})
	body := base64.RawURLEncoding.EncodeToString(c)
	return tokenPrefix + body + "." + s.mac(body)
}

func (s *Signer) mac(body string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// IsToken reports whether tok looks like an action token.
func IsToken(tok string) bool { return strings.HasPrefix(tok, tokenPrefix) }

var errBadToken = errors.New("invalid or expired transfer token")

// Verify checks a token for repo/oid/op. An upload token also authorizes
// downloading and verifying the same object.
func (s *Signer) Verify(tok, repo, oid, op string) error {
	body, sig, ok := strings.Cut(strings.TrimPrefix(tok, tokenPrefix), ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac(body))) {
		return errBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return errBadToken
	}
	var c claims
	if json.Unmarshal(raw, &c) != nil || c.Repo != repo || c.OID != oid || s.now().Unix() > c.Exp {
		return errBadToken
	}
	if c.Op != op && c.Op != "upload" {
		return errBadToken
	}
	return nil
}

// hashHex is a helper for tests and uploads.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
