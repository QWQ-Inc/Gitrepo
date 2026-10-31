package secrets

import "testing"

func TestSealOpen(t *testing.T) {
	key, err := ParseKey(GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := box.Seal([]byte("ghp_secret"), "target:github")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := box.Open(ct, "target:github")
	if err != nil || string(pt) != "ghp_secret" {
		t.Fatalf("round trip: %q %v", pt, err)
	}
	if _, err := box.Open(ct, "target:other"); err == nil {
		t.Fatal("ciphertext opened under a different target")
	}
	other, _ := New(make([]byte, 32))
	if _, err := other.Open(ct, "target:github"); err == nil {
		t.Fatal("ciphertext opened with the wrong key")
	}
}

func TestParseKey(t *testing.T) {
	hex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if k, err := ParseKey(hex); err != nil || len(k) != 32 || k[31] != 0x1f {
		t.Fatalf("hex key: %v %v", k, err)
	}
	if _, err := ParseKey("too-short"); err == nil {
		t.Fatal("accepted short key")
	}
}
