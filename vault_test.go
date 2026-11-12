package main

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// CFRG 官方测试向量：HPKE base, DHKEM(X25519,HKDF-SHA256), HKDF-SHA256,
// ChaCha20-Poly1305（RFC 9180 附录 A.2，KEM=0x20 KDF=0x1 AEAD=0x3）。
const vectorIkmR = "1ac01f181fdf9f352797655161c58b75c656a6cc2716dcb66372da835542e1df"
const vectorIkmE = "909a9b35d3dc4713a5e72a4da274b55d3d3821a37e5d099e74a647db583a904b"
const vectorPkRm = "4310ee97d88cc1f088a5576c77ab0cf5c3ac797f3d95139c6c84b5429c59662a"
const vectorPkEm = "1afa08d3dec047a643885163f1180476fa7ddb54c6a8029ea33f95796bf2ac4a"
const vectorShared = "0bbe78490412b4bbea4812666f7916932b828bba79942424abb65244930d69a7"
const vectorKey = "ad2744de8e17f4ebba575b3f5f5a8fa1f69c2a07f6e7500bc60ca6e3e3ec1c91"
const vectorNonce = "5c4d98150661b848853b547f"
const vectorAAD = "436f756e742d30"
const vectorPT = "4265617574792069732074727574682c20747275746820626561757479"
const vectorCT = "1c5250d8034ec2b784ba2cfd69dbdb8af406cfe3ff938e131f0def8c8b60b4db21993c62ce81883d2dd1b51a28"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The RFC vector uses its own info; the vault binds "Onit vault credential",
// so the vector runs through the same schedule with the RFC info, proving the
// pipeline, before the vault-specific round-trip below.

func TestVaultHPKEMatchesRFCVector(t *testing.T) {
	skR, err := deriveKeyPair(mustHex(t, vectorIkmR))
	if err != nil {
		t.Fatal(err)
	}
	if pk := skR.PublicKey().Bytes(); hex.EncodeToString(pk) != vectorPkRm {
		t.Fatalf("pkRm mismatch: %x", pk)
	}
	enc, shared, err := encapsulate(skR.PublicKey(), mustHex(t, vectorIkmE))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(enc) != vectorPkEm {
		t.Fatalf("enc mismatch: %x", enc)
	}
	if hex.EncodeToString(shared) != vectorShared {
		t.Fatalf("shared_secret mismatch: %x", shared)
	}
	session, err := keySchedule(shared, mustHex(t, "4f6465206f6e2061204772656369616e2055726e"))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(session.key) != vectorKey {
		t.Fatalf("key mismatch: %x", session.key)
	}
	if hex.EncodeToString(session.baseNonce) != vectorNonce {
		t.Fatalf("base_nonce mismatch: %x", session.baseNonce)
	}
	ct, err := session.aead(0, mustHex(t, vectorAAD), mustHex(t, vectorPT))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(ct) != vectorCT {
		t.Fatalf("ciphertext mismatch: %x", ct)
	}
	pt, err := session.open(0, mustHex(t, vectorAAD), ct)
	if err != nil || !bytes.Equal(pt, mustHex(t, vectorPT)) {
		t.Fatalf("open mismatch: %v %x", err, pt)
	}
	decapped, err := decapsulate(skR, mustHex(t, vectorPkEm))
	if err != nil || !bytes.Equal(decapped, shared) {
		t.Fatalf("decapsulate mismatch: %v", err)
	}
}

