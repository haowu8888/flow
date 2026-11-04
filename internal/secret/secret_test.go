package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	c := NewRandomCipher()
	ct := c.Seal([]byte("hunter2"), "default//TOKEN")
	if bytes.Contains(ct, []byte("hunter2")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	if got, err := c.Open(ct, "default//TOKEN"); err != nil || string(got) != "hunter2" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := c.Open(ct, "default/p1/TOKEN"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong aad = %v", err)
	}
	if _, err := NewRandomCipher().Open(ct, "default//TOKEN"); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong key = %v", err)
	}
	if _, err := c.Open([]byte{9, 1, 2}, ""); !errors.Is(err, ErrDecrypt) {
		t.Errorf("garbage = %v", err)
	}
}

func TestKeys(t *testing.T) {
	k := RandomKey()
	if got, err := ParseKey(base64.StdEncoding.EncodeToString(k)); err != nil || !bytes.Equal(got, k) {
		t.Errorf("base64 = %v", err)
	}
	if _, err := ParseKey("short"); err == nil {
		t.Error("short key accepted")
	}
	path := filepath.Join(t.TempDir(), "sub", "secret.key")
	k1, created, err := LoadOrCreateKeyFile(path)
	if err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	k2, created, err := LoadOrCreateKeyFile(path)
	if err != nil || created || !bytes.Equal(k1, k2) {
		t.Fatalf("reload = %v, %v", created, err)
	}
}

func TestMasker(t *testing.T) {
	var out bytes.Buffer
	m := NewMasker(&out, []string{"s3cr3t-value", "abc", "line-one\nline-two"})
	for _, chunk := range []string{"token=s3cr", "3t-value ok\n", "short abc stays\n", "key: line-one\nkey: line-two\n", "tail s3cr3t-value"} {
		m.Write([]byte(chunk))
	}
	m.Flush()
	want := "token=*** ok\nshort abc stays\nkey: ***\nkey: ***\ntail ***"
	if out.String() != want {
		t.Errorf("masked =\n%q\nwant\n%q", out.String(), want)
	}
	var plain bytes.Buffer
	NewMasker(&plain, nil).Write([]byte("no newline"))
	if plain.String() != "no newline" {
		t.Errorf("passthrough = %q", plain.String())
	}
}
