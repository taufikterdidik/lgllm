package secret

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	box, err := New("this-is-a-long-enough-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := box.Encrypt("sk-super-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, Prefix) {
		t.Errorf("ciphertext = %q, want the %q prefix", enc, Prefix)
	}
	if strings.Contains(enc, "sk-super-secret") {
		t.Fatal("the plaintext is visible in the ciphertext")
	}
	got, err := box.Decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-super-secret" {
		t.Errorf("decrypted = %q", got)
	}
}

func TestPlaintextPassesThrough(t *testing.T) {
	box, _ := New("this-is-a-long-enough-passphrase")
	got, err := box.Decrypt("sk-plain")
	if err != nil || got != "sk-plain" {
		t.Errorf("got %q, err %v", got, err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	a, _ := New("key-one-is-long-enough")
	b, _ := New("key-two-is-also-long-enough")
	enc, _ := a.Encrypt("secret")
	if _, err := b.Decrypt(enc); err == nil {
		t.Fatal("decrypting with another key should fail")
	}
}

func TestMissingKey(t *testing.T) {
	if _, err := New(""); err != ErrNoKey {
		t.Errorf("New(\"\") = %v, want ErrNoKey", err)
	}
	box, _ := New("this-is-a-long-enough-passphrase")
	enc, _ := box.Encrypt("x")
	if _, err := (&Box{}).Decrypt(enc); err == nil {
		t.Error("a zero Box should refuse ciphertext")
	}
}

func TestShortKeyIsRefused(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("a short master key should be refused")
	}
}

func TestExplicitKeyBytes(t *testing.T) {
	// 32 bytes in base64 and hex are accepted as-is.
	b64 := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	if b, err := KeyBytes(b64); err != nil || len(b) != 32 {
		t.Errorf("base64 key: %v %d", err, len(b))
	}
	hexKey := strings.Repeat("ab", 32)
	if b, err := KeyBytes(hexKey); err != nil || len(b) != 32 {
		t.Errorf("hex key: %v %d", err, len(b))
	}
}

func TestNonceIsRandom(t *testing.T) {
	box, _ := New("this-is-a-long-enough-passphrase")
	a, _ := box.Encrypt("same")
	b, _ := box.Encrypt("same")
	if a == b {
		t.Error("two encryptions of the same value are identical")
	}
}

func TestIsEncrypted(t *testing.T) {
	if !IsEncrypted("enc:v1:abc") {
		t.Error("encrypted value not detected")
	}
	if IsEncrypted("sk-plain") {
		t.Error("plaintext misdetected")
	}
}
