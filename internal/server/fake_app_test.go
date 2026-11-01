package server_test

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// fakeApp is a GitHub App registered with the fake GitHub: JWTs signed
// with its key buy installation tokens, which then work like a PAT.
type fakeApp struct {
	id           string
	pub          *rsa.PublicKey
	installation map[string]int // account -> installation id
	minted       map[string]bool
	mints        int
	tokenTTL     time.Duration
}

func (f *fakePlatforms) installApp(id string, pub *rsa.PublicKey, accounts map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.app = &fakeApp{id: id, pub: pub, installation: accounts, minted: map[string]bool{}, tokenTTL: time.Hour}
}

func (f *fakePlatforms) appMints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.app.mints
}

// ghTokenOK accepts the fake PAT and live installation tokens.
func (f *fakePlatforms) ghTokenOK(tok string) bool {
	if tok == ghToken {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.app != nil && f.app.minted[tok]
}

func (a *fakeApp) jwtOK(r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(a.pub, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss any   `json:"iss"`
		Exp int64 `json:"exp"`
	}
	json.Unmarshal(raw, &c)
	return fmt.Sprint(c.Iss) == a.id && c.Exp > time.Now().Unix()
}

// serveApp handles the App endpoints of the fake GitHub.
func (f *fakePlatforms) serveApp(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/gh/api/v3/") {
		return false
	}
	p = strings.TrimPrefix(p, "/gh/api/v3")
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.app
	switch {
	case a == nil:
		return false
	case p == "/installation/repositories":
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !a.minted[tok] {
			writeJSONResp(w, 401, map[string]string{"message": "Bad credentials"})
			return true
		}
		writeJSONResp(w, 200, map[string]any{"total_count": 3, "repositories": []any{}})
		return true
	case r.Method == "GET" && p == "/user" && a.minted[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]:
		writeJSONResp(w, 403, map[string]string{"message": "Resource not accessible by integration"})
		return true
	case strings.HasPrefix(p, "/orgs/") && strings.HasSuffix(p, "/installation"), strings.HasPrefix(p, "/users/") && strings.HasSuffix(p, "/installation"):
		if !a.jwtOK(r) {
			writeJSONResp(w, 401, map[string]string{"message": "A JSON web token could not be decoded"})
			return true
		}
		account := strings.Split(p, "/")[2]
		id, ok := a.installation[account]
		if !ok || strings.HasPrefix(p, "/orgs/") != (account == "acme") {
			writeJSONResp(w, 404, map[string]string{"message": "Not Found"})
			return true
		}
		writeJSONResp(w, 200, map[string]any{"id": id})
		return true
	case r.Method == "POST" && strings.HasPrefix(p, "/app/installations/") && strings.HasSuffix(p, "/access_tokens"):
		if !a.jwtOK(r) {
			writeJSONResp(w, 401, map[string]string{"message": "A JSON web token could not be decoded"})
			return true
		}
		a.mints++
		tok := fmt.Sprintf("ghs_fake%d", a.mints)
		a.minted[tok] = true
		writeJSONResp(w, 201, map[string]any{"token": tok, "expires_at": time.Now().Add(a.tokenTTL).UTC().Format(time.RFC3339)})
		return true
	}
	return false
}
