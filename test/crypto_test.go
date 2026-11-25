package test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"anz-training/crypto"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestCrypto_RoundTrip(t *testing.T) {
	plain := []byte(`{"owner":"acme","token":"ghp_x"}`)
	enc, err := crypto.Encrypt(testKey(1), plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "acme") {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := crypto.Decrypt(testKey(1), enc)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip failed: %q, %v", got, err)
	}
}

func TestCrypto_FreshNoncePerCall(t *testing.T) {
	a, _ := crypto.Encrypt(testKey(1), []byte("same"))
	b, _ := crypto.Encrypt(testKey(1), []byte("same"))
	if a == b {
		t.Fatal("two encryptions of the same plaintext are identical (nonce reuse)")
	}
}

func TestCrypto_WrongKeyAndTamper(t *testing.T) {
	enc, _ := crypto.Encrypt(testKey(1), []byte("secret"))
	if _, err := crypto.Decrypt(testKey(2), enc); err == nil {
		t.Fatal("decrypt with wrong key must fail")
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	raw[len(raw)-1] ^= 0xff
	if _, err := crypto.Decrypt(testKey(1), base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
	for _, bad := range []string{"", "not-base64!!", "AAAA"} {
		if _, err := crypto.Decrypt(testKey(1), bad); err == nil {
			t.Fatalf("decrypt(%q) must fail", bad)
		}
	}
}

func TestCrypto_BadKeySize(t *testing.T) {
	if _, err := crypto.Encrypt([]byte("short"), []byte("x")); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestCrypto_LoadMasterKey(t *testing.T) {
	k := testKey(7)
	if got, err := crypto.LoadMasterKey(hex.EncodeToString(k)); err != nil || !bytes.Equal(got, k) {
		t.Fatalf("hex: %v", err)
	}
	if got, err := crypto.LoadMasterKey(base64.StdEncoding.EncodeToString(k)); err != nil || !bytes.Equal(got, k) {
		t.Fatalf("base64: %v", err)
	}
	for _, bad := range []string{"", "tooshort", base64.StdEncoding.EncodeToString([]byte("16 bytes only!!!"))} {
		if _, err := crypto.LoadMasterKey(bad); err == nil {
			t.Fatalf("LoadMasterKey(%q) should fail", bad)
		}
	}
}
