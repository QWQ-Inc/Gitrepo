// Package gitexec runs the system git binary. All transport goes through the
// real git so protocol v2, force-with-lease, atomic pushes and future LFS
// support behave exactly like upstream.
//
// Credentials never appear on the command line or in URLs: HTTPS remotes get
// them through GIT_ASKPASS (a Gitrepo shim that asks the server over a unix
// socket using a one-time ticket), SSH remotes through a temporary key file
// referenced by GIT_SSH_COMMAND.
package gitexec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qwq-inc/gitrepo/internal/lfs"
)

// ZeroSHA is git's all-zero object name used for creations and deletions.
const ZeroSHA = "0000000000000000000000000000000000000000"

// IsZero reports whether sha is empty or the all-zero object name (SHA-1 or
// SHA-256 length).
func IsZero(sha string) bool {
	return sha == "" || strings.Trim(sha, "0") == ""
}

// Cred is a resolved credential for one remote operation.
type Cred struct {
	Kind       string // token | basic | ssh | none
	Username   string
	Secret     string // password/token, or private key for ssh
	KnownHosts string // optional known_hosts content for ssh
}

// Git runs git subprocesses.
type Git struct {
	Bin string
	// Env is appended to the inherited environment of every git process.
	Env []string
	// TmpDir holds short-lived files (ssh keys).
	TmpDir string
	// KnownHostsFile is the shared known_hosts used with accept-new.
	KnownHostsFile string
	// AskpassPath is an executable that implements GIT_ASKPASS via the
	// server's internal socket.
	AskpassPath string
	// SocketPath is the server's internal unix socket.
	SocketPath string
	// Tickets hands out askpass tickets.
	Tickets *Tickets
}

// baseConfig neutralises user/system settings that would change push
// semantics or leak credentials.
var baseConfig = []string{
	"-c", "credential.helper=",
	"-c", "push.negotiate=false",
	"-c", "push.followTags=false",
	"-c", "push.gpgSign=false",
	"-c", "push.recurseSubmodules=no",
	"-c", "core.askPass=",
	"-c", "gc.auto=0",
}

// Error is returned when git exits non-zero.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	return fmt.Sprintf("git %s: exit %d: %s", firstArg(e.Args), e.ExitCode, msg)
}

func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			return a
		}
	}
	return strings.Join(args, " ")
}

// Run executes git in dir and returns stdout.
func (g *Git) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	out, _, err := g.run(ctx, dir, nil, nil, args...)
	return out, err
}

// RunInput executes git with stdin.
func (g *Git) RunInput(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	out, _, err := g.run(ctx, dir, stdin, nil, args...)
	return out, err
}

