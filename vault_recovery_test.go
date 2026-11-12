package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func legacyVaultKey(t *testing.T, s *Store, user string) *ecdh.PrivateKey {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, key.Bytes()); err != nil {
		t.Fatal(err)
	}
	return key
}

func sealCredential(t *testing.T, s *Store, user string, public []byte) (string, []byte) {
	t.Helper()
	secret := []byte("synthetic credential secret")
	aad := []byte("connector:synthetic")
	enc, ct, err := hpkeSealRemote(public, secret, aad)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.StoreSealedCredential(user, "Synthetic", enc, ct, aad)
	if err != nil {
		t.Fatal(err)
	}
	return id, secret
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSyntheticKeyFile(t *testing.T, dir, user string, raw []byte) string {
	t.Helper()
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

// An old database has the private key only in vault_keys and no key file.
// Opening an old credential must use that key and migrate it to the file.
func TestVaultMigratesLegacyDBKeyToFile(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "old.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	oldKey := legacyVaultKey(t, s, user)
	id, secret := sealCredential(t, s, user, oldKey.PublicKey().Bytes())

	keyPath := filepath.Join(dir, ".vault", user+".key")
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key file should not exist before migration: %v", err)
	}
	opened, err := s.OpenSealedCredential(user, id)
	if err != nil {
		t.Fatalf("old DB key should decrypt: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("opened %q, want %q", opened, secret)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("migration should write the key file: %v", err)
	}
	if !bytes.Equal(raw, oldKey.Bytes()) {
		t.Fatal("migrated file must contain the old DB key")
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM vault_keys WHERE user_id=?", user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("migration should remove the database recovery copy after verification")
	}
}

// The key file disappears after a credential was sealed and the new key was
// written only to the file. Opening must refuse to generate a new key, because
// that would make the old ciphertext permanently unreadable.
func TestVaultMissingKeyFileWithCredentialsRefusesNewKey(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "missing.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := sealCredential(t, s, user, public)
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	opened, err := s.OpenSealedCredential(user, id)
	if !errors.Is(err, ErrVaultKeyMissing) {
		t.Fatalf("missing key file with credentials must return ErrVaultKeyMissing, got %v", err)
	}
	if opened != nil {
		t.Fatalf("missing key file must not return plaintext, got %q", opened)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("missing key file must not be replaced by a new key: %v", err)
	}
}

// A corrupt key file is not silently replaced when the legacy database copy
// still has the key; the copy repairs the file.
func TestVaultCorruptKeyFileRecoversFromLegacyDBKey(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "corrupt.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	oldKey := legacyVaultKey(t, s, user)
	id, secret := sealCredential(t, s, user, oldKey.PublicKey().Bytes())

	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := s.OpenSealedCredential(user, id)
	if err != nil {
		t.Fatalf("legacy DB key should repair a corrupt file: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("opened %q, want %q", opened, secret)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, oldKey.Bytes()) {
		t.Fatal("repair must write the legacy key, not a new one")
	}
}

// A corrupt key file with no usable recovery copy must fail, not silently
// generate a key that cannot open existing credentials.
func TestVaultCorruptKeyFileWithoutRecoveryRefusesNewKey(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "corrupt-only.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := sealCredential(t, s, user, public)
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.WriteFile(keyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	opened, err := s.OpenSealedCredential(user, id)
	if !errors.Is(err, ErrVaultKeyInvalid) {
		t.Fatalf("corrupt key file without recovery must return ErrVaultKeyInvalid, got %v", err)
	}
	if opened != nil {
		t.Fatalf("corrupt key file must not return plaintext, got %q", opened)
	}
}

// A restart must keep using the same file key.
func TestVaultRestartKeepsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restart.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := storeUser(t, s)
	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, public)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	opened, err := s2.OpenSealedCredential(user, id)
	if err != nil {
		t.Fatalf("restart should keep the same key: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("opened %q, want %q", opened, secret)
	}
}

// A backup that includes the database and the .vault directory restores.
func TestVaultBackupRestoreWithKeyDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := storeUser(t, s)
	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, public)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	restoreDir := t.TempDir()
	copyFile(t, path, filepath.Join(restoreDir, "backup.sqlite"))
	copyFile(t, filepath.Join(dir, ".vault", user+".key"), filepath.Join(restoreDir, ".vault", user+".key"))

	s2, err := OpenStore(filepath.Join(restoreDir, "backup.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	opened, err := s2.OpenSealedCredential(user, id)
	if err != nil {
		t.Fatalf("backup with .vault should restore: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("opened %q, want %q", opened, secret)
	}
}

// A database-only backup after migration has no key. It must fail clearly
// instead of generating a new key and losing the old credentials.
func TestVaultDatabaseOnlyRestoreRefusesNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dbonly.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := storeUser(t, s)
	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := sealCredential(t, s, user, public)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	restoreDir := t.TempDir()
	copyFile(t, path, filepath.Join(restoreDir, "dbonly.sqlite"))
	s2, err := OpenStore(filepath.Join(restoreDir, "dbonly.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	opened, err := s2.OpenSealedCredential(user, id)
	if !errors.Is(err, ErrVaultKeyMissing) {
		t.Fatalf("database-only restore must return ErrVaultKeyMissing, got %v", err)
	}
	if opened != nil {
		t.Fatalf("database-only restore must not return plaintext, got %q", opened)
	}
}

// A file key and a different legacy database key cannot be resolved
// automatically; the caller must choose which key is correct.
func TestVaultKeyFileAndDatabaseConflict(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "conflict.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	if _, err := s.VaultPublicKey(user); err != nil {
		t.Fatal(err)
	}
	legacyVaultKey(t, s, user)
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyConflict) {
		t.Fatalf("different file and DB keys must return ErrVaultKeyConflict, got %v", err)
	}
	keyPath := filepath.Join(dir, ".vault", user+".key")
	fileRaw, err := os.ReadFile(keyPath)
	if err != nil || len(fileRaw) != 32 {
		t.Fatalf("conflict must keep the file key: %v", err)
	}
	var dbRaw []byte
	if err := s.db.QueryRow("SELECT private FROM vault_keys WHERE user_id=?", user).Scan(&dbRaw); err != nil {
		t.Fatalf("conflict must keep the DB recovery copy: %v", err)
	}
	if len(dbRaw) != 32 {
		t.Fatalf("DB recovery copy was damaged: %d bytes", len(dbRaw))
	}
}

func readVaultDBKey(t *testing.T, s *Store, user string) []byte {
	t.Helper()
	var raw []byte
	if err := s.db.QueryRow("SELECT private FROM vault_keys WHERE user_id=?", user).Scan(&raw); err != nil {
		t.Fatalf("read DB recovery key: %v", err)
	}
	return raw
}

func openSealedWithKey(t *testing.T, s *Store, id string, key *ecdh.PrivateKey) []byte {
	t.Helper()
	var sealed, aad []byte
	if err := s.db.QueryRow("SELECT sealed,aad FROM vault_credentials WHERE id=?", id).Scan(&sealed, &aad); err != nil {
		t.Fatal(err)
	}
	if len(sealed) <= encLength {
		t.Fatal("synthetic credential is too short")
	}
	plain, err := hpkeOpen(key, sealed[:encLength], sealed[encLength:], aad)
	if err != nil {
		t.Fatalf("the original key must still open the credential: %v", err)
	}
	return plain
}

// The exact reported case: the authoritative file key A is unreadable, while
// the legacy database contains a different 32-byte key B. The old code used B,
// replaced A and deleted the database row, losing the only copy of A.
func TestVaultUnreadableKeyFileDoesNotUseDifferentDBKey(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 file")
	}
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "unreadable.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyA.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyB.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(keyPath, 0o600)

	opened, err := s.OpenSealedCredential(user, id)
	if !errors.Is(err, ErrVaultKeyInvalid) {
		t.Fatalf("unreadable authoritative file must return ErrVaultKeyInvalid, got %v", err)
	}
	if opened != nil {
		t.Fatalf("unreadable file must not return plaintext, got %q", opened)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, keyA.Bytes()) {
		t.Fatal("unreadable file key A was overwritten by database key B")
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyB.Bytes()) {
		t.Fatal("database recovery key B was deleted")
	}
	if plain := openSealedWithKey(t, s, id, keyA); !bytes.Equal(plain, secret) {
		t.Fatalf("original key A no longer opens the credential: %q", plain)
	}
}

// A directory at the key path is another deterministic unreadable-file case,
// with no dependency on process permissions.
func TestVaultKeyPathDirectoryBlocksWithoutUsingDBKey(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "keydir.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.MkdirAll(keyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyB.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyInvalid) {
		t.Fatalf("directory at key path must return ErrVaultKeyInvalid, got %v", err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyB.Bytes()) {
		t.Fatal("database recovery key B was deleted")
	}
	info, err := os.Stat(keyPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("key path directory must remain untouched: %v", err)
	}
}

// A 32-byte database key is not proof that it is the right key. If it cannot
// open the existing ciphertext, it must not be migrated or deleted.
func TestVaultWrongDBKeyDoesNotMigrateOrDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "wrongkey.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyB.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyMismatch) {
		t.Fatalf("wrong 32-byte DB key must return ErrVaultKeyMismatch, got %v", err)
	}
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("wrong DB key must not create a key file: %v", err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyB.Bytes()) {
		t.Fatal("wrong DB key must remain available for manual recovery")
	}
	if plain := openSealedWithKey(t, s, id, keyA); !bytes.Equal(plain, secret) {
		t.Fatalf("original key A no longer opens the credential: %q", plain)
	}
}

