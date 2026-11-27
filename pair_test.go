package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"denpa-mirakurun-bridge/internal/pairing"
)

// denpa 役のクライアント。自分の証明書を出し、ブリッジの証明書は確かめない (覚えるのは組んだ後の denpa の仕事)
func denpaClient(t *testing.T) (*http.Client, string) {
	t.Helper()
	q, err := pairing.Load(t.TempDir(), "") // 証明書の作り方はブリッジと同じ
	if err != nil {
		t.Fatal(err)
	}
	cert := q.Certificate
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}}}
	t.Cleanup(c.CloseIdleConnections)
	return c, pairing.Fingerprint(cert.Certificate[0])
}

func setupTLS(t *testing.T, dir, envToken string) (*pairing.Pairing, string) {
	t.Helper()
	p, err := pairing.Load(dir, envToken)
	if err != nil {
		t.Fatal(err)
	}
	p.FailDelay = 50 * time.Millisecond
	b, err := NewBridge(Config{MirakurunURL: "http://127.0.0.1:1", ChannelsFile: filepath.Join(t.TempDir(), "channels.json"), Version: "test", Pairing: p})
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
	status, body := postPair(t, c, base, d, p.Proof("client", d))
	if status != 200 || body["proof"] != p.Proof("agent", d) {
		t.Fatalf("pair: %d %v", status, body)
	}
	if status, _ := getChannels(t, c, base); status != 200 {
		t.Fatalf("paired: %d", status)
	}
	// 組んだ相手からの組み直しも受ける
	if status, _ := postPair(t, c, base, d, p.Proof("client", d)); status != 200 {
		t.Fatalf("re-pair: %d", status)
	}

	again, err := pairing.Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.Proof("agent", d) != p.Proof("agent", d) || !again.Paired(d) { // 証明書と合言葉が同じなら proof も同じ
		t.Fatal("restart must keep the certificate, token and clients")
	}
	for _, f := range []string{"agent.key", "agent.crt", "pair-token", "clients"} {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
}

func TestPairRefusals(t *testing.T) {
	dir := t.TempDir()
	p, base := setupTLS(t, dir, "")
	// 証明書 (指紋) は同じで合言葉だけ違う
	wrongDir := t.TempDir()
	for _, name := range []string{"agent.key", "agent.crt"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			err = os.WriteFile(filepath.Join(wrongDir, name), data, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	wrong, err := pairing.Load(wrongDir, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	c, d := denpaClient(t)
	_, other := denpaClient(t)

	if status, body := postPair(t, c, base, d, wrong.Proof("client", d)); status != 403 || !strings.Contains(body["error"].(string), "合言葉") {
		t.Fatalf("wrong token: %d %v", status, body)
	}
	if status, body := postPair(t, c, base, other, p.Proof("client", other)); status != 403 || !strings.Contains(body["error"].(string), "一致しません") {
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

func deletePair(t *testing.T, c *http.Client, base string) int {
	t.Helper()
	req, _ := http.NewRequest("DELETE", base+"/denpa/pair", nil)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// 削除: 呼んできた denpa だけを clients から消し、204 を返す。組んでいない相手は呼べない
func TestUnpairRemovesOnlyTheCaller(t *testing.T) {
	dir := t.TempDir()
	p, base := setupTLS(t, dir, "")
	c1, d1 := denpaClient(t)
	c2, d2 := denpaClient(t)
	for _, x := range []struct {
		c *http.Client
		d string
	}{{c1, d1}, {c2, d2}} {
		if status, _ := postPair(t, x.c, base, x.d, p.Proof("client", x.d)); status != 200 {
			t.Fatalf("pair %s: %d", x.d[:8], status)
		}
	}

	// 組んでいない相手からの DELETE は gate が断る
	c3, _ := denpaClient(t)
	if status := deletePair(t, c3, base); status != 403 {
		t.Fatalf("unpaired DELETE: %d", status)
	}

	if status := deletePair(t, c1, base); status != 204 {
		t.Fatalf("DELETE: %d", status)
	}
	if status, _ := getChannels(t, c1, base); status != 403 {
		t.Fatalf("removed denpa must be refused: %d", status)
	}
	if status, _ := getChannels(t, c2, base); status != 200 {
		t.Fatalf("other denpa must stay paired: %d", status)
	}

	again, err := pairing.Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.Paired(d1) || !again.Paired(d2) {
		t.Fatalf("clients: d1=%v d2=%v", again.Paired(d1), again.Paired(d2))
	}
}

// 書き込めなければ、消した組を戻す
func TestUnpairSaveFailureKeepsThePairing(t *testing.T) {
	dir := t.TempDir()
	p, base := setupTLS(t, dir, "")
	c, d := denpaClient(t)
	if status, _ := postPair(t, c, base, d, p.Proof("client", d)); status != 200 {
		t.Fatalf("pair: %d", status)
	}
	// clients をディレクトリに置き換えて、rename を失敗させる
	clients := filepath.Join(dir, "clients")
	if err := os.Remove(clients); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(clients, 0o700); err != nil {
		t.Fatal(err)
	}
	if status := deletePair(t, c, base); status != 500 {
		t.Fatalf("DELETE: %d", status)
	}
	if !p.Paired(d) {
		t.Fatal("保存に失敗したら組を戻すこと")
	}
	if status, _ := getChannels(t, c, base); status != 200 {
		t.Fatalf("rolled back denpa must stay paired: %d", status)
	}
}

// 書き込めなければ、足した組を戻す
func TestPairSaveFailureKeepsUnpaired(t *testing.T) {
	dir := t.TempDir()
	p, base := setupTLS(t, dir, "")
	c, d := denpaClient(t)
	// clients をディレクトリにして、rename を失敗させる
	if err := os.Mkdir(filepath.Join(dir, "clients"), 0o700); err != nil {
		t.Fatal(err)
	}
	if status, _ := postPair(t, c, base, d, p.Proof("client", d)); status != 500 {
		t.Fatalf("pair: %d", status)
	}
	if p.Paired(d) {
		t.Fatal("保存に失敗したら組まないこと")
	}
	if status, _ := getChannels(t, c, base); status != 403 {
		t.Fatalf("unsaved denpa must not pass: %d", status)
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
	if elapsed := time.Since(start); elapsed < 4*p.FailDelay {
		t.Fatalf("wrong proofs must wait one by one: %v", elapsed)
	}
	// 正しい合言葉は外れの後でも組める
	if status, _ := postPair(t, c, base, d, p.Proof("client", d)); status != 200 {
		t.Fatalf("pair after failures: %d", status)
	}
}
