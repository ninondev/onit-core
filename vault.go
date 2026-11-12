package main

// 连接器凭据的 HPKE 封装（F-067）：RFC 9180 基础模式，
// DHKEM(X25519, HKDF-SHA256) + HKDF-SHA256 + ChaCha20-Poly1305，
// info 绑定 "Onit vault credential"。
//
// 信任边界（如实）：凭据明文不入库（磁盘上只有密文）；但服务端持有
// 解封装私钥（.vault 密钥文件）。旧版数据库的 vault_keys 表会在首次
// 读取时尝试迁移：只有钥匙打开全部现有密文、文件写回校验一致，且旧行
// 可证明是同一把钥匙（或只多出尾部空白）时才删除。无法验证的旧行会
// 保留并返回明确错误，数据库文件仍可能含私钥；旧备份也可能仍含
// vault_keys。自部署运营者可解密所有凭据。这是「传输与静态存储加密」，
// 不是「服务端不可解密的端到端加密」。

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	hpkeKEMID  = uint16(32)
	hpkeKDFID  = uint16(1)
	hpkeAEADID = uint16(3)
	vaultInfo  = "Onit vault credential"
	encLength  = 32 // X25519 public key
)

var kemSuiteID = concat([]byte("KEM"), i2osp2(hpkeKEMID))
var hpkeSuiteID = concat([]byte("HPKE"), i2osp2(hpkeKEMID), i2osp2(hpkeKDFID), i2osp2(hpkeAEADID))

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}
func i2osp2(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }

func labeledExtract(salt, label, ikm, suiteID []byte) ([]byte, error) {
	labeled := concat([]byte("HPKE-v1"), suiteID, label, ikm)
	return hkdf.Extract(sha256.New, labeled, salt)
}
func labeledExpand(prk, label, info, suiteID []byte, length int) ([]byte, error) {
	labeled := concat(i2osp2(uint16(length)), []byte("HPKE-v1"), suiteID, label, info)
	return hkdf.Expand(sha256.New, prk, string(labeled), length)
}

// DeriveKeyPair (RFC 9180 §7.1.3, X25519)
func deriveKeyPair(ikm []byte) (*ecdh.PrivateKey, error) {
	dkpPrk, err := labeledExtract(nil, []byte("dkp_prk"), ikm, kemSuiteID)
	if err != nil {
		return nil, err
	}
	sk, err := labeledExpand(dkpPrk, []byte("sk"), nil, kemSuiteID, 32)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(sk)
}

func encapsulate(recipient *ecdh.PublicKey, ikmE []byte) (enc, shared []byte, err error) {
	eph, err := deriveKeyPair(ikmE)
	if err != nil {
		return nil, nil, err
	}
	dh, err := eph.ECDH(recipient)
	if err != nil {
		return nil, nil, err
	}
	enc = eph.PublicKey().Bytes()
	eae, err := labeledExtract(nil, []byte("eae_prk"), dh, kemSuiteID)
	if err != nil {
		return nil, nil, err
	}
	context := concat(enc, recipient.Bytes())
	shared, err = labeledExpand(eae, []byte("shared_secret"), context, kemSuiteID, 32)
	return enc, shared, err
}

func decapsulate(recipient *ecdh.PrivateKey, enc []byte) ([]byte, error) {
	if len(enc) != encLength {
		return nil, ErrInvalid
	}
	peer, err := ecdh.X25519().NewPublicKey(enc)
	if err != nil {
		return nil, err
	}
	dh, err := recipient.ECDH(peer)
	if err != nil {
		return nil, err
	}
	eae, err := labeledExtract(nil, []byte("eae_prk"), dh, kemSuiteID)
	if err != nil {
		return nil, err
	}
	context := concat(enc, recipient.PublicKey().Bytes())
	return labeledExpand(eae, []byte("shared_secret"), context, kemSuiteID, 32)
}

type hpkeSession struct{ key, baseNonce []byte }

func keySchedule(shared, info []byte) (*hpkeSession, error) {
	pskIDHash, err := labeledExtract(nil, []byte("psk_id_hash"), nil, hpkeSuiteID)
	if err != nil {
		return nil, err
	}
	infoHash, err := labeledExtract(nil, []byte("info_hash"), info, hpkeSuiteID)
	if err != nil {
		return nil, err
	}
	context := concat([]byte{0}, pskIDHash, infoHash)                         // mode 0 (base); suite id lives in the labels, not the context
	secret, err := labeledExtract(shared, []byte("secret"), nil, hpkeSuiteID) // base mode: psk is empty
	if err != nil {
		return nil, err
	}
	key, err := labeledExpand(secret, []byte("key"), context, hpkeSuiteID, 32)
	if err != nil {
		return nil, err
	}
	nonce, err := labeledExpand(secret, []byte("base_nonce"), context, hpkeSuiteID, 12)
	if err != nil {
		return nil, err
	}
	return &hpkeSession{key: key, baseNonce: nonce}, nil
}

