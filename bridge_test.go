package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func services(networkID int, ids ...int) []struct {
	ServiceID int `json:"serviceId"`
	NetworkID int `json:"networkId"`
} {
	var out []struct {
		ServiceID int `json:"serviceId"`
		NetworkID int `json:"networkId"`
	}
	for _, id := range ids {
		out = append(out, struct {
			ServiceID int `json:"serviceId"`
			NetworkID int `json:"networkId"`
		}{id, networkID})
	}
	return out
}

var (
	grChannels = []MirakurunChannel{
		{Type: "GR", Channel: "27", Services: services(32736, 1024)},
		{Type: "GR", Channel: "21", Services: services(32737, 1056)},
	}
	bsChannels = []MirakurunChannel{
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
func setup(t *testing.T, tuners [][]string, channels []MirakurunChannel, priorityMap string, keylessTuners int, tweak ...func(*Bridge)) testEnv {
	t.Helper()
	mk := newMock(tuners, channels)
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

func TestPriorityMapAndDecode(t *testing.T) {
	e := setup(t, twoGR, grChannels, "10:2,3:-1", 9)
	open(t, e.base, q("GR", "T27", "10", "rec 1"))
	r := e.mk.lastRequest()
	if r.Channel != "27" || r.Priority != 2 || r.Decode != "1" || !strings.HasSuffix(r.Agent, "(rec 1)") {
		t.Fatalf("rec: %+v", r)
	}
	open(t, e.base, q("GR", "T21", "3", "epg T21"))
	// 負は Mirakurun の HTTP API が断るので 0。番組表には復号させない
	if r := e.mk.lastRequest(); r.Priority != 0 || r.Decode != "0" {
		t.Fatalf("epg: %+v", r)
	}
}

func TestSameChannelShared(t *testing.T) {
	e := setup(t, twoGR, grChannels, "", 9)
	open(t, e.base, q("GR", "T27", "10", "rec 1"))
	open(t, e.base, q("GR", "T27", "9", "live a"))
	if e.mk.users(0) != 2 || e.mk.users(1) != 0 {
		t.Fatalf("users: %d %d", e.mk.users(0), e.mk.users(1))
	}
	tuners := getTuners(t, e.base)
	ch, _ := json.Marshal(tuners[0]["channel"])
	users, _ := json.Marshal(tuners[0]["users"])
	if string(ch) != `{"channel":"T27","type":"GR"}` ||
		string(users) != `[{"priority":10,"use":"rec 1"},{"priority":9,"use":"live a"}]` ||
		tuners[0]["rank"] != 0.0 || tuners[1]["rank"] != 1.0 || tuners[1]["channel"] != nil ||
		tuners[0]["pin"] != "T0" || tuners[1]["pin"] != "T1" {
		t.Fatalf("tuners: %s %s %v", ch, users, tuners)
	}
}

func TestBusyBecomes409(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	open(t, e.base, q("GR", "T27", "10", "rec 1"))
	if s := open(t, e.base, q("GR", "T21", "10", "rec 2")).status; s != 409 {
		t.Fatal(s)
	}
}

func TestUnknownChannel404(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	if s := open(t, e.base, q("GR", "T40", "5", "scan T40")).status; s != 404 {
		t.Fatal(s)
	}
}

func TestTakenOverStreamIsBroken(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	epg := open(t, e.base, q("GR", "T27", "3", "epg T27"))
	if s := open(t, e.base, q("GR", "T21", "10", "rec 1")).status; s != 200 {
		t.Fatal(s)
	}
	if got := ended(t, epg); got != "broken" {
		t.Fatal(got)
	}
}

func TestClientCloseReleasesMirakurun(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	live := open(t, e.base, q("GR", "T27", "9", "live a"))
	until(t, func() bool { return e.mk.users(0) == 1 })
	live.cancel()
	until(t, func() bool { return e.mk.users(0) == 0 })
}

func TestAfterHandsOffCleanly(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	prev := open(t, e.base, q("GR", "T27", "10", "rec 1"))
	if s := open(t, e.base, q("GR", "T21", "10", "rec 2"), "rec 1").status; s != 200 {
		t.Fatal(s)
	}
	if got := ended(t, prev); got != "complete" {
		t.Fatal(got)
	}
}

func TestAfterSameChannelKeepsTuning(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	prev := open(t, e.base, q("GR", "T27", "10", "rec 1"))
	if s := open(t, e.base, q("GR", "T27", "10", "rec 2"), "rec 1").status; s != 200 {
		t.Fatal(s)
	}
	if got := ended(t, prev); got != "complete" {
		t.Fatal(got)
	}
	until(t, func() bool { return e.mk.users(0) == 1 })
}

func TestSatelliteMatchedByServices(t *testing.T) {
	e := setup(t, [][]string{{"BS", "CS"}}, bsChannels, "", 9)
	// 預かる前: TSID の無い入口は中継器のどれか、TSID 付きは断る
	open(t, e.base, q("BS", "BS15", "3", "epg BS15")).cancel()
	if r := e.mk.lastRequest(); r.Channel != "BS15_0" {
		t.Fatal(r)
	}
	if s := open(t, e.base, q("BS", "BS15:16626", "3", "epg x")).status; s != 404 {
		t.Fatal(s)
	}
	body := `{"scanned":["BS"],"channels":[{"type":"BS","channel":"BS15:16626","networkId":4,"transportStreamId":16626,
		"remoteControlKeyId":3,"services":[{"serviceId":103,"serviceType":1,"name":"BS 103"}]}]}`
	req, _ := http.NewRequest("PUT", e.base+"/denpa/channels", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res.StatusCode)
	}
	open(t, e.base, q("BS", "BS15:16626", "10", "rec 1"))
	if r := e.mk.lastRequest(); r.Channel != "BS15_1" {
		t.Fatal(r)
	}
	until(t, func() bool {
		ch, _ := json.Marshal(getTuners(t, e.base)[0]["channel"])
		return string(ch) == `{"channel":"BS15:16626","type":"BS"}`
	})
}

func TestTunerEventsBecomeSSE(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	res, err := http.Get(e.base + "/denpa/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	time.Sleep(100 * time.Millisecond) // 購読が Mirakurun まで繋がるのを待つ
	open(t, e.base, q("GR", "T27", "9", "live a"))
	buf := make([]byte, 64)
	n, err := res.Body.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "event: tuners\ndata: {}\n\n") {
		t.Fatalf("%q %v", buf[:n], err)
	}
}

func TestHostAndContentType(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	req, _ := http.NewRequest("GET", e.base+"/denpa/tuners", nil)
	req.Host = "evil.example.com"
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	res, _ := http.Post(e.base+"/denpa/decode", "text/plain", strings.NewReader("{}"))
	if res.StatusCode != 415 {
		t.Fatal(res.StatusCode)
	}
	req, _ = http.NewRequest("PUT", e.base+"/denpa/channels", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 400 {
		t.Fatal(res.StatusCode)
	}
}

func TestHostAllowed(t *testing.T) {
	for _, h := range []string{"192.168.1.5:25252", "[fd00::5]:25252", "localhost", "tuner-agent:25252", "host.docker.internal", "mac.local"} {
		if !hostAllowed(h, nil) {
			t.Error(h)
		}
	}
	if hostAllowed("agent.example.com", nil) || hostAllowed("", nil) || !hostAllowed("a.example.com", []string{".example.com"}) {
		t.Error("extra")
	}
}

func TestKeylessLimit(t *testing.T) {
	e := setup(t, twoGR, grChannels, "", 1)
	for _, c := range []struct {
		ch, priority, use string
		want              int
	}{
		{"T27", "3", "epg T27", 200},
		{"T21", "3", "epg T21", 409},
		{"T27", "1", "logo GR/T27", 200}, // 相乗りは数えない
		{"T21", "10", "rec 1", 200},      // 録画には掛からない
		{"T21", "3", "epg T21", 200},     // 録画が開いたチャンネルには乗れる
	} {
		if s := open(t, e.base, q("GR", c.ch, c.priority, c.use)).status; s != c.want {
			t.Fatalf("%s: %d", c.use, s)
		}
	}
}

// PRIORITY_MAP で全部同じ値に潰しても、denpa の優先度どおりに取り上げる
func TestPreemptsOwnLowerPriorityUsers(t *testing.T) {
	e := setup(t, oneGR, grChannels, "10:0,9:0,3:0", 1)
	epg := open(t, e.base, q("GR", "T27", "3", "epg T27"))
	live := open(t, e.base, q("GR", "T21", "9", "live a"))
	if live.status != 200 {
		t.Fatal(live.status)
	}
	if got := ended(t, epg); got != "broken" {
		t.Fatal(got)
	}
	if s := open(t, e.base, q("GR", "T27", "10", "rec 1")).status; s != 200 {
		t.Fatal(s)
	}
	if got := ended(t, live); got != "broken" {
		t.Fatal(got)
	}
	// 録画からは取り上げない
	if s := open(t, e.base, q("GR", "T21", "9", "live b")).status; s != 409 {
		t.Fatal(s)
	}
}

// 取り上げた相手がすぐ掛け直しても、空いたチューナーを先に取らせない
func TestPreemptedUserCannotRetake(t *testing.T) {
	e := setup(t, oneGR, grChannels, "10:0,9:0,3:0", 1)
	epg := open(t, e.base, q("GR", "T27", "3", "epg T27"))
	live := make(chan opened, 1)
	go func() { live <- open(t, e.base, q("GR", "T21", "9", "live a")) }()
	if got := ended(t, epg); got != "broken" {
		t.Fatal(got)
	}
	if s := open(t, e.base, q("GR", "T27", "3", "epg T27")).status; s != 409 {
		t.Fatal(s)
	}
	if s := (<-live).status; s != 200 {
		t.Fatal(s)
	}
}

func TestKeylessLimitSharesSatelliteTuners(t *testing.T) {
	channels := append(slices.Clone(bsChannels), MirakurunChannel{Type: "CS", Channel: "CS2", Services: services(6, 296)})
	e := setup(t, [][]string{{"BS", "CS"}, {"BS", "CS"}}, channels, "", 1)
	if s := open(t, e.base, q("BS", "BS15", "3", "epg BS15")).status; s != 200 {
		t.Fatal(s)
	}
	if s := open(t, e.base, q("CS", "CS2", "3", "epg CS2")).status; s != 409 {
		t.Fatal(s)
	}
}

func TestKeylessLimitCountsRidingOnMirakurunEPG(t *testing.T) {
	e := setup(t, twoGR, grChannels, "", 1)
	// Mirakurun 自身の番組表取得 (-1) が両方のチューナーを使っている
	for _, ch := range []string{"27", "21"} {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, "GET", e.mk.URL+"/api/channels/GR/"+ch+"/stream", nil)
		req.Header.Set("X-Mirakurun-Priority", "-1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = io.Copy(io.Discard, res.Body) }()
	}
	until(t, func() bool { return e.mk.users(0) == 1 && e.mk.users(1) == 1 })
	if s := open(t, e.base, q("GR", "T27", "3", "epg T27")).status; s != 200 {
		t.Fatal(s)
	}
	if s := open(t, e.base, q("GR", "T21", "3", "epg T21")).status; s != 409 {
		t.Fatal(s)
	}
}

func TestGRMatchedByNumberNotServices(t *testing.T) {
	// 中継局: T16 と T20 は同じ局を流す
	mk := []MirakurunChannel{{Type: "GR", Channel: "16"}, {Type: "GR", Channel: "20", Services: services(32736, 1024)}}
	var stored []DenpaChannel
	for _, ch := range []string{"T16", "T20"} {
		var d DenpaChannel
		_ = json.Unmarshal([]byte(`{"type":"GR","channel":"`+ch+`","networkId":32736,"services":[{"serviceId":1024}]}`), &d)
		stored = append(stored, d)
	}
	if toMirakurun("GR", "T16", stored, mk) != "16" || toMirakurun("GR", "T20", stored, mk) != "20" || toDenpa("GR", "20", stored, mk) != "T20" {
		t.Fatal("GR mapping")
	}
}

func TestParsePriorityMap(t *testing.T) {
	if _, err := parsePriorityMap("10:2,x", messages{}); err == nil || !strings.Contains(err.Error(), "読めません") {
		t.Fatal(err)
	}
	if _, err := parsePriorityMap("10:2,x", messages{en: true}); err == nil || !strings.HasPrefix(err.Error(), "cannot parse PRIORITY_MAP") {
		t.Fatal(err)
	}
}

func errorOf(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct{ Error string }
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Error
}

func TestMessagesJapaneseByDefaultAndEnglish(t *testing.T) {
	ja := setup(t, oneGR, grChannels, "", 9)
	if s, msg := errorOf(t, ja.base+"/denpa/stream?type=XX&channel=1"); s != 400 || msg != "type・channel・priority を確かめてください" {
		t.Fatal(s, msg)
	}
	en := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.msg = messages{en: true} })
	if s, msg := errorOf(t, en.base+"/denpa/stream?type=XX&channel=1"); s != 400 || msg != "check type, channel and priority" {
		t.Fatal(s, msg)
	}
	if s, msg := errorOf(t, en.base+"/denpa/stream?type=GR&channel=T40"); s != 404 || msg != "Mirakurun has no channel matching GR T40" {
		t.Fatal(s, msg)
	}
}

// 開いたあと Mirakurun が黙ったら、録れていないのに続いているように見せず接続ごと壊す
func TestStalledMirakurunIsBroken(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.stallTimeout = 200 * time.Millisecond })
	e.mk.mu.Lock()
	e.mk.stall = true
	e.mk.mu.Unlock()
	rec := open(t, e.base, q("GR", "T27", "10", "rec 1"))
	if got := ended(t, rec); got != "broken" {
		t.Fatal(got)
	}
	until(t, func() bool { return e.mk.users(0) == 0 })
}

