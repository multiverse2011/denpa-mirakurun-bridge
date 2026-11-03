package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// denpa 役のクライアント。自分の証明書を出し、ブリッジの証明書は確かめない (覚えるのは組んだ後の denpa の仕事)
func denpaClient(t *testing.T) (*http.Client, string) {
	t.Helper()
	keyPEM, certPEM, err := newCertificatePEM()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}}}
	t.Cleanup(c.CloseIdleConnections)
	return c, fingerprintOf(cert.Certificate[0])
}

func setupTLS(t *testing.T, dir, envToken string) (*Pairing, string) {
	t.Helper()
	p, err := LoadPairing(dir, envToken)
	if err != nil {
		t.Fatal(err)
	}
	p.failDelay = 50 * time.Millisecond
	b, err := NewBridge(Config{ChannelsFile: filepath.Join(t.TempDir(), "channels.json"), Version: "test", Pairing: p})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(b)
	srv.TLS = p.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return p, srv.URL
}

func postPair(t *testing.T, c *http.Client, base, client, proof string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"client": client, "proof": proof})
	res, err := c.Post(base+"/denpa/pair", "text/plain", bytes.NewReader(body)) // Content-Type は見ない
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func getChannels(t *testing.T, c *http.Client, base string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/denpa/channels", nil)
	req.Host = "evil.example.com" // 名前も見ない
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestPairAndRestart(t *testing.T) {
	dir := t.TempDir()
	p, base := setupTLS(t, dir, "")
	c, d := denpaClient(t)

	if status, body := getChannels(t, c, base); status != 403 || body["paired"] != false {
		t.Fatalf("unpaired: %d %v", status, body)
	}
	if _, unpaired := p.PairURL("h", "1"); !unpaired {
		t.Fatal("PairURL should report unpaired")
	}
	status, body := postPair(t, c, base, d, pairProof(p.token, "client", p.fingerprint, d))
	if status != 200 || body["proof"] != pairProof(p.token, "agent", p.fingerprint, d) {
		t.Fatalf("pair: %d %v", status, body)
	}
	if status, _ := getChannels(t, c, base); status != 200 {
		t.Fatalf("paired: %d", status)
	}
	// 組んだ相手からの組み直しも受ける
	if status, _ := postPair(t, c, base, d, pairProof(p.token, "client", p.fingerprint, d)); status != 200 {
		t.Fatalf("re-pair: %d", status)
	}

	again, err := LoadPairing(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.fingerprint != p.fingerprint || again.token != p.token || !again.clients[d] {
		t.Fatal("restart must keep the certificate, token and clients")
	}
	for _, f := range []string{"agent.key", "agent.crt", "pair-token", "clients"} {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
}

func TestPairRefusals(t *testing.T) {
	p, base := setupTLS(t, t.TempDir(), "")
	c, d := denpaClient(t)
	_, other := denpaClient(t)

	if status, body := postPair(t, c, base, d, pairProof("wrong", "client", p.fingerprint, d)); status != 403 || !strings.Contains(body["error"].(string), "合言葉") {
		t.Fatalf("wrong token: %d %v", status, body)
	}
	if status, body := postPair(t, c, base, other, pairProof(p.token, "client", p.fingerprint, other)); status != 403 || !strings.Contains(body["error"].(string), "名乗り") {
		t.Fatalf("wrong client: %d %v", status, body)
	}
	if status, _ := postPair(t, c, base, d, ""); status != 403 {
		t.Fatalf("empty proof: %d", status)
	}
	if status, _ := getChannels(t, c, base); status != 403 {
		t.Fatalf("refused pairs must not pass: %d", status)
	}

	// クライアント証明書の無い相手 (ブラウザなど) は TLS の段で落ちる
	bare := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if res, err := bare.Get(base + "/denpa/channels"); err == nil {
		res.Body.Close()
		t.Fatalf("no client certificate must fail the handshake: %d", res.StatusCode)
	}
}

func TestPairTokenFromEnv(t *testing.T) {
	dir := t.TempDir()
	p, err := LoadPairing(dir, " given \n")
	if err != nil {
		t.Fatal(err)
	}
	if p.token != "given" {
		t.Fatalf("token %q", p.token)
	}
	if _, err := os.Stat(filepath.Join(dir, "pair-token")); !os.IsNotExist(err) {
		t.Fatal("pair-token must not be written when AGENT_PAIR_TOKEN is set")
	}
}

// denpa (Bun) が信頼の起点にできる形か: basicConstraints と keyUsage を付けない (AgentTls.cs の CreatePem)
func TestCertificateShape(t *testing.T) {
	p, err := LoadPairing(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(p.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if cert.BasicConstraintsValid || cert.KeyUsage != 0 || len(cert.SubjectKeyId) == 0 || len(cert.AuthorityKeyId) != 0 ||
		cert.Subject.CommonName != "denpa-agent" || cert.NotAfter.Year()-cert.NotBefore.Year() < 99 {
		t.Fatalf("unexpected certificate: %+v", cert)
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Fatal(err)
	}
}

// 合言葉の外れは 1 本ずつ待たせるので、並べて送っても速くならない
func TestWrongProofsAreSerialized(t *testing.T) {
	p, base := setupTLS(t, t.TempDir(), "")
	c, d := denpaClient(t)
	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { postPair(t, c, base, d, "wrong") })
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed < 4*p.failDelay {
		t.Fatalf("wrong proofs must wait one by one: %v", elapsed)
	}
	// 正しい合言葉は外れの後でも組める
	if status, _ := postPair(t, c, base, d, pairProof(p.token, "client", p.fingerprint, d)); status != 200 {
		t.Fatalf("pair after failures: %d", status)
	}
}
