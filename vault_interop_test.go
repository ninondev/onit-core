package main

// 跨实现互验：用独立的 JavaScript 实现（node:crypto 的 HMAC/X25519/
// ChaCha20-Poly1305 原语，按 RFC 9180 手拼全部步骤）封装凭据，Go 金库
// 必须解出同一原文。两端不是共享代码，证明的是同一协议。
import (
	"encoding/base64"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const interopScript = `
const crypto=require('node:crypto');
const KEM=Buffer.concat([Buffer.from('KEM'),Buffer.from([0,32])]);
const HPKE=Buffer.concat([Buffer.from('HPKE'),Buffer.from([0,32]),Buffer.from([0,1]),Buffer.from([0,3])]);
function extract(salt,ikm){return crypto.createHmac('sha256',salt.length?salt:Buffer.alloc(32)).update(ikm).digest();}
function expand(prk,info,len){
  const out=[];let prev=Buffer.alloc(0);
  for(let i=1;Buffer.concat(out).length<len;i++){
    prev=crypto.createHmac('sha256',prk).update(Buffer.concat([prev,info,Buffer.from([i])])).digest();
    out.push(prev);
  }
  return Buffer.concat(out).subarray(0,len);
}
function labeledExtract(salt,label,ikm,suite){return extract(salt,Buffer.concat([Buffer.from('HPKE-v1'),suite,Buffer.from(label),ikm]));}
function labeledExpand(prk,label,info,suite,len){return expand(prk,Buffer.concat([Buffer.from([len>>8,len&255]),Buffer.from('HPKE-v1'),suite,Buffer.from(label),info]),len);}
const x25519Private=sk=>crypto.createPrivateKey({key:Buffer.concat([Buffer.from('302e020100300506032b656e04220420','hex'),sk]),format:'der',type:'pkcs8'});
const x25519Public=pk=>crypto.createPublicKey({key:Buffer.concat([Buffer.from('302a300506032b656e032100','hex'),pk]),format:'der',type:'spki'});
const INPUT=JSON.parse(require('fs').readFileSync(0,'utf8'));
const pkR=Buffer.from(INPUT.pkR,'base64');
const ikmE=Buffer.from(INPUT.ikmE,'hex');
const dkp=labeledExtract(Buffer.alloc(0),'dkp_prk',ikmE,KEM);
const skE=labeledExpand(dkp,'sk',Buffer.alloc(0),KEM,32);
const enc=crypto.createPublicKey(x25519Private(skE)).export({type:'spki',format:'der'}).subarray(12);
const shared0=crypto.diffieHellman({privateKey:x25519Private(skE),publicKey:x25519Public(pkR)});
const eae=labeledExtract(Buffer.alloc(0),'eae_prk',shared0,KEM);
const shared=labeledExpand(eae,'shared_secret',Buffer.concat([enc,pkR]),KEM,32);
const psk=labeledExtract(Buffer.alloc(0),'psk_id_hash',Buffer.alloc(0),HPKE);
const infoHash=labeledExtract(Buffer.alloc(0),'info_hash',Buffer.from(INPUT.info,'utf8'),HPKE);
const ctx=Buffer.concat([Buffer.from([0]),psk,infoHash]);
const secret=labeledExtract(shared,'secret',Buffer.alloc(0),HPKE);
const key=labeledExpand(secret,'key',ctx,HPKE,32);
const nonce=labeledExpand(secret,'base_nonce',ctx,HPKE,12);
const box=crypto.createCipheriv('chacha20-poly1305',key,nonce,{authTagLength:16});
box.setAAD(Buffer.from(INPUT.aad,'utf8'));
const pt=Buffer.from(INPUT.pt,'utf8');
const ct=Buffer.concat([box.update(pt),box.final(),box.getAuthTag()]);
process.stdout.write(JSON.stringify({enc:enc.toString('base64'),shared:shared.toString('base64'),ct:ct.toString('base64')}));
`

func runInteropSeal(t *testing.T, input string) object {
	t.Helper()
	script := filepath.Join(t.TempDir(), "seal.cjs")
	if err := os.WriteFile(script, []byte(interopScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script)
	cmd.Stdin = strings.NewReader(input)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node interop script failed: %v\n%s", err, raw)
	}
	out, err := parseObject(raw)
	if err != nil {
		t.Fatal(err, string(raw))
	}
	return out
}

func TestVaultCrossImplementationWithNode(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "interop.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	token := issue(t, s)
	user, _ := s.User(token)

	public, err := s.VaultPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	input := object{"pkR": base64.StdEncoding.EncodeToString(public),
		"ikmE": "909a9b35d3dc4713a5e72a4da274b55d3d3821a37e5d099e74a647db583a904b",
		"pt":   `{"api_key":"hevy-live-42"}`,
		"aad":  "connector:hevy", "info": vaultInfo}
	script := runInteropSeal(t, encode(input))

	// JS 自己算出的 shared_secret 先对上 Go 的解封装，锁死 KEM 一致性。
	enc, _ := base64.StdEncoding.DecodeString(str(script, "enc"))
	sharedJS, _ := base64.StdEncoding.DecodeString(str(script, "shared"))
	priv, err := s.vaultPrivateKey(user)
	if err != nil {
		t.Fatal(err)
	}
	sharedGo, err := decapsulate(priv, enc)
	if err != nil || string(sharedGo) != string(sharedJS) {
		t.Fatalf("KEM mismatch between implementations: %x vs %x", sharedGo, sharedJS)
	}

	ct, _ := base64.StdEncoding.DecodeString(str(script, "ct"))
	id, err := s.StoreSealedCredential(user, "Hevy", enc, ct, []byte("connector:hevy"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := s.OpenSealedCredential(user, id)
	if err != nil || string(opened) != `{"api_key":"hevy-live-42"}` {
		t.Fatalf("Go failed to open independently sealed credential: %v %s", err, opened)
	}
}
