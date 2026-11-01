// Package githooks implements the hub repository hooks and the GIT_ASKPASS
// helper. They run as short-lived `gitrepo hook …` / `gitrepo askpass`
// subprocesses spawned by git and talk back to the server over its internal
// unix socket.
package githooks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
	"github.com/qwq-inc/gitrepo/internal/lfs"
)

// Environment passed from the server to hooks (through git http-backend).
const (
	EnvSocket      = "GITREPO_SOCKET"
	EnvHookToken   = "GITREPO_HOOK_TOKEN"
	EnvRepo        = "GITREPO_REPO"
	EnvMaxFileSize = "GITREPO_MAX_FILE_SIZE"
	EnvWait        = "GITREPO_WAIT_MS"
	EnvGit         = "GITREPO_GIT"
	EnvTicket      = "GITREPO_ASKPASS_TICKET"
	// EnvTransport ("http" or "ssh") and EnvClientAddr describe the pusher's
	// connection for the audit log.
	EnvTransport  = "GITREPO_TRANSPORT"
	EnvClientAddr = "GITREPO_CLIENT_ADDR"
)

// PostReceiveRequest is sent by the post-receive hook to the server.
type PostReceiveRequest struct {
	Token   string      `json:"token"`
	Repo    string      `json:"repo"`
	Updates []RefUpdate `json:"updates"`
	WaitMS  int64       `json:"wait_ms"`
	Pusher  string      `json:"pusher"`
	// Transport and ClientAddr are recorded in the audit log.
	Transport  string `json:"transport,omitempty"`
	ClientAddr string `json:"client_addr,omitempty"`
}

type RefUpdate struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// AskpassRequest is sent by the askpass helper.
type AskpassRequest struct {
	Ticket string `json:"ticket"`
	Prompt string `json:"prompt"`
}

func readUpdates(r io.Reader) ([]RefUpdate, error) {
	var out []RefUpdate
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		out = append(out, RefUpdate{Old: f[0], New: f[1], Ref: f[2]})
	}
	return out, sc.Err()
}

func say(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "gitrepo: "+format+"\n", args...)
}

func gitBin() string {
	if b := os.Getenv(EnvGit); b != "" {
		return b
	}
	return "git"
}

// ------------------------------------------------------------ pre-receive

// PreReceive enforces hub policy before any ref is updated:
//   - only refs/heads/* and refs/tags/* may be pushed;
//   - protected refs (GITREPO_PROTECTED) are not deleted or rewritten;
//   - no new blob may exceed GITREPO_MAX_FILE_SIZE (the strictest limit of
//     all remotes), so a push the hub accepts will not be rejected later by
//     e.g. GitHub's 100 MiB limit.
//
// New objects are still in git's quarantine area; the inherited environment
// makes them visible to the git commands run here.
func PreReceive(stdin io.Reader, stderr io.Writer) int {
	updates, err := readUpdates(stdin)
	if err != nil {
		say(stderr, "cannot read updates: %v", err)
		return 1
	}
	var problems []string
	var tips []string
	for _, u := range updates {
		if !strings.HasPrefix(u.Ref, "refs/heads/") && !strings.HasPrefix(u.Ref, "refs/tags/") {
			problems = append(problems, fmt.Sprintf("%s: only branches (refs/heads/*) and tags (refs/tags/*) can be pushed", u.Ref))
			continue
		}
		if !gitexec.IsZero(u.New) {
			tips = append(tips, u.New)
		}
	}
	if len(problems) == 0 {
		rules, err := protectedFromEnv()
		if err != nil {
			say(stderr, "cannot read the protection rules: %v", err)
			return 1
		}
		if p := checkProtected(rules, updates); len(p) > 0 {
			problems = append(problems, p...)
			problems = append(problems, "an administrator can lift the rule (gr protect rm) if this is really intended; overwritten history is kept under refs/backup/")
		}
	}
	maxSize, _ := strconv.ParseInt(os.Getenv(EnvMaxFileSize), 10, 64)
	if len(problems) == 0 && len(tips) > 0 {
		scan, err := scanNewBlobs(tips, maxSize)
		if err != nil {
			say(stderr, "cannot inspect pushed objects: %v", err)
			return 1
		}
		problems = append(problems, scan.big...)
		if len(scan.big) > 0 {
			problems = append(problems, fmt.Sprintf("files larger than %s are rejected because at least one mirror would refuse them; remove them from history (e.g. git filter-repo) and push again", humanSize(maxSize)))
		}
		missing, err := missingLFSObjects(scan.small)
		if err != nil {
			say(stderr, "cannot inspect LFS pointers: %v", err)
			return 1
		}
		problems = append(problems, missing...)
		if len(missing) > 0 {
			problems = append(problems, "the LFS objects above were not uploaded to the hub; make sure git-lfs is installed, then run `git lfs push --all <remote>` and push again")
		}
	}
	if len(problems) > 0 {
		say(stderr, "push rejected:")
		for _, p := range problems {
			say(stderr, "  %s", p)
		}
		return 1
	}
	return 0
}

