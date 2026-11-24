package gitexec

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// CatFile reads objects through one long-running `git cat-file --batch`,
// so that walking a history costs one process, not one per object.
type CatFile struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	in    *bufio.Writer
	out   *bufio.Reader
}

// OpenCatFile starts the reader; Close must be called.
func (g *Git) OpenCatFile(ctx context.Context, repo string) (*CatFile, error) {
	cmd := g.command(ctx, repo, nil, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &CatFile{cmd: cmd, stdin: stdin, in: bufio.NewWriter(stdin), out: bufio.NewReaderSize(stdout, 64<<10)}, nil
}

// Get returns the object rev names (a SHA, or e.g. "<commit>:<path>").
// ok is false when it does not exist.
func (c *CatFile) Get(rev string) (sha, typ string, data []byte, ok bool, err error) {
	if strings.ContainsAny(rev, "\n") {
		return "", "", nil, false, errors.New("invalid object name")
	}
	if _, err = c.in.WriteString(rev + "\n"); err != nil {
		return
	}
	if err = c.in.Flush(); err != nil {
		return
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return "", "", nil, false, fmt.Errorf("cat-file: %w", err)
	}
	f := strings.Fields(header)
	if len(f) == 2 && (f[1] == "missing" || f[1] == "ambiguous") {
		return "", "", nil, false, nil
	}
	if len(f) != 3 {
		return "", "", nil, false, fmt.Errorf("cat-file: unexpected header %q", header)
	}
	size, err := strconv.Atoi(f[2])
	if err != nil {
		return "", "", nil, false, fmt.Errorf("cat-file: unexpected header %q", header)
	}
	data = make([]byte, size+1) // contents and a newline
	if _, err = io.ReadFull(c.out, data); err != nil {
		return "", "", nil, false, fmt.Errorf("cat-file: %w", err)
	}
	return f[0], f[1], data[:size], true, nil
}

// Close stops the reader.
func (c *CatFile) Close() error {
	c.stdin.Close()
	return c.cmd.Wait()
}

// ObjectFormat is the repository's hash ("sha1" or "sha256").
func (g *Git) ObjectFormat(ctx context.Context, repo string) (string, error) {
	out, err := g.Run(ctx, repo, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// HashObject computes the ID git gives an object of type typ with content
// in a repository using format, without writing it.
func HashObject(format, typ string, content []byte) string {
	header := typ + " " + strconv.Itoa(len(content)) + "\x00"
	if format == "sha256" {
		h := sha256.New()
		h.Write([]byte(header))
		h.Write(content)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New()
	h.Write([]byte(header))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// WriteObjects stores objects of type typ in one `git hash-object` run and
// returns their IDs, in order.
func (g *Git) WriteObjects(ctx context.Context, repo, typ string, contents [][]byte) ([]string, error) {
	if len(contents) == 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp(g.TmpDir, "objects-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var paths strings.Builder
	for i, c := range contents {
		p := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(p, c, 0o600); err != nil {
			return nil, err
		}
		paths.WriteString(p + "\n")
	}
	out, err := g.RunInput(ctx, repo, []byte(paths.String()), "hash-object", "-t", typ, "-w", "--stdin-paths")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if len(ids) != len(contents) {
		return nil, fmt.Errorf("hash-object returned %d IDs for %d objects", len(ids), len(contents))
	}
	return ids, nil
}
