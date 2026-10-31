// Package gitexec runs the system git binary. All transport goes through the
// real git so protocol v2, force-with-lease, atomic pushes and future LFS
// support behave exactly like upstream.
//
// Credentials never appear on the command line or in URLs: HTTPS remotes get
// them through GIT_ASKPASS (a repohub shim that asks the server over a unix
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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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

func (g *Git) run(ctx context.Context, dir string, stdin []byte, extraEnv []string, args ...string) ([]byte, []byte, error) {
	full := append(append([]string{}, baseConfig...), args...)
	cmd := exec.CommandContext(ctx, g.Bin, full...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), g.Env...), extraEnv...)
	cmd.WaitDelay = 5 * time.Second
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
func (g *Git) runRemote(ctx context.Context, dir string, cred *Cred, args ...string) ([]byte, []byte, error) {
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
			ticket, release := g.Tickets.Issue(*cred)
			cleanup = append(cleanup, release)
			env = append(env,
				"GIT_ASKPASS="+g.AskpassPath,
				"REPOHUB_SOCKET="+g.SocketPath,
				"REPOHUB_ASKPASS_TICKET="+ticket,
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
	return g.run(ctx, dir, nil, env, args...)
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
	out, _, err := g.runRemote(ctx, "", cred, "ls-remote", "--refs", url)
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
	_, _, err := g.runRemote(ctx, repo, cred, args...)
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
// same atomic push failed.
func (s RefStatus) AtomicCollateral() bool {
	return s.Flag == '!' && strings.Contains(s.Reason, "atomic push failed")
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
	stdout, stderr, err := g.runRemote(ctx, repo, cred, args...)
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
type Tickets struct {
	mu sync.Mutex
	m  map[string]Cred
}

func NewTickets() *Tickets { return &Tickets{m: map[string]Cred{}} }

// Issue registers cred and returns a ticket plus a release function.
func (t *Tickets) Issue(c Cred) (string, func()) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(b)
	t.mu.Lock()
	t.m[id] = c
	t.mu.Unlock()
	return id, func() {
		t.mu.Lock()
		delete(t.m, id)
		t.mu.Unlock()
	}
}

// Answer returns the response for a git credential prompt.
func (t *Tickets) Answer(ticket, prompt string) (string, bool) {
	t.mu.Lock()
	c, ok := t.m[ticket]
	t.mu.Unlock()
	if !ok {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(prompt)), "username") {
		return c.Username, true
	}
	return c.Secret, true
}