// run executes git. A non-empty dir must be a bare repository; it is passed
// explicitly with --git-dir so a damaged repository can never make git
// discover and operate on an enclosing one.
// command builds a git command with the standard configuration overrides
// and environment.
func (g *Git) command(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	full := append([]string{}, baseConfig...)
	if dir != "" {
		full = append(full, "--git-dir="+dir)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, g.Bin, full...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), g.Env...), extraEnv...)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (g *Git) run(ctx context.Context, dir string, stdin []byte, extraEnv []string, args ...string) ([]byte, []byte, error) {
	cmd := g.command(ctx, dir, extraEnv, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		msg := stderr.String()
		if ctx.Err() != nil {
			msg = ctx.Err().Error() + ": " + msg
		}
		return stdout.Bytes(), stderr.Bytes(), &Error{Args: args, ExitCode: code, Stderr: msg}
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

// runRemote executes a git command that talks to a remote, injecting cred.
// remoteURL is the URL the command is meant to talk to; credentials are
// only released for prompts naming that URL's origin.
func (g *Git) runRemote(ctx context.Context, dir string, cred *Cred, remoteURL string, args ...string) ([]byte, []byte, error) {
	return g.runRemoteInput(ctx, dir, cred, remoteURL, nil, args...)
}

func (g *Git) runRemoteInput(ctx context.Context, dir string, cred *Cred, remoteURL string, stdin []byte, args ...string) ([]byte, []byte, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0", "SSH_ASKPASS=", "GIT_SSH_VARIANT=ssh"}
	var cleanup []func()
	defer func() {
		for _, f := range cleanup {
			f()
		}
	}()
	if cred != nil {
		switch cred.Kind {
		case "token", "basic":
			if g.Tickets == nil || g.AskpassPath == "" {
				return nil, nil, errors.New("askpass is not configured")
			}
			ticket, release := g.Tickets.Issue(*cred, Origin(remoteURL))
			cleanup = append(cleanup, release)
			env = append(env,
				"GIT_ASKPASS="+g.AskpassPath,
				"GITREPO_SOCKET="+g.SocketPath,
				"GITREPO_ASKPASS_TICKET="+ticket,
			)
		case "ssh":
			sshCmd, rm, err := g.sshCommand(cred)
			if err != nil {
				return nil, nil, err
			}
			cleanup = append(cleanup, rm)
			env = append(env, "GIT_SSH_COMMAND="+sshCmd)
		}
	}
	return g.run(ctx, dir, stdin, env, args...)
}

func (g *Git) sshCommand(cred *Cred) (string, func(), error) {
	if err := os.MkdirAll(g.TmpDir, 0o700); err != nil {
		return "", nil, err
	}
	key, err := os.CreateTemp(g.TmpDir, "sshkey-*")
	if err != nil {
		return "", nil, err
	}
	files := []string{key.Name()}
	rm := func() {
		for _, f := range files {
			os.Remove(f)
		}
	}
	secret := cred.Secret
	if !strings.HasSuffix(secret, "\n") {
		secret += "\n"
	}
	if err := key.Chmod(0o600); err != nil {
		key.Close()
		rm()
		return "", nil, err
	}
	if _, err := key.WriteString(secret); err != nil {
		key.Close()
		rm()
		return "", nil, err
	}
	key.Close()
	knownHosts := g.KnownHostsFile
	strict := "accept-new"
	if cred.KnownHosts != "" {
		kh, err := os.CreateTemp(g.TmpDir, "known_hosts-*")
		if err != nil {
			rm()
			return "", nil, err
		}
		files = append(files, kh.Name())
		kh.WriteString(cred.KnownHosts + "\n")
		kh.Close()
		knownHosts = kh.Name()
		strict = "yes"
	}
	cmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=%s -o UserKnownHostsFile=%s",
		shellQuote(key.Name()), strict, shellQuote(knownHosts))
	return cmd, rm, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ------------------------------------------------------------- repo ops

// InitBare creates a bare repository at path.
func (g *Git) InitBare(ctx context.Context, path, initialBranch string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := g.Run(ctx, "", "init", "--quiet", "--bare", "--initial-branch="+initialBranch, path)
	return err
}

// ForEachRef lists refs under the given prefixes as ref → object name.
func (g *Git) ForEachRef(ctx context.Context, repo string, prefixes ...string) (map[string]string, error) {
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, prefixes...)
	out, err := g.Run(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		sha, ref, ok := strings.Cut(sc.Text(), " ")
		if ok {
			refs[ref] = sha
		}
	}
	return refs, sc.Err()
}

// HubRefs returns the branches and tags of a hub repository.
func (g *Git) HubRefs(ctx context.Context, repo string) (map[string]string, error) {
	return g.ForEachRef(ctx, repo, "refs/heads/", "refs/tags/")
}

// HasObject reports whether the repository contains object sha.
func (g *Git) HasObject(ctx context.Context, repo, sha string) bool {
	if IsZero(sha) {
		return false
	}
	_, err := g.Run(ctx, repo, "cat-file", "-e", sha)
	return err == nil
}

// IsAncestor reports whether anc is an ancestor of (or equal to) desc. Both
// may be tags; they are peeled to commits. Missing objects yield false.
func (g *Git) IsAncestor(ctx context.Context, repo, anc, desc string) bool {
	if IsZero(anc) || IsZero(desc) {
		return false
	}
	if anc == desc {
		return true
	}
	if !g.HasObject(ctx, repo, anc) || !g.HasObject(ctx, repo, desc) {
		return false
	}
	_, err := g.Run(ctx, repo, "merge-base", "--is-ancestor", anc+"^{commit}", desc+"^{commit}")
	return err == nil
}

// Referenced reports whether some ref of the repository (hidden ones
// included) contains the commit sha, i.e. whether it is safe from gc.
func (g *Git) Referenced(ctx context.Context, repo, sha string) bool {
	out, err := g.Run(ctx, repo, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", sha+"^{commit}")
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

// UpdateRefs applies "update <ref> <sha>" / "delete <ref>" lines atomically.
func (g *Git) UpdateRefs(ctx context.Context, repo string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	_, err := g.RunInput(ctx, repo, []byte(strings.Join(lines, "\n")+"\n"), "update-ref", "--stdin")
	return err
}

// SymbolicRef reads HEAD's target.
func (g *Git) SymbolicRef(ctx context.Context, repo string) (string, error) {
	out, err := g.Run(ctx, repo, "symbolic-ref", "HEAD")
	return strings.TrimSpace(string(out)), err
}

// SetHead points HEAD at ref.
func (g *Git) SetHead(ctx context.Context, repo, ref string) error {
	_, err := g.Run(ctx, repo, "symbolic-ref", "HEAD", ref)
	return err
}

// Version returns git's version as (major, minor).
func (g *Git) Version(ctx context.Context) (int, int, error) {
	out, err := g.Run(ctx, "", "version")
	if err != nil {
		return 0, 0, err
	}
	// "git version 2.43.0" (possibly with vendor suffixes)
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return 0, 0, fmt.Errorf("unexpected git version output %q", out)
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected git version %q", fields[2])
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("unexpected git version %q", fields[2])
	}
	return major, minor, nil
}

// ------------------------------------------------------------ remote ops

// LsRemote returns the branches and tags advertised by url.
func (g *Git) LsRemote(ctx context.Context, url string, cred *Cred) (map[string]string, error) {
	out, _, err := g.runRemote(ctx, "", cred, url, "ls-remote", "--refs", url)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		sha, ref, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
			refs[ref] = sha
		}
	}
	return refs, sc.Err()
}