// A corrupt file plus a wrong database key must not overwrite the file.
func TestVaultCorruptFileWithWrongDBKeyDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "corrupt-wrong.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyB.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyMismatch) {
		t.Fatalf("wrong DB key must not repair a corrupt file: %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, []byte("short")) {
		t.Fatalf("corrupt file must remain untouched: %q %v", raw, err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyB.Bytes()) {
		t.Fatal("database recovery key B was deleted")
	}
	if plain := openSealedWithKey(t, s, id, keyA); !bytes.Equal(plain, secret) {
		t.Fatalf("original key A no longer opens the credential: %q", plain)
	}
}

// A corrupt legacy row is not a usable 32-byte key. It must not be replaced by
// a newly generated key, and it must not be deleted.
func TestVaultCorruptDBKeyRecordDoesNotGenerateNewKey(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "corrupt-record.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealCredential(t, s, user, keyA.PublicKey().Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, []byte("short")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyInvalid) {
		t.Fatalf("corrupt DB row must return ErrVaultKeyInvalid, got %v", err)
	}
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt DB row must not create a new key file: %v", err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), []byte("short")) {
		t.Fatal("corrupt DB row must remain for inspection")
	}
}

// The file and the database may hold the same key, but that key still has to
// open the existing ciphertext. A correct byte length is not proof that the
// existing credentials are usable, and the recovery copy must be kept.
func TestVaultSameKeyButCredentialMismatchReturnsErrorAndKeepsDBRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "same-wrong.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyOther, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyOther.PublicKey().Bytes())
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyA.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyA.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyMismatch) {
		t.Fatalf("a 32-byte key that opens no credential must return ErrVaultKeyMismatch, got %v", err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyA.Bytes()) {
		t.Fatal("unverified database recovery copy must not be deleted")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyA.Bytes()) {
		t.Fatalf("file key must remain untouched: %q %v", raw, err)
	}
	if plain := openSealedWithKey(t, s, id, keyOther); !bytes.Equal(plain, secret) {
		t.Fatalf("the credential's original key must still open it: %q", plain)
	}
}

