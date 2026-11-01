package provider

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwq-inc/gitrepo/internal/store"
)

func testAppKey(t *testing.T) (*rsa.PrivateKey, string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// verifyJWT checks an RS256 JWT and returns its claims.
func verifyJWT(tok string, pub *rsa.PublicKey) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, err
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	return claims, json.Unmarshal(raw, &claims)
}

func TestAppJWTAndKeys(t *testing.T) {
	k, pemPKCS1 := testAppKey(t)
	now := time.Now()
	tok, err := appJWT("4242", k, now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifyJWT(tok, &k.PublicKey)
	if err != nil {
		t.Fatalf("signature: %v", err)
	}
	if claims["iss"] != "4242" || int64(claims["exp"].(float64)) > now.Add(10*time.Minute).Unix() || int64(claims["iat"].(float64)) > now.Unix() {
		t.Fatalf("claims: %v", claims)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pemPKCS8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	for in, ok := range map[string]bool{
		pemPKCS1: true, pemPKCS8: true, "not a key": false,
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})): false,
	} {
		if _, err := ParseAppKey(in); (err == nil) != ok {
			t.Errorf("ParseAppKey(%.30q): %v", in, err)
		}
	}
}

func TestInstallationTokens(t *testing.T) {
	k, keyPEM := testAppKey(t)
	var mints atomic.Int32
	ttl := time.Hour
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, err := verifyJWT(auth, &k.PublicKey); err != nil {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/v3/orgs/acme/installation":
			w.WriteHeader(404) // acme is a user here
		case "/api/v3/users/acme/installation":
			fmt.Fprint(w, `{"id": 77}`)
		case "/api/v3/app/installations/77/access_tokens":
			n := mints.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("ghs_%d", n), "expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339)})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	tg := &store.Target{Provider: "github", BaseURL: srv.URL, Namespace: "acme", CredKind: CredGitHubApp, AppID: "4242"}
	ctx := context.Background()
	id, err := FindAppInstallation(ctx, tg, keyPEM)
	if err != nil || id != "77" {
		t.Fatalf("installation: %q %v", id, err)
	}
	tg.InstallationID = id
	a, err := ResolveSecret(ctx, tg, keyPEM)
	b, _ := ResolveSecret(ctx, tg, keyPEM)
	if err != nil || a != "ghs_1" || b != "ghs_1" || mints.Load() != 1 {
		t.Fatalf("cached token: %q %q %v (mints %d)", a, b, err, mints.Load())
	}
	// A token that could expire during a release publish (up to 30
	// minutes) is replaced.
	ttl = 35 * time.Minute
	tg2 := *tg
	tg2.InstallationID = "77"
	other := keyPEM + "\n" // a different key text: a separate cache entry
	c, _ := ResolveSecret(ctx, &tg2, other)
	d, _ := ResolveSecret(ctx, &tg2, other)
	if c == d || mints.Load() != 3 {
		t.Fatalf("short-lived tokens must be renewed: %q %q (mints %d)", c, d, mints.Load())
	}
	// Tokens and passwords pass through unchanged.
	if s, _ := ResolveSecret(ctx, &store.Target{CredKind: "token"}, "pat"); s != "pat" {
		t.Fatal(s)
	}
	tg.InstallationID = ""
	if _, err := ResolveSecret(ctx, tg, keyPEM); err == nil {
		t.Fatal("no installation, no token")
	}
}