func (s *hpkeSession) aead(seq uint64, aad, plaintext []byte) ([]byte, error) {
	box, err := chacha20poly1305.New(s.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	for i := range nonce {
		nonce[i] ^= s.baseNonce[i]
	}
	if len(plaintext) == 0 {
		return nil, errors.New("empty plaintext")
	}
	return box.Seal(nil, nonce, plaintext, aad), nil
}

func (s *hpkeSession) open(seq uint64, aad, ct []byte) ([]byte, error) {
	box, err := chacha20poly1305.New(s.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], seq)
	for i := range nonce {
		nonce[i] ^= s.baseNonce[i]
	}
	return box.Open(nil, nonce, ct, aad)
}

// hpkeSealWithSeed 让发送方用确定的临时种子，供 RFC 向量逐位校验。
func hpkeSealWithSeed(recipient *ecdh.PublicKey, ikmE, plaintext, aad []byte) (enc, ct []byte, err error) {
	enc, shared, err := encapsulate(recipient, ikmE)
	if err != nil {
		return nil, nil, err
	}
	session, err := keySchedule(shared, []byte(vaultInfo))
	if err != nil {
		return nil, nil, err
	}
	ct, err = session.aead(0, aad, plaintext)
	return enc, ct, err
}

func hpkeSeal(recipient *ecdh.PublicKey, plaintext, aad []byte) (enc, ct []byte, err error) {
	seed := make([]byte, 32)
	if _, err = rand.Read(seed); err != nil {
		return nil, nil, err
	}
	return hpkeSealWithSeed(recipient, seed, plaintext, aad)
}

// hpkeSealRemote 封装到原始公钥字节（远程/跨进程发送方使用）。
func hpkeSealRemote(recipientPublic, plaintext, aad []byte) (enc, ct []byte, err error) {
	if len(recipientPublic) != encLength {
		return nil, nil, ErrInvalid
	}
	public, err := ecdh.X25519().NewPublicKey(recipientPublic)
	if err != nil {
		return nil, nil, err
	}
	return hpkeSeal(public, plaintext, aad)
}

func hpkeOpen(recipient *ecdh.PrivateKey, enc, ct, aad []byte) ([]byte, error) {
	shared, err := decapsulate(recipient, enc)
	if err != nil {
		return nil, err
	}
	session, err := keySchedule(shared, []byte(vaultInfo))
	if err != nil {
		return nil, err
	}
	return session.open(0, aad, ct)
}

const vaultSchema = `
CREATE TABLE IF NOT EXISTS vault_keys (
 user_id TEXT PRIMARY KEY REFERENCES users(id),
 private BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS vault_credentials (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
 label TEXT NOT NULL, sealed BLOB NOT NULL, aad BLOB NOT NULL, at TEXT NOT NULL
);`

var (
	ErrVaultKeyMissing          = errors.New("vault private key is missing")
	ErrVaultKeyInvalid          = errors.New("vault private key is invalid")
	ErrVaultKeyConflict         = errors.New("vault key file and database recovery copy differ")
	ErrVaultKeyMismatch         = errors.New("vault key does not open existing credentials")
	ErrVaultCredentialCorrupt   = errors.New("vault credential ciphertext is corrupt")
	ErrVaultLegacyKeyUnverified = errors.New("vault key file is valid but the legacy database key cannot be verified or safely removed")
)

// vaultKeyMu serializes key migration and creation inside this process.
var vaultKeyMu sync.Mutex