// One corrupt credential is enough to stop a migration: the database key has
// not been verified against all existing ciphertext.
func TestVaultOneCorruptCredentialBlocksMigration(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "one-corrupt.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealCredential(t, s, user, keyA.PublicKey().Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_credentials(id,user_id,label,sealed,aad,at) VALUES(?,?,?,?,?,?)",
		"corrupt-credential", user, "Corrupt", []byte("short"), []byte("connector:synthetic"), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyA.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultCredentialCorrupt) {
		t.Fatalf("a truncated credential must return ErrVaultCredentialCorrupt, got %v", err)
	}
	keyPath := filepath.Join(dir, ".vault", user+".key")
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("blocked migration must not create a key file: %v", err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyA.Bytes()) {
		t.Fatal("database recovery key must remain after a blocked migration")
	}
}

// This blocks creation of the .vault directory, so the failure happens before
// any key file is written. It does not simulate a partial write, power loss, or
// a crash after the file is in place.
func TestVaultPreWriteFailureKeepsDBAndRestartRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "midfail.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyA.Bytes()); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, ".vault")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); err == nil {
		t.Fatal("migration should fail when the key directory cannot be created")
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyA.Bytes()) {
		t.Fatal("database recovery key must survive a failed migration")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	opened, err := s2.OpenSealedCredential(user, id)
	if err != nil {
		t.Fatalf("restart should finish the migration: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Fatalf("opened %q, want %q", opened, secret)
	}
	var rows int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM vault_keys WHERE user_id=?", user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("successful restart migration should remove the database copy after verification")
	}
}