// blobRef is a new blob small enough to be an LFS pointer.
type blobRef struct{ id, path string }

type blobScan struct {
	big   []string  // "path (size)" of blobs above the limit
	small []blobRef // candidate LFS pointers
}

// oversizedBlobs lists new blobs larger than limit as "path (size)".
func oversizedBlobs(tips []string, limit int64) ([]string, error) {
	scan, err := scanNewBlobs(tips, limit)
	return scan.big, err
}

// missingLFSObjects reads candidate pointers and reports those whose object
// is not stored in the repository (the hook's GIT_DIR).
func missingLFSObjects(blobs []blobRef) ([]string, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	gitDir, err := filepath.Abs(os.Getenv("GIT_DIR"))
	if err != nil || os.Getenv("GIT_DIR") == "" {
		if gitDir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	paths := map[string]string{}
	for _, b := range blobs {
		paths[b.id] = b.path
	}
	cmd := exec.Command(gitBin(), "cat-file", "--batch")
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
	go func() {
		w := bufio.NewWriter(stdin)
		for _, b := range blobs {
			w.WriteString(b.id + "\n")
		}
		w.Flush()
		stdin.Close()
	}()
	var missing []string
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
			continue
		}
		size, _ := strconv.Atoi(f[2])
		content := make([]byte, size+1) // trailing newline
		if _, err := io.ReadFull(r, content); err != nil {
			readErr = err
			break
		}
		if p, ok := lfs.ParsePointer(content[:size]); ok && !lfs.Has(gitDir, p.OID, p.Size) {
			missing = append(missing, fmt.Sprintf("%s: LFS object %.12s (%s) is missing on the hub", paths[f[0]], p.OID, humanSize(p.Size)))
		}
	}
	if readErr != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, readErr
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("cat-file --batch: %w", err)
	}
	return missing, nil
}