// Mirakurun が応答ヘッダを返さないままでも、待ち続けずにチューナーを返す
func TestHungMirakurunGivesUp(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.stallTimeout = 200 * time.Millisecond })
	e.mk.mu.Lock()
	e.mk.hang = true
	e.mk.mu.Unlock()
	if got := open(t, e.base, q("GR", "T27", "10", "rec 1")).status; got != 502 {
		t.Fatal(got)
	}
	until(t, func() bool { return e.mk.users(0) == 0 })
}

// 受け手が読まないまま (電源断など) でも、書き込みの期限でチューナーを返す
func TestUnreadClientReleasesMirakurun(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.writeTimeout = 200 * time.Millisecond })
	e.mk.mu.Lock()
	e.mk.burst = true
	e.mk.mu.Unlock()
	conn, err := net.Dial("tcp", strings.TrimPrefix(e.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	_, _ = io.WriteString(conn, "GET /denpa/stream?type=GR&channel=T27&priority=10&use=rec+1 HTTP/1.1\r\nHost: localhost\r\n\r\n")
	until(t, func() bool { return e.mk.users(0) == 1 })
	until(t, func() bool { return e.mk.users(0) == 0 })
}

// 同時に来た番組表 2 本が、どちらも空きと読んで上限を超えない
func TestKeylessLimitHoldsUnderConcurrency(t *testing.T) {
	e := setup(t, twoGR, grChannels, "", 1)
	statuses := make(chan int, 2)
	for _, ch := range []string{"T27", "T21"} {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			v := url.Values{"type": {"GR"}, "channel": {ch}, "priority": {"3"}, "use": {"epg " + ch}}
			req, _ := http.NewRequestWithContext(ctx, "GET", e.base+"/denpa/stream?"+v.Encode(), nil)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			statuses <- res.StatusCode
			go func() { _, _ = io.Copy(io.Discard, res.Body); res.Body.Close() }()
		}()
	}
	got := []int{<-statuses, <-statuses}
	slices.Sort(got)
	if !slices.Equal(got, []int{200, 409}) {
		t.Fatal(got)
	}
}