// vaultPrivateKey 获取或生成用户的 X25519 私钥。
// 私钥文件 <db_path>.vault/<user_id>.key 是权威来源，权限 0600。
// 文件存在时，钥匙必须先打开全部现有凭据才可用；32 字节长度本身不是
// 可用性的证明。旧版数据库可能只在 vault_keys 表里有私钥；首次读取时
// 迁移到文件，但只有在该钥匙确实打开全部现有密文、文件写回校验通过，
// 且旧行可证明是同一把钥匙（或只多出尾部空白）后才删除数据库副本。
// 无法验证的旧行会保留并返回 ErrVaultLegacyKeyUnverified，不能静默
// 宣称数据库已经不含私钥。文件存在但不可读时直接阻断，绝不拿数据库里
// 的另一把 32 字节钥匙覆盖它。文件缺失且没有可用数据库副本、而该用户
// 已有凭据时返回错误而不是生成新钥匙，避免旧密文永久无法解密。
// 迁移完成前，数据库文件本身仍可能包含私钥；旧备份也可能仍含
// vault_keys；不要声称“拿到数据库文件的人一定没有私钥”。
func (s *Store) vaultPrivateKey(user string) (*ecdh.PrivateKey, error) {
	vaultKeyMu.Lock()
	defer vaultKeyMu.Unlock()
	keyPath := s.vaultKeyPath(user)

	fileRaw, fileErr := os.ReadFile(keyPath)
	fileMissing := os.IsNotExist(fileErr)
	fileValid := fileErr == nil && len(fileRaw) == 32
	fileUnreadable := fileErr != nil && !fileMissing
	fileCorrupt := fileErr == nil && !fileValid

	// An existing file that cannot be read is authoritative. The database
	// copy may be a different key; replacing the file would destroy the only
	// copy of the key that existing ciphertext may need. Do not even read the
	// database recovery row on this path.
	if fileUnreadable {
		return nil, ErrVaultKeyInvalid
	}

	dbRaw, dbPresent, dbErr := s.vaultKeyFromDB(user)
	if dbErr != nil {
		return nil, dbErr
	}
	dbValid := dbPresent && len(dbRaw) == 32

	switch {
	case fileValid && dbValid:
		if !bytes.Equal(fileRaw, dbRaw) {
			return nil, ErrVaultKeyConflict
		}
		key, err := ecdh.X25519().NewPrivateKey(fileRaw)
		if err != nil {
			return nil, ErrVaultKeyInvalid
		}
		// A 32-byte key is not proof that it opens the existing credentials.
		// Verify before deleting the database recovery copy; on any failure,
		// keep both the file and the recovery row.
		if err := s.vaultKeyOpensCredentials(user, key); err != nil {
			return nil, err
		}
		if err := s.deleteVaultKeyFromDB(user); err != nil {
			return nil, err
		}
		return key, nil
	case fileValid:
		key, err := ecdh.X25519().NewPrivateKey(fileRaw)
		if err != nil {
			return nil, ErrVaultKeyInvalid
		}
		// The file is authoritative, but it still has to open every existing
		// credential. A correct byte length alone is not a usable vault.
		if err := s.vaultKeyOpensCredentials(user, key); err != nil {
			return nil, err
		}
		if !dbPresent {
			return key, nil
		}
		// dbPresent is true and dbValid is false here. The row is not a usable
		// 32-byte key. Remove it only when it is provably the same key with
		// trailing whitespace; otherwise keep it and report the incomplete
		// migration instead of silently claiming the database is clean.
		if !legacyKeyIsFileKeyWithWhitespace(dbRaw, fileRaw) {
			return nil, ErrVaultLegacyKeyUnverified
		}
		if err := s.deleteVaultKeyFromDB(user); err != nil {
			return nil, err
		}
		return key, nil
	case fileCorrupt:
		if !dbValid {
			// Do not replace an existing file with a newly generated key.
			return nil, ErrVaultKeyInvalid
		}
		return s.migrateVaultKey(user, keyPath, dbRaw)
	case fileMissing:
		if dbPresent && !dbValid {
			// A corrupt recovery row is evidence. Do not delete it and do not
			// generate a new key over it.
			return nil, ErrVaultKeyInvalid
		}
		if dbValid {
			return s.migrateVaultKey(user, keyPath, dbRaw)
		}
		// No key material at all. Generate only when there is nothing to lose.
		count, err := s.vaultCredentialCount(user)
		if err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, ErrVaultKeyMissing
		}
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		if err := writeVaultKeyFile(keyPath, key.Bytes()); err != nil {
			return nil, err
		}
		return key, nil
	default:
		// Unreachable: a read error is either missing or unreadable, and a
		// successful read is either valid or corrupt.
		return nil, ErrVaultKeyInvalid
	}
}