// scanNewBlobs walks the objects a push introduces. It reports blobs larger
// than limit (when limit > 0) and collects blobs small enough to be LFS
// pointers.
func scanNewBlobs(tips []string, limit int64) (blobScan, error) {
	var scan blobScan
	revArgs := append([]string{"rev-list", "--objects"}, tips...)
	// Only published branches and tags count as already present: objects
	// reachable solely from refs/backup/* (e.g. fetched from a mirror
	// before an overwrite) were never checked and must be checked now.
	revArgs = append(revArgs, "--not", "--branches", "--tags")
	rev := exec.Command(gitBin(), revArgs...)
	check := exec.Command(gitBin(), "cat-file", "--batch-check=%(objecttype) %(objectsize) %(objectname) %(rest)")
	pipe, err := rev.StdoutPipe()
	if err != nil {
		return scan, err
	}
	check.Stdin = pipe
	var revErr bytes.Buffer
	rev.Stderr = &revErr
	out, err := check.StdoutPipe()
	if err != nil {
		return scan, err
	}
	if err := rev.Start(); err != nil {
		return scan, err
	}
	if err := check.Start(); err != nil {
		rev.Process.Kill()
		rev.Wait()
		return scan, err
	}
	// Read with no line-length limit (paths can be arbitrarily long). On any
	// read error both processes are killed before waiting, so neither can be
	// left blocked on a full pipe.
	br := bufio.NewReaderSize(out, 64<<10)
	var readErr error
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimSuffix(line, "\n"); line != "" {
			f := strings.SplitN(line, " ", 4)
			if len(f) >= 3 && f[0] == "blob" {
				size, _ := strconv.ParseInt(f[1], 10, 64)
				if size >= 100 && size <= lfs.MaxPointerSize {
					p := ""
					if len(f) == 4 {
						p = f[3]
					}
					scan.small = append(scan.small, blobRef{id: f[2], path: p})
				}
				if limit > 0 && size > limit {
					path := f[2]
					if len(path) > 12 {
						path = path[:12]
					}
					if len(f) == 4 && f[3] != "" {
						path = f[3]
						if len(path) > 200 {
							path = "…" + path[len(path)-200:]
						}
					}
					scan.big = append(scan.big, fmt.Sprintf("%s (%s)", path, humanSize(size)))
				}
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
		rev.Process.Kill()
		check.Process.Kill()
		rev.Wait()
		check.Wait()
		return scan, readErr
	}
	if err := rev.Wait(); err != nil {
		check.Process.Kill()
		check.Wait()
		return scan, fmt.Errorf("rev-list: %v: %s", err, strings.TrimSpace(revErr.String()))
	}
	if err := check.Wait(); err != nil {
		return scan, fmt.Errorf("cat-file: %v", err)
	}
	return scan, nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ----------------------------------------------------------- post-receive

// PostReceive notifies the server, which backs up overwritten refs, queues
// the mirror sync and streams per-remote results back for up to the wait
// time. Push options:
//
//	git push -o gitrepo.wait=0      return immediately
//	git push -o gitrepo.wait=2m     wait longer
func PostReceive(stdin io.Reader, stderr io.Writer) int {
	updates, err := readUpdates(stdin)
	if err != nil || len(updates) == 0 {
		return 0
	}
	wait, _ := strconv.ParseInt(os.Getenv(EnvWait), 10, 64)
	if n, _ := strconv.Atoi(os.Getenv("GIT_PUSH_OPTION_COUNT")); n > 0 {
		for i := 0; i < n; i++ {
			opt := os.Getenv(fmt.Sprintf("GIT_PUSH_OPTION_%d", i))
			if v, ok := strings.CutPrefix(opt, "gitrepo.wait="); ok {
				if d, err := parseWait(v); err == nil {
					wait = d.Milliseconds()
				} else {
					say(stderr, "ignoring push option %q: %v", opt, err)
				}
			}
		}
	}
	req := PostReceiveRequest{
		Token:      os.Getenv(EnvHookToken),
		Repo:       os.Getenv(EnvRepo),
		Updates:    updates,
		WaitMS:     wait,
		Pusher:     os.Getenv("REMOTE_USER"),
		Transport:  os.Getenv(EnvTransport),
		ClientAddr: os.Getenv(EnvClientAddr),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wait)*time.Millisecond+30*time.Second)
	defer cancel()
	resp, err := post(ctx, "/internal/post-receive", req)
	if err != nil {
		say(stderr, "warning: could not notify the sync service (%v); the periodic patrol will sync this push", err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		say(stderr, "warning: sync service returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
		return 0
	}
	// Stream lines as they arrive so the client sees progress live.
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		fmt.Fprintln(stderr, sc.Text())
	}
	return 0
}

func parseWait(v string) (time.Duration, error) {
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("want seconds or a duration like 90s")
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d, nil
}

// ---------------------------------------------------------------- askpass

// Askpass answers a git credential prompt using the ticket in the
// environment.
func Askpass(prompt string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := post(ctx, "/internal/askpass", AskpassRequest{Ticket: os.Getenv(EnvTicket), Prompt: prompt})
	if err != nil {
		fmt.Fprintf(stderr, "gitrepo askpass: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "gitrepo askpass: %s\n", resp.Status)
		return 1
	}
	fmt.Fprintln(stdout, string(body))
	return 0
}

// --------------------------------------------------------------- transport

func post(ctx context.Context, path string, body any) (*http.Response, error) {
	sock := os.Getenv(EnvSocket)
	if sock == "" {
		return nil, fmt.Errorf("%s is not set", EnvSocket)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gitrepo"+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}
