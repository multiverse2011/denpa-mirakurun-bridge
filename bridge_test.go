package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"denpa-mirakurun-bridge/internal/channels"
)

func services(networkID int, ids ...int) []channels.MirakurunService {
	var out []channels.MirakurunService
	for _, id := range ids {
		out = append(out, channels.MirakurunService{ServiceID: id, NetworkID: networkID, Type: 1})
	}
	return out
}

var (
	grChannels = []channels.Mirakurun{
		{Type: "GR", Channel: "27", Services: services(32736, 1024)},
		{Type: "GR", Channel: "21", Services: services(32737, 1056)},
	}
	bsChannels = []channels.Mirakurun{
		{Type: "BS", Channel: "BS15_0", Services: services(4, 101)},
		{Type: "BS", Channel: "BS15_1", Services: services(4, 103)},
	}
	oneGR = [][]string{{"GR"}}
	twoGR = [][]string{{"GR"}, {"GR"}}
)

type testEnv struct {
	mk   *mockMirakurun
	base string
	b    *Bridge
}

// tweak は Bridge が動き出す前に時間などを縮める
func setup(t *testing.T, tuners [][]string, mkChannels []channels.Mirakurun, priorityMap string, keylessTuners int, tweak ...func(*Bridge)) testEnv {
	t.Helper()
	mk := newMock(tuners, mkChannels)
	t.Cleanup(mk.Close)
	pm, err := parsePriorityMap(priorityMap, messages{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBridge(Config{MirakurunURL: mk.URL, PriorityMap: pm, ChannelsFile: filepath.Join(t.TempDir(), "channels.json"),
		Version: "test", KeylessTuners: keylessTuners})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tweak {
		f(b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go b.Watch(ctx)
	srv := httptest.NewServer(b)
	t.Cleanup(func() {
		cancel()
		srv.CloseClientConnections()
		srv.Close()
	})
	return testEnv{mk, srv.URL, b}
}

type opened struct {
	status int
	cancel context.CancelFunc
	// "complete" は正常な終わり (chunked の終端まで届いた)、"broken" は接続ごと壊れた
	ended chan string
}

// denpa の openChannelStream と同じ呼び方。200 なら最初の塊まで待つ
func open(t *testing.T, base string, q map[string]string, after ...string) opened {
	t.Helper()
	v := url.Values{}
	for k, x := range q {
		v.Set(k, x)
	}
	for _, a := range after {
		v.Add("after", a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/denpa/stream?"+v.Encode(), nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	o := opened{status: res.StatusCode, cancel: cancel, ended: make(chan string, 1)}
	if res.StatusCode != 200 {
		res.Body.Close()
		return o
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(res.Body, first); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := io.Copy(io.Discard, res.Body)
		if err == nil {
			o.ended <- "complete"
		} else {
			o.ended <- "broken"
		}
		res.Body.Close()
	}()
	return o
}

func until(t *testing.T, check func() bool) {
	t.Helper()
	for range 100 {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("待っても条件が揃いませんでした")
}

func ended(t *testing.T, o opened) string {
	t.Helper()
	select {
	case e := <-o.ended:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("ストリームが終わりません")
		return ""
	}
}

func getTuners(t *testing.T, base string) []map[string]any {
	t.Helper()
	res, err := http.Get(base + "/denpa/tuners")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct{ Tuners []map[string]any }
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Tuners
}

func q(typ, ch, priority, use string) map[string]string {
	return map[string]string{"type": typ, "channel": ch, "priority": priority, "use": use}
}

func TestBadMirakurunURL(t *testing.T) {
	for _, u := range []string{"http://[::1", "mirakurun:40772", "mirakurun.lan:40772", "ftp://mirakurun", "http://"} {
		_, err := NewBridge(Config{MirakurunURL: u, ChannelsFile: filepath.Join(t.TempDir(), "channels.json")})
		if err == nil || !strings.Contains(err.Error(), "MIRAKURUN_URL") {
			t.Errorf("%s: want MIRAKURUN_URL error, got %v", u, err)
		}
	}
	for _, u := range []string{"http://mirakurun:40772", "https://mirakurun.lan", "http://[::1]:40772"} {
		if _, err := NewBridge(Config{MirakurunURL: u, ChannelsFile: filepath.Join(t.TempDir(), "channels.json")}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

// 停止を求められたあとも、録画の選局が終わるまで待つ。ライブなどほかの利用者は待たない (denpa のエージェントの Drain と同じ)
func TestDrainWaitsOnlyForRecordings(t *testing.T) {
	e := setup(t, twoGR, grChannels, "", 9, func(b *Bridge) { b.drainPoll = 10 * time.Millisecond })
	drained := func(limit time.Duration) chan struct{} {
		done := make(chan struct{})
		go func() { e.b.Drain(limit); close(done) }()
		return done
	}

	live := open(t, e.base, q("GR", "T27", "9", "live a"))
	select {
	case <-drained(time.Minute):
	case <-time.After(time.Second):
		t.Fatal("ライブを待っています")
	}

	rec := open(t, e.base, q("GR", "T21", "10", "rec 1"))
	done := drained(time.Minute)
	select {
	case <-done:
		t.Fatal("録画の途中で止まりました")
	case <-time.After(200 * time.Millisecond):
	}
	rec.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("録画が終わっても止まりません")
	}
	live.cancel()

	// 上限を過ぎたら待たない (見に行く間隔が長くても上限で抜ける)
	e.b.drainPoll = time.Minute
	open(t, e.base, q("GR", "T21", "10", "rec"))
	start := time.Now()
	e.b.Drain(100 * time.Millisecond)
	if d := time.Since(start); d < 100*time.Millisecond || d > time.Second {
		t.Fatal(d)
	}
}

// 止まるときは SSE をこちらから閉じる。denpa は開きっぱなしにするので、閉じないと Shutdown が期限まで待つ
func TestStopClosesEvents(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	res, err := http.Get(e.base + "/denpa/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, res.Body)
		ended <- err
	}()
	e.b.Stop()
	e.b.Stop() // Shutdown を二度呼んでも落ちない
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("SSE が閉じません")
	}
}

// 仮のカードの番号は初回に作って保存し、次からはそれを読む。ブリッジ (置き場) ごとに違い、10 進 16 桁で、
// 実物の B-CAS 番号 (48 ビット) と重ならない。壊れている・書けないときは、上書きせずにその起動の間だけの番号を返す
func TestLoadCardID(t *testing.T) {
	valid := func(id string) bool {
		n, err := strconv.ParseUint(id, 10, 64)
		return err == nil && len(id) == 16 && n >= 1<<48
	}
	dir := filepath.Join(t.TempDir(), "data")
	id, err := loadCardID(dir)
	if err != nil || !valid(id) {
		t.Fatalf("番号: %s %v", id, err)
	}
	if again, err := loadCardID(dir); err != nil || again != id {
		t.Fatalf("読み直すと変わった: %s → %s %v", id, again, err)
	}
	if other, err := loadCardID(t.TempDir()); err != nil || other == id {
		t.Fatalf("別のブリッジと同じ番号: %s %v", other, err)
	}
	for _, bad := range []string{"", "123", "0000000000000001", "abcdefghijklmnop"} {
		name := filepath.Join(dir, "card-id")
		if err := os.WriteFile(name, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := loadCardID(dir)
		if data, _ := os.ReadFile(name); err == nil || !valid(got) || string(data) != bad {
			t.Fatalf("%q: %s %v (ファイル %q)", bad, got, err, data)
		}
	}
	// 置き場を作れない (ファイルがある)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadCardID(file); err == nil || !valid(got) {
		t.Fatalf("書けないとき: %s %v", got, err)
	}
}

// 番号を保存できなくても (置き場が読み取り専用) 起動し、その起動の間だけの番号を名乗る
func TestNewBridgeCardIDUnwritable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600) == nil {
		t.Skip("読み取り専用の置き場にも書ける (root)")
	}
	b, err := NewBridge(Config{MirakurunURL: "http://127.0.0.1:40772", ChannelsFile: filepath.Join(dir, "channels.json")})
	if err != nil || len(b.cardID) != 16 {
		t.Fatalf("%v %+v", err, b)
	}
}
