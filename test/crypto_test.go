package test

import (
	"anz-training/internal/testutil"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"anz-training/crypto"
)

func TestCrypto_RoundTrip(t *testing.T) {
	plain := []byte(`{"owner":"acme","token":"ghp_x"}`)
	enc, err := crypto.Encrypt(testutil.TestKey(1), plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "acme") {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := crypto.Decrypt(testutil.TestKey(1), enc)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip failed: %q, %v", got, err)
	}
}

func TestCrypto_FreshNoncePerCall(t *testing.T) {
	a, _ := crypto.Encrypt(testutil.TestKey(1), []byte("same"))
	b, _ := crypto.Encrypt(testutil.TestKey(1), []byte("same"))
	if a == b {
		t.Fatal("two encryptions of the same plaintext are identical (nonce reuse)")
	}
}

func TestCrypto_WrongKeyAndTamper(t *testing.T) {
	enc, _ := crypto.Encrypt(testutil.TestKey(1), []byte("secret"))
	if _, err := crypto.Decrypt(testutil.TestKey(2), enc); err == nil {
		t.Fatal("decrypt with wrong key must fail")
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	raw[len(raw)-1] ^= 0xff
	if _, err := crypto.Decrypt(testutil.TestKey(1), base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
	for _, bad := range []string{"", "not-base64!!", "AAAA"} {
		if _, err := crypto.Decrypt(testutil.TestKey(1), bad); err == nil {
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
	k := testutil.TestKey(7)
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

// A key pasted into Secret Manager (console or `echo`) usually carries a trailing newline.
func TestCrypto_LoadMasterKey_IgnoresSurroundingWhitespace(t *testing.T) {
	k := testutil.TestKey(9)
	for _, in := range []string{hex.EncodeToString(k) + "\n", " " + hex.EncodeToString(k) + " \r\n", base64.StdEncoding.EncodeToString(k) + "\n"} {
		if got, err := crypto.LoadMasterKey(in); err != nil || !bytes.Equal(got, k) {
			t.Fatalf("LoadMasterKey(%q) = %v, %v", in, got, err)
		}
	}
}