func TestVaultSealOpenRoundTrip(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"email":"user@example.com","password":"hunter2"}`)
	aad := []byte("connector:monarch")
	enc, ct, err := hpkeSeal(key.PublicKey(), plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plaintext) || bytes.Contains(enc, plaintext) {
		t.Fatal("plaintext leaked into sealed output")
	}
	opened, err := hpkeOpen(key, enc, ct, aad)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("round trip failed: %v", err)
	}
	wrong, err := hpkeOpen(key, enc, ct, []byte("connector:hevy"))
	if err == nil || wrong != nil {
		t.Fatal("aad change must reject")
	}
}

func TestVaultCredentialStaysSealedAtRest(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "vault.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	if len(public) != 32 {
		t.Fatal("X25519 public key must be 32 bytes")
	}
	secret := []byte("monarch api key 0123456789")
	enc, ct, err := hpkeSealRemote(public, secret, []byte("connector:monarch"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.StoreSealedCredential(user, "Monarch", enc, ct, []byte("connector:monarch"))
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err = s.db.QueryRow("SELECT sealed FROM vault_credentials WHERE id=?", id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("database stores plaintext")
	}
	opened, err := s.OpenSealedCredential(user, id)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("vault open failed: %v %s", err, opened)
	}
	if _, err = s.OpenSealedCredential(user, "missing"); err != ErrNotFound {
		t.Fatalf("unknown credential must be ErrNotFound, got %v", err)
	}
}

func TestVaultHTTPEndpoints(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "vault-http.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	token := issue(t, s)
	user, _ := s.User(token)
	key := unpack[object](t, apiCall(t, server.URL, token, "GET", "/v1/vault", nil, 200))
	public, err := base64.StdEncoding.DecodeString(str(key, "publicKey"))
	if err != nil || len(public) != 32 {
		t.Fatalf("public key: %v", err)
	}
	secret := []byte(`{"api_key":"sk-live-abc123"}`)
	aad := []byte("connector:hevy")
	enc, ct, err := hpkeSealRemote(public, secret, aad)
	if err != nil {
		t.Fatal(err)
	}
	in := object{"label": "Hevy", "enc": base64.StdEncoding.EncodeToString(enc),
		"ct": base64.StdEncoding.EncodeToString(ct), "aad": base64.StdEncoding.EncodeToString(aad)}
	stored := unpack[object](t, apiCall(t, server.URL, token, "POST", "/v1/vault/credentials", in, 200))
	opened, err := s.OpenSealedCredential(user, str(stored, "id"))
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("http-stored credential did not open: %v %s", err, opened)
	}
	// 原版 deleteAccount 的凭据侧（F-076）：删掉后不可再解封，重复删除 404。
	apiCall(t, server.URL, token, "DELETE", "/v1/vault/credentials/"+str(stored, "id"), nil, 200)
	if _, err = s.OpenSealedCredential(user, str(stored, "id")); err != ErrNotFound {
		t.Fatalf("deleted credential must be gone, got %v", err)
	}
	apiCall(t, server.URL, token, "DELETE", "/v1/vault/credentials/"+str(stored, "id"), nil, 404)
}

func TestVaultKeyStoredInSeparateFile(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "vault-sep.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)

	// 触发私钥生成
	_, err = s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}

	// 私钥文件应在 <dir>/.vault/<user>.key
	keyPath := filepath.Join(dir, ".vault", user+".key")
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("private key file should exist at %s: %v", keyPath, err)
	}
	if len(raw) != 32 {
		t.Fatalf("private key should be 32 bytes, got %d", len(raw))
	}

	// 再次获取应该是同一把钥匙
	key2, err := s.vaultPrivateKey(user)
	if err != nil {
		t.Fatal(err)
	}
	if string(key2.Bytes()) != string(raw) {
		t.Fatal("private key should be stable across reads")
	}

	// 新钥匙只写文件，不再写 SQLite。
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM vault_keys WHERE user_id=?", user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("new keys must not be written to SQLite")
	}

	// 没有凭据时，删除文件后重新生成是安全的；有凭据时由恢复测试保证
	// 不会静默换钥匙。
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	key3, err := s.vaultPrivateKey(user)
	if err != nil {
		t.Fatal(err)
	}
	if string(key3.Bytes()) == string(raw) {
		t.Fatal("deleted key file with no credentials should result in a new key")
	}
}