// SSE を行ごとに読む
func sseLines(t *testing.T, base string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/denpa/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 100)
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func TestTunerEventsAreCoalesced(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	lines := sseLines(t, e.base)
	// 繋がったときの "[" から出る 1 回を流してから数える
	time.Sleep(300 * time.Millisecond)
	for len(lines) > 0 {
		<-lines
	}
	e.mk.mu.Lock()
	for range 5 {
		e.mk.changed()
	}
	e.mk.mu.Unlock()
	count := 0
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case l := <-lines:
			if l == "event: tuners" {
				count++
			}
			continue
		case <-deadline:
		}
		break
	}
	if count != 1 {
		t.Fatal(count)
	}
}

func TestSSEHeartbeat(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.heartbeat = 50 * time.Millisecond })
	lines := sseLines(t, e.base)
	select {
	case l := <-lines:
		if l != ":" {
			t.Fatalf("%q", l)
		}
	case <-time.After(time.Second):
		t.Fatal("死活確認が来ません")
	}
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

// scanned の種別だけ入れ替え、ほかの種別と知らない項目はそのまま残す (denpa の Config.SaveChannels と同じ)。再起動しても残る
func TestPutChannelsReplacesOnlyScannedTypes(t *testing.T) {
	var file string
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { file = b.cfg.ChannelsFile })
	put := func(body string) int {
		t.Helper()
		req, _ := http.NewRequest("PUT", e.base+"/denpa/channels", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	names := func(raw []json.RawMessage) []string {
		var out []string
		for _, r := range raw {
			var c struct{ Type, Channel string }
			_ = json.Unmarshal(r, &c)
			out = append(out, c.Type+" "+c.Channel)
		}
		slices.Sort(out)
		return out
	}
	get := func() []json.RawMessage {
		t.Helper()
		res, err := http.Get(e.base + "/denpa/channels")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var raw []json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	if s := put(`{"scanned":["GR"],"channels":[{"type":"GR","channel":"T27"},{"type":"GR","channel":"T21"}]}`); s != 200 {
		t.Fatal(s)
	}
	if s := put(`{"scanned":["BS"],"channels":[{"type":"BS","channel":"BS15:16626","extra":{"kept":true}}]}`); s != 200 {
		t.Fatal(s)
	}
	if s := put(`{"scanned":["GR"],"channels":[{"type":"GR","channel":"T16"}]}`); s != 200 {
		t.Fatal(s)
	}
	want := []string{"BS BS15:16626", "GR T16"}
	if got := names(get()); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	// 空や scanned 無しは断り、預かったものを消さない
	for _, body := range []string{`{"scanned":["GR"],"channels":[]}`, `{"channels":[{"type":"GR","channel":"T27"}]}`} {
		if s := put(body); s != 400 {
			t.Fatal(body, s)
		}
	}
	raw := get()
	if got := names(raw); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	if !slices.ContainsFunc(raw, func(r json.RawMessage) bool { return strings.Contains(string(r), `"extra":{"kept":true}`) }) {
		t.Fatalf("unknown fields were dropped: %s", raw)
	}

	again, err := NewBridge(Config{MirakurunURL: "http://127.0.0.1:1", ChannelsFile: file})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(again.stored); !slices.Equal(got, want) {
		t.Fatal(got)
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
