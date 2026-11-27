// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// denpa の agent/Denpa.Agent/AgentTls.cs (MPL-2.0, https://github.com/danything/denpa) を Go に移したもの。

// Package pairing は denpa と TLS で組むための鍵・証明書・合言葉と、組んだ denpa の指紋を受け持つ。
package pairing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"denpa-mirakurun-bridge/internal/atomicfile"
)

// Pairing は denpa とお互いを証明書で確かめる (denpa の docs/agent.md「denpa とは TLS で組む」、agent/Denpa.Agent/AgentTls.cs と同じ決まり)。
// 置き場 (dir) には agent.key / agent.crt (一度作ったら作り直さない)・pair-token・clients (組んだ denpa の指紋) を置く
type Pairing struct {
	dir         string
	Certificate tls.Certificate
	fingerprint string // A
	token       string
	mu          sync.Mutex
	clients     map[string]bool
	tryMu       sync.Mutex // 合言葉の照合を 1 本ずつ通す
	// FailDelay は外れたら次を受けるまで待つ長さ (総当たり避け)
	FailDelay time.Duration
}

// Load は dir の鍵・証明書・合言葉・組んだ denpa を読む。無ければ作る
func Load(dir, envToken string) (*Pairing, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyFile, certFile := filepath.Join(dir, "agent.key"), filepath.Join(dir, "agent.crt")
	_, errKey := os.Stat(keyFile)
	_, errCert := os.Stat(certFile)
	// 読めないだけ (権限など) で作り直すと、組んだ denpa が全部外れる
	for _, err := range []error{errKey, errCert} {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	// 片方だけ残るのは作る途中で落ちたとき。証明書を書く前なので、それを覚えた denpa はいない
	if errKey != nil || errCert != nil {
		key, cert, err := newCertificatePEM()
		if err == nil {
			err = atomicfile.WritePrivate(keyFile, key)
		}
		if err == nil {
			err = atomicfile.WritePrivate(certFile, cert)
		}
		if err != nil {
			return nil, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	token, err := resolveToken(envToken, filepath.Join(dir, "pair-token"))
	if err != nil {
		return nil, err
	}
	p := &Pairing{dir: dir, Certificate: cert, fingerprint: Fingerprint(cert.Certificate[0]), token: token, clients: map[string]bool{}, FailDelay: time.Second}
	data, err := os.ReadFile(filepath.Join(dir, "clients"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.ToLower(strings.TrimSpace(line)); line != "" {
			p.clients[line] = true
		}
	}
	return p, nil
}

// TLSConfig はクライアント証明書を必須にし、どの証明書も受ける。見分けるのは HTTP の段 (gate)
func (p *Pairing) TLSConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{p.Certificate}, ClientAuth: tls.RequireAnyClientCert}
}

// PairURL は denpa の TUNER_AGENT_URL にそのまま書ける。unpaired はまだ誰とも組んでいないとき
func (p *Pairing) PairURL(host, port string) (url string, unpaired bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return "https://" + host + ":" + port + "#pair=" + p.token, len(p.clients) == 0
}

// Paired は client (指紋) と組んでいるか
func (p *Pairing) Paired(client string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clients[client]
}

// CheckProof は denpa が出した proof が合言葉と合うか。
// AGENT_PAIR_TOKEN に短い合言葉を渡されても総当たりしにくいよう、外れは 1 本ずつ FailDelay 待たせる
func (p *Pairing) CheckProof(client, proof string) bool {
	p.tryMu.Lock()
	defer p.tryMu.Unlock()
	if !hmac.Equal([]byte(proof), []byte(pairProof(p.token, "client", p.fingerprint, client))) {
		time.Sleep(p.FailDelay)
		return false
	}
	return true
}

// Proof は role (client か agent) の proof。こちらが返すのは agent
func (p *Pairing) Proof(role, client string) string {
	return pairProof(p.token, role, p.fingerprint, client)
}

// Add は client と組んで clients に保存する。added はまだ組んでいなかったとき。保存できなければ組まない
func (p *Pairing) Add(client string) (added bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	added = !p.clients[client]
	if added {
		p.clients[client] = true
		lines := slices.Sorted(maps.Keys(p.clients))
		err = atomicfile.WritePrivate(filepath.Join(p.dir, "clients"), []byte(strings.Join(lines, "\n")+"\n"))
		if err != nil {
			delete(p.clients, client)
		}
	}
	return added, err
}

// Remove は client を clients から消して保存する。paired は組んでいたとき。保存できなければ組を戻す
func (p *Pairing) Remove(client string) (paired bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.clients[client] {
		return false, nil
	}
	delete(p.clients, client)
	lines := slices.Sorted(maps.Keys(p.clients))
	var data []byte
	if len(lines) > 0 {
		data = []byte(strings.Join(lines, "\n") + "\n")
	}
	err = atomicfile.WritePrivate(filepath.Join(p.dir, "clients"), data)
	if err != nil {
		p.clients[client] = true
	}
	return true, err
}

// Fingerprint は指紋: DER の SHA-256 を小文字の 16 進で (64 字)
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// base64url(HMAC-SHA256(合言葉, "denpa-pair/<role>\n" + A + "\n" + D))。role は client (denpa が出す) か agent (こちらが返す)
func pairProof(token, role, agent, client string) string {
	h := hmac.New(sha256.New, []byte(token))
	io.WriteString(h, "denpa-pair/"+role+"\n"+agent+"\n"+client)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func resolveToken(envToken, file string) (string, error) {
	if t := strings.TrimSpace(envToken); t != "" {
		return t, nil
	}
	data, err := os.ReadFile(file)
	if t := strings.TrimSpace(string(data)); err == nil && t != "" {
		return t, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := base64.RawURLEncoding.EncodeToString(buf)
	return t, atomicfile.WritePrivate(file, []byte(t+"\n"))
}

// EC P-256 の鍵 (PKCS#8) と自己署名の証明書 (CN=denpa-agent、前日から 100 年)。
// 拡張は serverAuth と SKI だけ: basicConstraints があると Bun はこれを信頼の起点にできない (AgentTls.cs の CreatePem)
func newCertificatePEM() (keyPEM, certPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return nil, nil, err
	}
	ski := sha1.Sum(pub.Bytes()) // RFC 5280 4.2.1.2 (1)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "denpa-agent"},
		NotBefore:    now.AddDate(0, 0, -1),
		NotAfter:     now.AddDate(100, 0, 0),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		SubjectKeyId: ski[:],
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	k, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