// A 32-byte file is not proof that it is the key for the existing credentials.
// A wrong file key must be reported, not returned as a usable public key.
func TestVaultWrong32ByteFileDoesNotReportSuccess(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "wrong-file.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := writeSyntheticKeyFile(t, dir, user, keyB.Bytes())

	if _, err := s.VaultPublicKey(user); !errors.Is(err, ErrVaultKeyMismatch) {
		t.Fatalf("wrong 32-byte file key must return ErrVaultKeyMismatch, got %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyB.Bytes()) {
		t.Fatalf("wrong file key must remain untouched: %q %v", raw, err)
	}
	if plain := openSealedWithKey(t, s, id, keyA); !bytes.Equal(plain, secret) {
		t.Fatalf("the credential's original key must still open it: %q", plain)
	}
}

// The file and the database can both hold the same wrong 32-byte key. Matching
// byte length and matching copies still do not prove that the key opens the
// existing credentials.
func TestVaultSameWrongFileAndDBKeyDoesNotReportSuccess(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "same-wrong-file-db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := writeSyntheticKeyFile(t, dir, user, keyB.Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyB.Bytes()); err != nil {
		t.Fatal(err)
	}

	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyMismatch) {
		t.Fatalf("matching wrong file and DB keys must return ErrVaultKeyMismatch, got %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyB.Bytes()) {
		t.Fatalf("file key must remain untouched: %q %v", raw, err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyB.Bytes()) {
		t.Fatal("database recovery copy must remain untouched")
	}
	if plain := openSealedWithKey(t, s, id, keyA); !bytes.Equal(plain, secret) {
		t.Fatalf("the credential's original key must still open it: %q", plain)
	}
}

// A truncated credential is a data-integrity error, not proof that the file
// key is wrong. It must be reported clearly and must not delete the key.
func TestVaultPartialCredentialCorruptionReturnsClearError(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "partial-corrupt.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealCredential(t, s, user, keyA.PublicKey().Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_credentials(id,user_id,label,sealed,aad,at) VALUES(?,?,?,?,?,?)",
		"truncated-credential", user, "Truncated", []byte("short"), []byte("connector:synthetic"), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	keyPath := writeSyntheticKeyFile(t, dir, user, keyA.Bytes())

	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultCredentialCorrupt) {
		t.Fatalf("a truncated credential must return ErrVaultCredentialCorrupt, got %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyA.Bytes()) {
		t.Fatalf("file key must remain untouched: %q %v", raw, err)
	}
}

// A brand-new user with no credentials still gets a key without any legacy
// material to verify.
func TestVaultNoCredentialsNewUserCreatesKey(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "new-user.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)

	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatalf("a new user with no credentials should get a key: %v", err)
	}
	if len(public) != encLength {
		t.Fatalf("public key length = %d, want %d", len(public), encLength)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".vault", user+".key"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("new user key file must contain 32 bytes: %d %v", len(raw), err)
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM vault_keys WHERE user_id=?", user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("a new key must not be written to the legacy database table")
	}
}

// A legacy row that is exactly the file key plus trailing whitespace contains
// no unique key material. It can be removed after the file key has opened every
// credential.
func TestVaultLegacyKeyPlusNewlineIsSafelyCleaned(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "key-newline.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	writeSyntheticKeyFile(t, dir, user, keyA.Bytes())
	dbRaw := append(append([]byte{}, keyA.Bytes()...), '\n')
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, dbRaw); err != nil {
		t.Fatal(err)
	}

	key, err := s.vaultPrivateKey(user)
	if err != nil {
		t.Fatalf("the same key with a trailing newline should be safely cleaned: %v", err)
	}
	if !bytes.Equal(key.Bytes(), keyA.Bytes()) {
		t.Fatal("the file key must be returned")
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM vault_keys WHERE user_id=?", user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("a safely identified duplicate legacy row should be removed")
	}
	opened, err := s.OpenSealedCredential(user, id)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("the credential must still open after cleanup: %q %v", opened, err)
	}
}

// A legacy row whose first 32 bytes are a different key cannot be proven safe
// to delete. It must be kept and reported as an incomplete migration.
func TestVaultUnverifiableLegacyRowReturnsIncomplete(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "unverifiable-row.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := writeSyntheticKeyFile(t, dir, user, keyA.Bytes())
	dbRaw := append(append([]byte{}, keyB.Bytes()...), '\n')
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, dbRaw); err != nil {
		t.Fatal(err)
	}

	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultLegacyKeyUnverified) {
		t.Fatalf("an unverifiable legacy row must return ErrVaultLegacyKeyUnverified, got %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyA.Bytes()) {
		t.Fatalf("file key must remain untouched: %q %v", raw, err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), dbRaw) {
		t.Fatal("unverifiable legacy row must remain for inspection")
	}
}