// Fetch fetches refspecs from url into repo.
func (g *Git) Fetch(ctx context.Context, repo, url string, cred *Cred, refspecs ...string) error {
	args := append([]string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", url}, refspecs...)
	_, _, err := g.runRemote(ctx, repo, cred, url, args...)
	return err
}

// MirrorFetch makes repo's branches and tags exactly those of url (an edge
// node following its hub): new and moved refs are fetched, removed ones
// pruned.
func (g *Git) MirrorFetch(ctx context.Context, repo, url string, cred *Cred) error {
	_, _, err := g.runRemote(ctx, repo, cred, url, "fetch", "--quiet", "--prune", "--no-tags", "--no-write-fetch-head", "--force", url,
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	return err
}

// LFSObjects returns the Git LFS objects (oid → size) referenced by blobs
// reachable from tips but not from exclude. Exclude values the repository
// does not have are ignored. Everything is streamed: only candidate pointer
// blob ids (≤ 1 KiB blobs) are held in memory.
func (g *Git) LFSObjects(ctx context.Context, repo string, tips, exclude []string) (map[string]int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rev := g.command(ctx, repo, nil, "rev-list", "--objects", "--ignore-missing", "--stdin")
	var in strings.Builder
	for _, t := range tips {
		in.WriteString(t + "\n")
	}
	for _, x := range exclude {
		in.WriteString("^" + x + "\n")
	}
	rev.Stdin = strings.NewReader(in.String())
	var revErr bytes.Buffer
	rev.Stderr = &revErr
	// %(rest) makes cat-file split "sha path" lines at the first space;
	// without it the whole line would be taken as the object name.
	check := g.command(ctx, repo, nil, "cat-file", "--batch-check=%(objecttype) %(objectsize) %(objectname) %(rest)")
	pipe, err := rev.StdoutPipe()
	if err != nil {
		return nil, err
	}
	check.Stdin = pipe
	checkOut, err := check.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := rev.Start(); err != nil {
		return nil, err
	}
	if err := check.Start(); err != nil {
		cancel()
		rev.Wait()
		return nil, err
	}
	var candidates []string
	br := bufio.NewReaderSize(checkOut, 64<<10)
	var readErr error
	for {
		line, err := br.ReadString('\n')
		if f := strings.SplitN(strings.TrimSuffix(line, "\n"), " ", 4); len(f) >= 3 && f[0] == "blob" {
			if size, _ := strconv.Atoi(f[1]); size >= 100 && size <= lfs.MaxPointerSize {
				candidates = append(candidates, f[2])
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	if readErr != nil {
		cancel()
	}
	revWaitErr := rev.Wait()
	checkWaitErr := check.Wait()
	switch {
	case readErr != nil:
		return nil, readErr
	case revWaitErr != nil:
		return nil, fmt.Errorf("rev-list: %v: %s", revWaitErr, strings.TrimSpace(revErr.String()))
	case checkWaitErr != nil:
		return nil, fmt.Errorf("cat-file: %v", checkWaitErr)
	}
	out := map[string]int64{}
	if len(candidates) == 0 {
		return out, nil
	}
	err = g.readBlobs(ctx, repo, candidates, func(_ string, body []byte) {
		if p, ok := lfs.ParsePointer(body); ok {
			out[p.OID] = p.Size
		}
	})
	return out, err
}

// readBlobs streams the contents of the given blobs through fn.
func (g *Git) readBlobs(ctx context.Context, repo string, ids []string, fn func(id string, body []byte)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := g.command(ctx, repo, nil, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		w := bufio.NewWriter(stdin)
		for _, id := range ids {
			w.WriteString(id + "\n")
		}
		w.Flush()
		stdin.Close()
	}()
	r := bufio.NewReaderSize(stdout, 64<<10)
	var readErr error
	for {
		header, err := r.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[1] != "blob" {
			continue // "<id> missing" or a non-blob
		}
		size, _ := strconv.Atoi(f[2])
		if size > lfs.MaxPointerSize {
			if _, err := io.CopyN(io.Discard, r, int64(size)+1); err != nil {
				readErr = err
				break
			}
			continue
		}
		body := make([]byte, size+1)
		if _, err := io.ReadFull(r, body); err != nil {
			readErr = err
			break
		}
		fn(f[0], body[:size])
	}
	if readErr != nil {
		cancel()
		cmd.Wait()
		return readErr
	}
	return cmd.Wait()
}

// LFSAvailable reports whether git-lfs is installed.
func (g *Git) LFSAvailable(ctx context.Context) bool {
	_, err := g.Run(ctx, "", "lfs", "version")
	return err == nil
}

// RemoteHead returns the branch a remote's HEAD points to ("" if unknown).
func (g *Git) RemoteHead(ctx context.Context, url string, cred *Cred) string {
	out, _, err := g.runRemote(ctx, "", cred, url, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: "); ok {
			ref, _, _ = strings.Cut(ref, "\t")
			return ref
		}
	}
	return ""
}

// ImportNamespace receives fetched refs during an import before they are
// published; it is hidden from clients like refs/backup.
const ImportNamespace = "refs/gitrepo/import/"

// FetchForImport copies every branch and tag of url into
// refs/gitrepo/import/{heads,tags}/ of repo (cleared first), verifying the
// received objects, and returns the fetched values keyed by their real
// names (refs/heads/…, refs/tags/…). Nothing visible to clients changes.
func (g *Git) FetchForImport(ctx context.Context, repo, url string, cred *Cred) (map[string]string, error) {
	if err := g.ClearImport(ctx, repo); err != nil {
		return nil, err
	}
	_, _, err := g.runRemote(ctx, repo, cred, url, "-c", "fetch.fsckObjects=true",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", url,
		"+refs/heads/*:"+ImportNamespace+"heads/*", "+refs/tags/*:"+ImportNamespace+"tags/*")
	if err != nil {
		return nil, err
	}
	raw, err := g.ForEachRef(ctx, repo, ImportNamespace)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for ref, sha := range raw {
		out["refs/"+strings.TrimPrefix(ref, ImportNamespace)] = sha
	}
	return out, nil
}

// ClearImport removes leftovers of an import.
func (g *Git) ClearImport(ctx context.Context, repo string) error {
	refs, err := g.ForEachRef(ctx, repo, ImportNamespace)
	if err != nil {
		return err
	}
	var lines []string
	for ref := range refs {
		lines = append(lines, "delete "+ref)
	}
	return g.UpdateRefs(ctx, repo, lines)
}

// lfsScratch creates a throwaway bare repository for running git-lfs on
// behalf of repo. It borrows repo's objects through alternates and has an
// unborn HEAD, so git-lfs cannot read a .lfsconfig (git-lfs honours lfs.url
// and lfs.pushurl from the HEAD commit of a bare repository): content
// pushed to the hub must never decide where LFS traffic, and the mirror
// credentials that come with it, are sent. refs (name → value) are created
// in it for commands that scan history.
func (g *Git) lfsScratch(ctx context.Context, repo string, refs map[string]string) (string, func(), error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(g.TmpDir, "lfs-*.git")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if _, err := g.Run(ctx, "", "init", "--quiet", "--bare", dir); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(filepath.Join(abs, "objects")+"\n"), 0o644); err != nil {
		cleanup()
		return "", nil, err
	}
	if _, err := g.Run(ctx, dir, "symbolic-ref", "HEAD", "refs/gitrepo/unborn"); err != nil {
		cleanup()
		return "", nil, err
	}
	if len(refs) > 0 {
		lines := make([]string, 0, len(refs))
		for ref, sha := range refs {
			lines = append(lines, "update "+ref+" "+sha)
		}
		if err := g.UpdateRefs(ctx, dir, lines); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return dir, cleanup, nil
}

// PublishedView returns a throwaway bare repository that shares repo's
// objects but contains only its branches and tags. Commands run there can
// neither resolve nor print hidden refs (refs/backup/*), e.g. git archive
// expanding %D through export-subst.
func (g *Git) PublishedView(ctx context.Context, repo string) (string, func(), error) {
	refs, err := g.HubRefs(ctx, repo)
	if err != nil {
		return "", nil, err
	}
	return g.lfsScratch(ctx, repo, refs)
}

// lfsArgs are the git-lfs settings for working on repo's LFS store with url
// as the only remote. lfs.url / lfs.pushurl are pinned to the endpoint
// git-lfs would derive for url, as a second line of defence.
func lfsArgs(repo, url string) ([]string, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-c", "lfs.storage=" + filepath.Join(abs, "lfs"),
		"-c", "remote.gitrepo-mirror.url=" + url,
		"-c", "lfs.locksverify=false",
	}
	if ep := lfsEndpoint(url); ep != "" {
		args = append(args, "-c", "lfs.url="+ep, "-c", "lfs.pushurl="+ep)
	}
	return args, nil
}

// lfsEndpoint derives the LFS API URL git-lfs uses for an HTTP(S) remote
// ("" for other transports, which git-lfs resolves over SSH).
func lfsEndpoint(remote string) string {
	if !strings.HasPrefix(remote, "http://") && !strings.HasPrefix(remote, "https://") {
		return ""
	}
	remote = strings.TrimRight(remote, "/")
	if strings.HasSuffix(remote, ".git") {
		return remote + "/info/lfs"
	}
	return remote + ".git/info/lfs"
}

// LFSFetchAll downloads every LFS object referenced from refs (name →
// value; objects must be in repo) from the LFS endpoint of url into repo's
// LFS store.
func (g *Git) LFSFetchAll(ctx context.Context, repo, url string, cred *Cred, refs map[string]string) error {
	scratch, cleanup, err := g.lfsScratch(ctx, repo, refs)
	if err != nil {
		return err
	}
	defer cleanup()
	args, err := lfsArgs(repo, url)
	if err != nil {
		return err
	}
	_, _, err = g.runRemote(ctx, scratch, cred, url, append(args, "lfs", "fetch", "--all", "gitrepo-mirror")...)
	return err
}

// LFSPush uploads the given LFS objects from repo's LFS store to the LFS
// endpoint of url.
func (g *Git) LFSPush(ctx context.Context, repo, url string, cred *Cred, oids []string) error {
	if len(oids) == 0 {
		return nil
	}
	scratch, cleanup, err := g.lfsScratch(ctx, repo, nil)
	if err != nil {
		return err
	}
	defer cleanup()
	args, err := lfsArgs(repo, url)
	if err != nil {
		return err
	}
	_, _, err = g.runRemoteInput(ctx, scratch, cred, url, []byte(strings.Join(oids, "\n")+"\n"),
		append(args, "lfs", "push", "--object-id", "gitrepo-mirror", "--stdin")...)
	return err
}

// PushOp is one ref update. New == "" deletes the ref. Expect is the value
// the remote ref must currently have ("" = must not exist); it is passed as
// --force-with-lease so a concurrent foreign change is never overwritten.
type PushOp struct {
	Ref    string
	New    string
	Expect string
}

// RefStatus is the per-ref outcome reported by `git push --porcelain`.
type RefStatus struct {
	Ref     string
	Flag    byte // ' ' '+' '-' '*' '=' '!'
	Summary string
	Reason  string
}

// OK reports whether the ref was updated or already up to date.
func (s RefStatus) OK() bool { return s.Flag != '!' }

// StaleLease reports a force-with-lease mismatch.
func (s RefStatus) StaleLease() bool { return s.Flag == '!' && s.Reason == "stale info" }

// AtomicCollateral reports a ref rejected only because another ref in the
// same atomic push failed. The client says "atomic push failed" when it
// aborts before sending; receive-pack says "atomic push failure" when the
// server rejected a sibling ref (e.g. an update hook enforcing ACLs).
func (s RefStatus) AtomicCollateral() bool {
	return s.Flag == '!' && (strings.Contains(s.Reason, "atomic push failed") || strings.Contains(s.Reason, "atomic push failure"))
}

// RemoteRejected reports a rejection decided by the server's hooks or rules.
func (s RefStatus) RemoteRejected() bool {
	return s.Flag == '!' && s.Summary == "[remote rejected]" && !s.AtomicCollateral()
}

// ErrAtomicUnsupported means the remote does not advertise atomic pushes.
var ErrAtomicUnsupported = errors.New("remote does not support atomic push")

// PushResult holds per-ref statuses plus raw output for diagnostics.
type PushResult struct {
	Refs   map[string]RefStatus
	Stderr string
}

// Push sends ops from repo to url. A non-nil error with an empty Refs map
// means the push failed as a whole (network, auth, …); per-ref rejections
// are reported in Refs with a nil error.
func (g *Git) Push(ctx context.Context, repo, url string, cred *Cred, ops []PushOp, atomic bool) (*PushResult, error) {
	args := []string{"push", "--porcelain"}
	if atomic {
		args = append(args, "--atomic")
	}
	for _, op := range ops {
		args = append(args, "--force-with-lease="+op.Ref+":"+op.Expect)
	}
	args = append(args, url)
	for _, op := range ops {
		if op.New == "" {
			args = append(args, ":"+op.Ref)
		} else {
			args = append(args, op.New+":"+op.Ref)
		}
	}
	stdout, stderr, err := g.runRemote(ctx, repo, cred, url, args...)
	res := &PushResult{Refs: ParsePorcelain(string(stdout)), Stderr: string(stderr)}
	if err != nil {
		if strings.Contains(res.Stderr, "does not support --atomic") {
			return res, ErrAtomicUnsupported
		}
		if len(res.Refs) == 0 {
			return res, err
		}
	}
	return res, nil
}

// ParsePorcelain parses `git push --porcelain` output.
//
//	To <url>
//	<flag>\t<from>:<to>\t<summary> (<reason>)
//	Done
func ParsePorcelain(out string) map[string]RefStatus {
	refs := map[string]RefStatus{}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 3 || line[1] != '\t' {
			continue
		}
		parts := strings.SplitN(line[2:], "\t", 2)
		if len(parts) < 1 {
			continue
		}
		_, to, ok := strings.Cut(parts[0], ":")
		if !ok {
			continue
		}
		st := RefStatus{Ref: to, Flag: line[0]}
		if len(parts) == 2 {
			summary := parts[1]
			if i := strings.Index(summary, " ("); i >= 0 && strings.HasSuffix(summary, ")") {
				st.Reason = summary[i+2 : len(summary)-1]
				summary = summary[:i]
			}
			st.Summary = summary
		}
		refs[to] = st
	}
	return refs
}

// --------------------------------------------------------------- tickets

// Tickets maps one-time askpass tickets to credentials held in memory.
// Each ticket is bound to the origin (scheme://host[:port]) of the remote it
// was issued for, and only answers prompts that name that origin, so a
// redirect or a repository-supplied URL cannot obtain the credential.
type Tickets struct {
	mu sync.Mutex
	m  map[string]ticket
}

type ticket struct {
	cred   Cred
	origin string
}

func NewTickets() *Tickets { return &Tickets{m: map[string]ticket{}} }

// Issue registers cred for origin and returns a ticket plus a release
// function.
func (t *Tickets) Issue(c Cred, origin string) (string, func()) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(b)
	t.mu.Lock()
	t.m[id] = ticket{cred: c, origin: origin}
	t.mu.Unlock()
	return id, func() {
		t.mu.Lock()
		delete(t.m, id)
		t.mu.Unlock()
	}
}

// Answer returns the response for a git credential prompt such as
// "Username for 'https://github.com': " (git) or
// "Password for \"https://user@github.com\"" (git-lfs).
func (t *Tickets) Answer(ticketID, prompt string) (string, bool) {
	t.mu.Lock()
	tk, ok := t.m[ticketID]
	t.mu.Unlock()
	if !ok || tk.origin == "" || promptOrigin(prompt) != tk.origin {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(prompt)), "username") {
		return tk.cred.Username, true
	}
	return tk.cred.Secret, true
}

// Origin normalises a URL to scheme://host[:port] (lowercase host, default
// ports dropped). It returns "" for URLs without a host.
func Origin(raw string) string {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}

// promptOrigin extracts the origin of the URL quoted in a credential prompt.
func promptOrigin(prompt string) string {
	for _, q := range []string{"'", "\""} {
		i := strings.Index(prompt, q)
		if i < 0 {
			continue
		}
		j := strings.Index(prompt[i+1:], q)
		if j < 0 {
			continue
		}
		return Origin(prompt[i+1 : i+1+j])
	}
	return ""
}
