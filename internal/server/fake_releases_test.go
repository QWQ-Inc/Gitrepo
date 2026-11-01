package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Fake release APIs of the four platforms, strict where it matters:
// creating a release needs the tag in the fake mirror's git repository,
// asset names must be unique, Gitee wants a name, notes and a target.

type fakeAsset struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	SHA  string `json:"-"`
	URL  string `json:"-"` // GitLab link
}

type fakeRelease struct {
	ID      int
	Created string
	Tag     string
	Name    string
	Body    string
	Pre     bool
	Target  string
	Assets  []*fakeAsset
}

type fakeReleaseStore struct {
	rels    map[string]*fakeRelease // platform|project|tag
	nextID  int
	uploads map[string]string // GitLab upload path -> sha
	log     []string
	// Failure injection, per platform.
	failDelete map[string]bool // DELETE of a release answers 500
	failCreate map[string]bool // POST of a release answers 500, nothing created
	loseCreate map[string]bool // the release is created, but the answer is a 504
	slowUpload map[string]bool // uploads take a second
}

// inject sets a failure switch.
func (f *fakePlatforms) inject(what, platform string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.relStore()
	m := map[string]*map[string]bool{"delete": &st.failDelete, "create": &st.failCreate, "lose": &st.loseCreate, "slow": &st.slowUpload}[what]
	if *m == nil {
		*m = map[string]bool{}
	}
	(*m)[platform] = on
}

// githubAssetName mimics GitHub, which rewrites spaces and other
// characters in uploaded file names.
func githubAssetName(n string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r > 127 {
			return '.'
		}
		return r
	}, n)
}

func (f *fakePlatforms) relStore() *fakeReleaseStore {
	if f.releases == nil {
		f.releases = &fakeReleaseStore{rels: map[string]*fakeRelease{}, uploads: map[string]string{}}
	}
	return f.releases
}

// release returns the fake release for a tag (nil if none).
func (f *fakePlatforms) release(platform, project, tag string) *fakeRelease {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relStore().rels[platform+"|"+project+"|"+tag]
}

// addForeignRelease simulates a release someone created on the platform.
func (f *fakePlatforms) addForeignRelease(platform, project, tag string, assets ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.relStore()
	st.nextID++
	rel := &fakeRelease{ID: st.nextID, Tag: tag, Name: "made by hand", Body: "by hand", Created: fmt.Sprintf("2026-01-01T00:00:%02dZ", st.nextID%60)}
	for _, a := range assets {
		st.nextID++
		rel.Assets = append(rel.Assets, &fakeAsset{ID: st.nextID, Name: a, Size: 1, SHA: "foreign"})
	}
	st.rels[platform+"|"+project+"|"+tag] = rel
}