// legacyKeyIsFileKeyWithWhitespace reports whether a legacy row contains the
// same 32-byte key as the authoritative file plus only trailing ASCII
// whitespace. Such a row has no unique key material and can be removed after
// the file key has opened every credential. Any other non-32-byte row is
// unverifiable and must be kept for inspection.
func legacyKeyIsFileKeyWithWhitespace(dbRaw, fileRaw []byte) bool {
	if len(dbRaw) < 32 || !bytes.Equal(dbRaw[:32], fileRaw) {
		return false
	}
	for _, b := range dbRaw[32:] {
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}

// migrateVaultKey moves a legacy database key to the authoritative file.
// The database copy is not deleted until the key has opened every existing
// credential and the file has been read back byte-for-byte.
func (s *Store) migrateVaultKey(user, keyPath string, dbRaw []byte) (*ecdh.PrivateKey, error) {
	key, err := ecdh.X25519().NewPrivateKey(dbRaw)
	if err != nil {
		return nil, ErrVaultKeyInvalid
	}
	if err := s.vaultKeyOpensCredentials(user, key); err != nil {
		// The database key is 32 bytes but does not open every existing
		// credential. Keep the recovery row and any existing file.
		return nil, err
	}
	if err := writeVaultKeyFile(keyPath, dbRaw); err != nil {
		return nil, err
	}
	check, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(check, dbRaw) {
		return nil, ErrVaultKeyInvalid
	}
	if err := s.deleteVaultKeyFromDB(user); err != nil {
		return nil, err
	}
	return key, nil
}

// vaultKeyOpensCredentials verifies a candidate key against every stored
// credential for the user. No credentials means there is nothing to verify.
// A truncated or malformed row is reported as corrupt, separately from a key
// that simply does not open the ciphertext.
func (s *Store) vaultKeyOpensCredentials(user string, key *ecdh.PrivateKey) error {
	rows, err := s.db.Query("SELECT sealed,aad FROM vault_credentials WHERE user_id=?", user)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sealed, aad []byte
		if err := rows.Scan(&sealed, &aad); err != nil {
			return err
		}
		if len(sealed) <= encLength {
			return ErrVaultCredentialCorrupt
		}
		if _, err := hpkeOpen(key, sealed[:encLength], sealed[encLength:], aad); err != nil {
			return ErrVaultKeyMismatch
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func (s *Store) vaultKeyFromDB(user string) ([]byte, bool, error) {
	var raw []byte
	err := s.db.QueryRow("SELECT private FROM vault_keys WHERE user_id=?", user).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (s *Store) vaultCredentialCount(user string) (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM vault_credentials WHERE user_id=?", user).Scan(&count)
	return count, err
}

func (s *Store) deleteVaultKeyFromDB(user string) error {
	return s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM vault_keys WHERE user_id=?", user)
		return err
	})
}

// writeVaultKeyFile writes the key to a fresh 0600 file and renames it into
// place, so a crash cannot leave a half-written key.
func writeVaultKeyFile(path string, raw []byte) error {
	if len(raw) != 32 {
		return ErrVaultKeyInvalid
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// vaultKeyPath 返回用户私钥文件的路径（与数据库同目录、独立子目录）。
func (s *Store) vaultKeyPath(user string) string {
	return filepath.Join(filepath.Dir(s.dbPath), ".vault", user+".key")
}

func (s *Store) VaultPublicKey(user string) ([]byte, error) {
	key, err := s.vaultPrivateKey(user)
	if err != nil {
		return nil, err
	}
	return key.PublicKey().Bytes(), nil
}

// StoreSealedCredential 只保存密文（enc||ct）；明文仅在 OpenSealedCredential
// 的调用现场出现。
func (s *Store) StoreSealedCredential(user, label string, enc, ct, aad []byte) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 80 || len(enc) != encLength || len(ct) == 0 || len(ct) > 1<<16 {
		return "", ErrInvalid
	}
	id := opaqueID(18)
	sealed := concat(enc, ct)
	return id, s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(vaultSchema); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO vault_credentials(id,user_id,label,sealed,aad,at) VALUES(?,?,?,?,?,?)",
			id, user, label, sealed, aad, time.Now().UTC().Format(time.RFC3339))
		return err
	})
}

func (s *Store) OpenSealedCredential(user, id string) ([]byte, error) {
	var sealed, aad []byte
	if err := s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(vaultSchema); err != nil {
			return err
		}
		return tx.QueryRow("SELECT sealed,aad FROM vault_credentials WHERE user_id=? AND id=?", user, id).Scan(&sealed, &aad)
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(sealed) <= encLength {
		return nil, ErrInvalid
	}
	key, err := s.vaultPrivateKey(user)
	if err != nil {
		return nil, err
	}
	return hpkeOpen(key, sealed[:encLength], sealed[encLength:], aad)
}

// DeleteSealedCredential 删除某个连接账号的封装凭据（原版 deleteAccount
// 的凭据侧，F-076 删除面）；删除后不可再解封。
func (s *Store) DeleteSealedCredential(user, id string) error {
	return s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(vaultSchema); err != nil {
			return err
		}
		result, err := tx.Exec("DELETE FROM vault_credentials WHERE user_id=? AND id=?", user, id)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
