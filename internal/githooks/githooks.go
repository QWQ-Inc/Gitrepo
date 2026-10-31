// Package githooks implements the hub repository hooks and the GIT_ASKPASS
// helper. They run as short-lived `repohub hook …` / `repohub askpass`
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
	"strconv"
	"strings"
	"time"

	"github.com/qwq-inc/gitrepo/internal/gitexec"
)

// Environment passed from the server to hooks (through git http-backend).
const (
	EnvSocket      = "REPOHUB_SOCKET"
	EnvHookToken   = "REPOHUB_HOOK_TOKEN"
	EnvRepo        = "REPOHUB_REPO"
	EnvMaxFileSize = "REPOHUB_MAX_FILE_SIZE"
	EnvWait        = "REPOHUB_WAIT_MS"
	EnvGit         = "REPOHUB_GIT"
	EnvTicket      = "REPOHUB_ASKPASS_TICKET"
)

// PostReceiveRequest is sent by the post-receive hook to the server.
type PostReceiveRequest struct {
	Token   string      `json:"token"`
	Repo    string      `json:"repo"`
	Updates []RefUpdate `json:"updates"`
	WaitMS  int64       `json:"wait_ms"`
	Pusher  string      `json:"pusher"`
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
	fmt.Fprintf(w, "repohub: "+format+"\n", args...)
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
//   - no new blob may exceed REPOHUB_MAX_FILE_SIZE (the strictest limit of
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
	maxSize, _ := strconv.ParseInt(os.Getenv(EnvMaxFileSize), 10, 64)
	if len(problems) == 0 && maxSize > 0 && len(tips) > 0 {
		big, err := oversizedBlobs(tips, maxSize)
		if err != nil {
			say(stderr, "cannot inspect pushed objects: %v", err)
			return 1
		}
		problems = append(problems, big...)
		if len(big) > 0 {
			problems = append(problems, fmt.Sprintf("files larger than %s are rejected because at least one mirror would refuse them; remove them from history (e.g. git filter-repo) and push again", humanSize(maxSize)))
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

// oversizedBlobs lists new blobs larger than limit as "path (size)".
func oversizedBlobs(tips []string, limit int64) ([]string, error) {
	revArgs := append([]string{"rev-list", "--objects"}, tips...)
	revArgs = append(revArgs, "--not", "--all")
	rev := exec.Command(gitBin(), revArgs...)
	check := exec.Command(gitBin(), "cat-file", "--batch-check=%(objecttype) %(objectsize) %(objectname) %(rest)")
	pipe, err := rev.StdoutPipe()
	if err != nil {
		return nil, err
	}
	check.Stdin = pipe
	var revErr bytes.Buffer
	rev.Stderr = &revErr
	out, err := check.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := rev.Start(); err != nil {
		return nil, err
	}
	if err := check.Start(); err != nil {
		rev.Process.Kill()
		rev.Wait()
		return nil, err
	}
	// Read with no line-length limit (paths can be arbitrarily long). On any
	// read error both processes are killed before waiting, so neither can be
	// left blocked on a full pipe.
	var big []string
	br := bufio.NewReaderSize(out, 64<<10)
	var readErr error
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimSuffix(line, "\n"); line != "" {
			f := strings.SplitN(line, " ", 4)
			if len(f) >= 3 && f[0] == "blob" {
				size, _ := strconv.ParseInt(f[1], 10, 64)
				if size > limit {
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
					big = append(big, fmt.Sprintf("%s (%s)", path, humanSize(size)))
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
		return nil, readErr
	}
	if err := rev.Wait(); err != nil {
		check.Process.Kill()
		check.Wait()
		return nil, fmt.Errorf("rev-list: %v: %s", err, strings.TrimSpace(revErr.String()))
	}
	if err := check.Wait(); err != nil {
		return nil, fmt.Errorf("cat-file: %v", err)
	}
	return big, nil
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
//	git push -o repohub.wait=0      return immediately
//	git push -o repohub.wait=2m     wait longer
func PostReceive(stdin io.Reader, stderr io.Writer) int {
	updates, err := readUpdates(stdin)
	if err != nil || len(updates) == 0 {
		return 0
	}
	wait, _ := strconv.ParseInt(os.Getenv(EnvWait), 10, 64)
	if n, _ := strconv.Atoi(os.Getenv("GIT_PUSH_OPTION_COUNT")); n > 0 {
		for i := 0; i < n; i++ {
			opt := os.Getenv(fmt.Sprintf("GIT_PUSH_OPTION_%d", i))
			if v, ok := strings.CutPrefix(opt, "repohub.wait="); ok {
				if d, err := parseWait(v); err == nil {
					wait = d.Milliseconds()
				} else {
					say(stderr, "ignoring push option %q: %v", opt, err)
				}
			}
		}
	}
	req := PostReceiveRequest{
		Token:   os.Getenv(EnvHookToken),
		Repo:    os.Getenv(EnvRepo),
		Updates: updates,
		WaitMS:  wait,
		Pusher:  os.Getenv("REMOTE_USER"),
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
		fmt.Fprintf(stderr, "repohub askpass: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "repohub askpass: %s\n", resp.Status)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://repohub"+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}
