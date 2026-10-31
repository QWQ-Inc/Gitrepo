package sshserve

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	for in, want := range map[string][]string{
		"git-upload-pack 'acme/demo.git'":           {"git-upload-pack", "acme/demo.git"},
		"git-receive-pack '/acme/demo.git'":         {"git-receive-pack", "/acme/demo.git"},
		"git upload-pack \"acme/my repo.git\"":      {"git", "upload-pack", "acme/my repo.git"},
		"git-lfs-authenticate acme/demo.git upload": {"git-lfs-authenticate", "acme/demo.git", "upload"},
		"  git-upload-archive   'acme/demo.git'   ": {"git-upload-archive", "acme/demo.git"},
		"git-upload-pack ''":                        {"git-upload-pack", ""},
	} {
		if got := splitCommand(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

type flakyListener struct {
	net.Listener
	failures int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures > 0 {
		l.failures--
		return nil, errors.New("accept: too many open files")
	}
	return l.Listener.Accept()
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{HostKeyPath: filepath.Join(t.TempDir(), "host_key"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func readBanner(t *testing.T, c net.Conn) (string, error) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	return line, err
}

// Transient accept errors must not end SSH service.
func TestAcceptErrorsAreRetried(t *testing.T) {
	s := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(&flakyListener{Listener: ln, failures: 5})
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if banner, err := readBanner(t, c); err != nil || !strings.HasPrefix(banner, "SSH-2.0-Gitrepo") {
		t.Fatalf("no banner after accept errors: %q %v", banner, err)
	}
}

func TestConnectionsPerAddressAreLimited(t *testing.T) {
	s := newTestServer(t)
	s.MaxConnsPerIP = 2
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	for i, c := range conns[:2] {
		if _, err := readBanner(t, c); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
	}
	if banner, err := readBanner(t, conns[2]); err == nil {
		t.Fatalf("third connection from one address was served: %q", banner)
	}
}