func (f *fakePlatforms) countLog(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.relStore().log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (f *fakePlatforms) tagExists(platform, project, tag string) bool {
	_, err := gitCmd(filepath.Join(f.dir, platform, project+".git"), "rev-parse", "--verify", "--quiet", "refs/tags/"+tag)
	return err == nil
}

// readUpload returns (name, content) of a raw or multipart upload.
func readUpload(r *http.Request, field string) (string, []byte, error) {
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		b, err := io.ReadAll(r.Body)
		return r.URL.Query().Get("name"), b, err
	}
	if r.ContentLength <= 0 {
		return "", nil, fmt.Errorf("multipart upload without Content-Length")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return "", nil, fmt.Errorf("no %q part: %v", field, err)
		}
		if part.FormName() == field {
			b, err := io.ReadAll(part)
			return part.FileName(), b, err
		}
	}
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// serveRelease handles release routes; it reports whether it did.
func (f *fakePlatforms) serveRelease(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.EscapedPath()
	var platform, rest string
	switch {
	case strings.HasPrefix(p, "/gh/api/v3/repos/") && strings.Contains(p, "/releases"):
		platform, rest = "gh", strings.TrimPrefix(p, "/gh/api/v3/repos/")
	case strings.HasPrefix(p, "/gh/api/uploads/repos/"):
		platform, rest = "gh-upload", strings.TrimPrefix(p, "/gh/api/uploads/repos/")
	case strings.HasPrefix(p, "/gl/api/v4/projects/") && (strings.Contains(p, "/releases") || strings.HasSuffix(p, "/uploads")):
		platform, rest = "gl", strings.TrimPrefix(p, "/gl/api/v4/projects/")
	case strings.HasPrefix(p, "/gt/api/v1/repos/") && strings.Contains(p, "/releases"):
		platform, rest = "gt", strings.TrimPrefix(p, "/gt/api/v1/repos/")
	case strings.HasPrefix(p, "/ge/api/v5/repos/") && strings.Contains(p, "/releases"):
		platform, rest = "ge", strings.TrimPrefix(p, "/ge/api/v5/repos/")
	default:
		return false
	}
	switch platform {
	case "gh", "gh-upload":
		if !f.ghTokenOK(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			writeJSONResp(w, 401, map[string]string{"message": "Bad credentials"})
			return true
		}
	case "gl":
		if r.Header.Get("PRIVATE-TOKEN") != glToken {
			writeJSONResp(w, 401, map[string]string{"message": "401 Unauthorized"})
			return true
		}
	case "gt":
		if r.Header.Get("Authorization") != "token "+gtToken {
			writeJSONResp(w, 401, map[string]string{"message": "token is required"})
			return true
		}
	case "ge":
		if r.URL.Query().Get("access_token") != geToken {
			writeJSONResp(w, 401, map[string]string{"message": "401 Unauthorized"})
			return true
		}
	}
	f.mu.Lock()
	slow := platform == "gh-upload" && f.relStore().slowUpload["gh"]
	f.mu.Unlock()
	if slow {
		time.Sleep(time.Second)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.relStore()
	seg := strings.Split(rest, "/")
	unesc := func(s string) string { v, _ := url.PathUnescape(s); return v }
	var project string
	var after []string
	if platform == "gl" {
		project, after = unesc(seg[0]), seg[1:]
	} else {
		project, after = seg[0]+"/"+seg[1], seg[2:] // after[0] == "releases"
	}
	plat := strings.TrimSuffix(platform, "-upload")
	key := func(tag string) string { return plat + "|" + project + "|" + tag }
	byID := func(id string) *fakeRelease {
		for k, rel := range st.rels {
			if strings.HasPrefix(k, plat+"|"+project+"|") && strconv.Itoa(rel.ID) == id {
				return rel
			}
		}
		return nil
	}
	relJSON := func(rel *fakeRelease) map[string]any {
		assets := []map[string]any{}
		for _, a := range rel.Assets {
			assets = append(assets, map[string]any{"id": a.ID, "name": a.Name, "size": a.Size, "url": a.URL})
		}
		out := map[string]any{"id": rel.ID, "tag_name": rel.Tag, "name": rel.Name, "body": rel.Body, "description": rel.Body, "prerelease": rel.Pre, "assets": assets, "created_at": rel.Created}
		if plat == "gh" {
			out["upload_url"] = f.srv.URL + "/gh/api/uploads/repos/" + project + "/releases/" + strconv.Itoa(rel.ID) + "/assets{?name,label}"
		}
		if plat == "gl" {
			out["assets"] = map[string]any{"links": assets}
		}
		return out
	}
	notFound := func() bool { writeJSONResp(w, 404, map[string]string{"message": "Not Found"}); return true }
	decode := func() map[string]any {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		return body
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	addAsset := func(rel *fakeRelease, name string, content []byte) *fakeAsset {
		for _, a := range rel.Assets {
			if a.Name == name {
				return nil
			}
		}
		st.nextID++
		a := &fakeAsset{ID: st.nextID, Name: name, Size: int64(len(content)), SHA: sha(content)}
		rel.Assets = append(rel.Assets, a)
		return a
	}
	removeAsset := func(rel *fakeRelease, id string) bool {
		for i, a := range rel.Assets {
			if strconv.Itoa(a.ID) == id {
				rel.Assets = append(rel.Assets[:i], rel.Assets[i+1:]...)
				return true
			}
		}
		return false
	}
	logf := func(format string, args ...any) { st.log = append(st.log, fmt.Sprintf(format, args...)) }

	switch {
	// GitLab project uploads and links.
	case plat == "gl" && r.Method == "POST" && len(after) == 1 && after[0] == "uploads":
		name, content, err := readUpload(r, "file")
		if err != nil {
			writeJSONResp(w, 400, map[string]string{"message": err.Error()})
			return true
		}
		path := "/" + project + "/uploads/" + sha(content)[:16] + "/" + name
		st.uploads[path] = sha(content)
		logf("gl upload %s", name)
		writeJSONResp(w, 201, map[string]any{"url": "/uploads/" + sha(content)[:16] + "/" + name, "full_path": path})
		return true
	case plat == "gl" && len(after) >= 2:
		tag := unesc(after[1])
		rel := st.rels[key(tag)]
		switch {
		case r.Method == "GET" && len(after) == 2:
			if rel == nil {
				return notFound()
			}
			writeJSONResp(w, 200, relJSON(rel))
		case r.Method == "PUT" && len(after) == 2:
			if rel == nil {
				return notFound()
			}
			b := decode()
			rel.Name, rel.Body = str(b, "name"), str(b, "description")
			logf("gl update %s", tag)
			writeJSONResp(w, 200, relJSON(rel))
		case r.Method == "DELETE" && len(after) == 2:
			if rel == nil {
				return notFound()
			}
			if st.failDelete["gl"] {
				writeJSONResp(w, 500, map[string]string{"message": "500 Internal Server Error"})
				return true
			}
			delete(st.rels, key(tag))
			logf("gl delete %s", tag)
			writeJSONResp(w, 200, relJSON(rel))
		case r.Method == "POST" && len(after) == 4 && after[2] == "assets" && after[3] == "links":
			if rel == nil {
				return notFound()
			}
			b := decode()
			link := str(b, "url")
			u, _ := url.Parse(link)
			shaOf, ok := st.uploads[strings.TrimPrefix(u.Path, "/gl")] // the fake's web root is /gl
			if !ok || !strings.HasPrefix(link, f.srv.URL+"/gl/") {
				writeJSONResp(w, 400, map[string]string{"message": "link does not point to an upload: " + link})
				return true
			}
			for _, a := range rel.Assets {
				if a.Name == str(b, "name") {
					writeJSONResp(w, 400, map[string]string{"message": "name has already been taken"})
					return true
				}
			}
			st.nextID++
			a := &fakeAsset{ID: st.nextID, Name: str(b, "name"), SHA: shaOf, URL: link}
			rel.Assets = append(rel.Assets, a)
			writeJSONResp(w, 201, map[string]any{"id": a.ID, "name": a.Name, "url": link})
		case r.Method == "DELETE" && len(after) == 5 && after[2] == "assets":
			if rel == nil || !removeAsset(rel, after[4]) {
				return notFound()
			}
			logf("gl delete asset %s", after[4])
			writeJSONResp(w, 200, map[string]any{})
		default:
			return notFound()
		}
		return true
	case plat == "gl" && r.Method == "POST" && len(after) == 1 && after[0] == "releases":
		b := decode()
		tag := str(b, "tag_name")
		if !f.tagExists("gl", project, tag) {
			writeJSONResp(w, 422, map[string]string{"message": "Tag does not exist"})
			return true
		}
		if st.rels[key(tag)] != nil {
			writeJSONResp(w, 409, map[string]string{"message": "Release already exists"})
			return true
		}
		st.nextID++
		rel := &fakeRelease{ID: st.nextID, Tag: tag, Name: str(b, "name"), Body: str(b, "description"), Created: fmt.Sprintf("2026-02-01T00:00:%02dZ", st.nextID%60)}
		st.rels[key(tag)] = rel
		logf("gl create %s", tag)
		writeJSONResp(w, 201, relJSON(rel))
		return true
	}

	// GitHub, Gitea and Gitee share the /repos/o/r/releases layout.
	if len(after) == 0 || after[0] != "releases" {
		return notFound()
	}
	after = after[1:]
	switch {
	case r.Method == "GET" && len(after) >= 2 && after[0] == "tags":
		if plat != "gh" && len(after) != 2 {
			return notFound() // Gitea and Gitee route the tag as one path segment
		}
		rel := st.rels[key(unesc(strings.Join(after[1:], "/")))]
		if rel == nil {
			if plat == "ge" {
				writeJSONResp(w, 200, map[string]any{}) // Gitee answers {} for a tag without a release
				return true
			}
			return notFound()
		}
		writeJSONResp(w, 200, relJSON(rel))
	case r.Method == "POST" && len(after) == 0:
		b := decode()
		tag := str(b, "tag_name")
		if !f.tagExists(plat, project, tag) {
			writeJSONResp(w, 422, map[string]string{"message": "tag does not exist; the platform would create it from a branch"})
			return true
		}
		if plat == "ge" && (str(b, "name") == "" || str(b, "body") == "" || str(b, "target_commitish") == "") {
			writeJSONResp(w, 400, map[string]string{"message": "name, body and target_commitish are required"})
			return true
		}
		if st.rels[key(tag)] != nil {
			writeJSONResp(w, 422, map[string]string{"message": "already_exists"})
			return true
		}
		if st.failCreate[plat] {
			logf("%s create-failed %s", plat, tag)
			writeJSONResp(w, 500, map[string]string{"message": "Server Error"})
			return true
		}
		st.nextID++
		pre, _ := b["prerelease"].(bool)
		rel := &fakeRelease{ID: st.nextID, Tag: tag, Name: str(b, "name"), Body: str(b, "body"), Pre: pre, Target: str(b, "target_commitish")}
		st.rels[key(tag)] = rel
		logf("%s create %s", plat, tag)
		if st.loseCreate[plat] {
			writeJSONResp(w, 504, map[string]string{"message": "Gateway Timeout"})
			return true
		}
		writeJSONResp(w, 201, relJSON(rel))
	case r.Method == "PATCH" && len(after) == 1:
		rel := byID(after[0])
		if rel == nil {
			return notFound()
		}
		b := decode()
		if plat == "ge" && (str(b, "tag_name") != rel.Tag || str(b, "name") == "" || str(b, "body") == "") {
			writeJSONResp(w, 400, map[string]string{"message": "tag_name, name and body are required"})
			return true
		}
		rel.Name, rel.Body = str(b, "name"), str(b, "body")
		rel.Pre, _ = b["prerelease"].(bool)
		logf("%s update %s", plat, rel.Tag)
		writeJSONResp(w, 200, relJSON(rel))
	case r.Method == "DELETE" && len(after) == 1:
		rel := byID(after[0])
		if rel == nil {
			return notFound()
		}
		if st.failDelete[plat] {
			writeJSONResp(w, 500, map[string]string{"message": "Server Error"})
			return true
		}
		delete(st.rels, key(rel.Tag))
		logf("%s delete %s", plat, rel.Tag)
		w.WriteHeader(204)
	case plat == "gh" && r.Method == "DELETE" && len(after) == 2 && after[0] == "assets":
		for _, rel := range st.rels {
			if removeAsset(rel, after[1]) {
				logf("gh delete asset %s", after[1])
				w.WriteHeader(204)
				return true
			}
		}
		return notFound()
	case r.Method == "POST" && len(after) == 2 && (platform == "gh-upload" && after[1] == "assets" || plat == "gt" && after[1] == "assets" || plat == "ge" && after[1] == "attach_files"):
		rel := byID(after[0])
		if rel == nil {
			return notFound()
		}
		field := map[string]string{"gt": "attachment", "ge": "file"}[plat]
		name, content, err := readUpload(r, field)
		if plat == "gt" {
			name = r.URL.Query().Get("name")
		}
		if err != nil || name == "" {
			writeJSONResp(w, 400, map[string]string{"message": fmt.Sprintf("bad upload: %v", err)})
			return true
		}
		if plat == "gh" && r.ContentLength != int64(len(content)) {
			writeJSONResp(w, 400, map[string]string{"message": "Content-Length required"})
			return true
		}
		if plat == "gh" {
			name = githubAssetName(name)
		}
		a := addAsset(rel, name, content)
		if a == nil {
			writeJSONResp(w, 422, map[string]string{"message": "already_exists"})
			return true
		}
		logf("%s upload %s", plat, name)
		writeJSONResp(w, 201, map[string]any{"id": a.ID, "name": a.Name, "size": a.Size})
	case plat == "ge" && r.Method == "GET" && len(after) == 2 && after[1] == "attach_files":
		rel := byID(after[0])
		if rel == nil {
			return notFound()
		}
		writeJSONResp(w, 200, relJSON(rel)["assets"])
	case r.Method == "DELETE" && len(after) == 3 && (plat == "gt" && after[1] == "assets" || plat == "ge" && after[1] == "attach_files"):
		rel := byID(after[0])
		if rel == nil || !removeAsset(rel, after[2]) {
			return notFound()
		}
		logf("%s delete asset %s", plat, after[2])
		w.WriteHeader(204)
	default:
		return notFound()
	}
	return true
}