// A short legacy row is not a usable key and cannot be proven to be a duplicate
// of the file key. It must be kept and reported, not silently ignored.
func TestVaultCorruptLegacyRowReturnsIncomplete(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "short-legacy-row.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := writeSyntheticKeyFile(t, dir, user, keyA.Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, []byte("short")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultLegacyKeyUnverified) {
		t.Fatalf("a short legacy row must return ErrVaultLegacyKeyUnverified, got %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyA.Bytes()) {
		t.Fatalf("file key must remain untouched: %q %v", raw, err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), []byte("short")) {
		t.Fatal("short legacy row must remain for inspection")
	}
}

// Credential verification comes before legacy-row cleanup. A wrong file key
// must be reported even when the legacy row is also unverifiable.
func TestVaultWrongFileAndCorruptLegacyRowKeepsBoth(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "wrong-file-short-row.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	keyPath := writeSyntheticKeyFile(t, dir, user, keyB.Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, []byte("short")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultKeyMismatch) {
		t.Fatalf("a wrong file key must return ErrVaultKeyMismatch, got %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyB.Bytes()) {
		t.Fatalf("file key must remain untouched: %q %v", raw, err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), []byte("short")) {
		t.Fatal("legacy row must remain for inspection")
	}
	if plain := openSealedWithKey(t, s, id, keyA); !bytes.Equal(plain, secret) {
		t.Fatalf("the credential's original key must still open it: %q", plain)
	}
}

// An incomplete legacy state must survive a restart, not become a silent
// success after the process reopens the database.
func TestVaultIncompleteLegacyStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incomplete-restart.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealCredential(t, s, user, keyA.PublicKey().Bytes())
	writeSyntheticKeyFile(t, dir, user, keyA.Bytes())
	dbRaw := append(append([]byte{}, keyB.Bytes()...), '\n')
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, dbRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vaultPrivateKey(user); !errors.Is(err, ErrVaultLegacyKeyUnverified) {
		t.Fatalf("first call must report the incomplete migration, got %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.vaultPrivateKey(user); !errors.Is(err, ErrVaultLegacyKeyUnverified) {
		t.Fatalf("restart must still report the incomplete migration, got %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".vault", user+".key"))
	if err != nil || !bytes.Equal(raw, keyA.Bytes()) {
		t.Fatalf("file key must survive restart: %q %v", raw, err)
	}
	if !bytes.Equal(readVaultDBKey(t, s2, user), dbRaw) {
		t.Fatal("legacy row must survive restart")
	}
}

// The key file can be written successfully and the database delete can then
// fail. The recovery row must stay, and a later restart must be able to finish.
func TestVaultFileWrittenButDBDeleteFailureKeepsRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "delete-failure.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := storeUser(t, s)
	keyA, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := sealCredential(t, s, user, keyA.PublicKey().Bytes())
	if _, err := s.db.Exec("INSERT INTO vault_keys(user_id,private) VALUES(?,?)", user, keyA.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER block_vault_key_delete BEFORE DELETE ON vault_keys BEGIN SELECT RAISE(ABORT, 'synthetic delete failure'); END;"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.vaultPrivateKey(user); err == nil {
		t.Fatal("a failed database delete must surface as an error")
	}
	keyPath := filepath.Join(dir, ".vault", user+".key")
	raw, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(raw, keyA.Bytes()) {
		t.Fatalf("the key file should have been written before the delete failed: %q %v", raw, err)
	}
	if !bytes.Equal(readVaultDBKey(t, s, user), keyA.Bytes()) {
		t.Fatal("the database recovery row must survive a failed delete")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.db.Exec("DROP TRIGGER block_vault_key_delete"); err != nil {
		t.Fatal(err)
	}
	opened, err := s2.OpenSealedCredential(user, id)
	if err != nil || !bytes.Equal(opened, secret) {
		t.Fatalf("restart after the delete failure should finish migration: %q %v", opened, err)
	}
	var rows int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM vault_keys WHERE user_id=?", user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("the recovery row should be removed after a successful retry")
	}
}
