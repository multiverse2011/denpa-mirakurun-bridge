package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseEPGMode(t *testing.T) {
	for _, mode := range []string{"broadcast", "synthetic"} {
		if got, err := parseEPGMode(mode, messages{}); err != nil || got != mode {
			t.Fatalf("%s: %q %v", mode, got, err)
		}
	}
	if _, err := parseEPGMode("x", messages{}); err == nil || !strings.Contains(err.Error(), "EPG_MODE は broadcast か synthetic") {
		t.Fatal(err)
	}
	if _, err := parseEPGMode("x", messages{en: true}); err == nil || !strings.HasPrefix(err.Error(), "EPG_MODE must be broadcast or synthetic") {
		t.Fatal(err)
	}
}

func TestEPGServicesFor(t *testing.T) {
	// 地上波はその中継の中だけ
	ids := func(services []epgService) []int {
		var out []int
		for _, s := range services {
			out = append(out, s.ServiceID)
		}
		return out
	}
	if got := ids(epgServicesFor("GR", "27", grChannels)); !slices.Equal(got, []int{1024}) {
		t.Fatalf("GR: %v", got)
	}
	// 衛星は同じ networkId の全局
	channels := []MirakurunChannel{
		{Type: "BS", Channel: "BS15_0", Services: services(4, 101)},
		{Type: "BS", Channel: "BS15_1", Services: services(4, 103)},
		{Type: "BS", Channel: "BS03_0", Services: services(4, 151)},
		{Type: "BS", Channel: "BS01_0", Services: services(5, 191)},
		{Type: "CS", Channel: "CS2", Services: services(6, 296)},
	}
	if got := ids(epgServicesFor("BS", "BS15_0", channels)); !slices.Equal(got, []int{101, 103, 151}) {
		t.Fatalf("BS: %v", got)
	}
	if got := ids(epgServicesFor("BS", "T99", channels)); len(got) != 0 {
		t.Fatalf("見つからないチャンネル: %v", got)
	}
}

// tsSections は応答の TS から指定した PID のセクションを取り出す (CRC は呼び側で見る)
func tsSections(t *testing.T, data []byte, pid int) [][]byte {
	t.Helper()
	var out [][]byte
	var buf []byte
	for at := 0; at+188 <= len(data); at += 188 {
		p := data[at : at+188]
		if p[0] != 0x47 {
			t.Fatalf("%d: sync %02x", at, p[0])
		}
		if int(p[1]&0x1f)<<8|int(p[2]) != pid {
			continue
		}
		payload := p[4:]
		if p[1]&0x40 != 0 {
			pointer := int(payload[0])
			payload = payload[1+pointer:]
			buf = nil
		}
		buf = append(buf, payload...)
		for len(buf) >= 3 {
			if buf[0] == 0xff {
				buf = nil
				break
			}
			length := 3 + (int(buf[1]&0x0f)<<8 | int(buf[2]))
			if len(buf) < length {
				break
			}
			out = append(out, slices.Clone(buf[:length]))
			buf = buf[length:]
		}
	}
	return out
}

func getEPG(t *testing.T, base string, query map[string]string) (int, []byte) {
	t.Helper()
	v := url.Values{}
	for k, x := range query {
		v.Set(k, x)
	}
	res, err := http.Get(base + "/denpa/stream?" + v.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body
}

// denpa は揃った時点で切るので、3 回送り切らなくても送り始めに成功のログを残す
func TestSyntheticEPGLogsWhenStreaming(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.cfg.EPGMode = "synthetic" })
	e.mk.mu.Lock()
	e.mk.programs = []epgProgram{{
		EventID: 1, ServiceID: 1024, NetworkID: 32736,
		StartAt: time.Now().UnixMilli(), Duration: time.Hour.Milliseconds(), Name: "番組",
	}}
	e.mk.mu.Unlock()
	o := open(t, e.base, q("GR", "T27", "3", "epg T27"))
	if o.status != 200 {
		t.Fatal(o.status)
	}
	o.cancel() // 3 回送り切る前に切る
	e.b.mu.Lock()
	found := slices.ContainsFunc(e.b.logs, func(l logLine) bool {
		return strings.Contains(l.Text, "合成して送ります") && strings.Contains(l.Text, "チューナー不使用")
	})
	e.b.mu.Unlock()
	if !found {
		t.Fatal("送り始めの成功ログがありません")
	}
}

func TestSyntheticEPG(t *testing.T) {
	channels := []MirakurunChannel{
		{Type: "GR", Channel: "27", Services: []MirakurunService{{ServiceID: 1024, NetworkID: 32736, Type: 1, Name: "サービスA"}}},
	}
	e := setup(t, oneGR, channels, "", 9, func(b *Bridge) { b.cfg.EPGMode = "synthetic" })
	now := time.Now()
	e.mk.mu.Lock()
	e.mk.programs = []epgProgram{
		{EventID: 100, ServiceID: 1024, NetworkID: 32736, StartAt: now.Add(-10 * time.Minute).UnixMilli(), Duration: 30 * time.Minute.Milliseconds(),
			IsFree: true, Name: "番組A", Description: "説明", Extended: map[string]string{"出演者": "太郎"},
			Genres: []epgGenre{{Lv1: 7, Lv2: 1}}, Video: &epgVideo{StreamContent: 1, ComponentType: 0xb3},
			Audios: []epgAudio{{ComponentType: 3, ComponentTag: 0x11, IsMain: true, SamplingRate: 48000, Langs: []string{"jpn"}}}},
		{EventID: 101, ServiceID: 1024, NetworkID: 32736, StartAt: now.Add(20 * time.Minute).UnixMilli(), Duration: time.Hour.Milliseconds(), IsFree: true, Name: "番組B"},
		{EventID: 102, ServiceID: 1024, NetworkID: 32736, StartAt: now.Add(24 * time.Hour).UnixMilli(), Duration: time.Hour.Milliseconds(), IsFree: true, Name: "番組C"},
		{EventID: 200, ServiceID: 1056, NetworkID: 32737, StartAt: now.Add(time.Hour).UnixMilli(), Duration: time.Hour.Milliseconds(), Name: "別の局"},
	}
	e.mk.mu.Unlock()

	status, body := getEPG(t, e.base, q("GR", "T27", "3", "epg T27"))
	if status != 200 || len(body) == 0 {
		t.Fatalf("status %d, %d バイト", status, len(body))
	}
	// チューナーを使っていない
	if u := e.mk.users(0); u != 0 {
		t.Fatalf("チューナーを使っています: %d", u)
	}

	// PAT: 1024 番
	pat := tsSections(t, body, pidPAT)
	if len(pat) == 0 || crc32MPEG(pat[0]) != 0 {
		t.Fatal("PAT がありません")
	}
	foundService := false
	for at := 8; at+4 <= len(pat[0])-4; at += 4 {
		if int(pat[0][at])<<8|int(pat[0][at+1]) == 1024 {
			foundService = true
		}
	}
	if !foundService {
		t.Fatal("PAT に 1024 がありません")
	}

	// SDT: service_type 1
	sdt := tsSections(t, body, pidSDT)
	if len(sdt) == 0 || crc32MPEG(sdt[0]) != 0 {
		t.Fatal("SDT がありません")
	}
	foundType := false
	for at := 11; at+5 <= len(sdt[0])-4; {
		id := int(sdt[0][at])<<8 | int(sdt[0][at+1])
		n := int(sdt[0][at+3]&0x0f)<<8 | int(sdt[0][at+4])
		desc := sdt[0][at+5 : at+5+n]
		if id == 1024 && len(desc) > 2 && desc[0] == 0x48 && desc[2] == 1 {
			foundType = true
		}
		at += 5 + n
	}
	if !foundType {
		t.Fatal("SDT の service_type が 1 ではありません")
	}

	// EIT: p/f と schedule、対象の局の番組だけ
	eits := tsSections(t, body, pidEIT)
	tables := map[byte]map[byte]bool{}
	ids := map[int]bool{}
	for _, sec := range eits {
		if crc32MPEG(sec) != 0 {
			t.Fatalf("EIT の CRC が合いません: % x", sec[:4])
		}
		if tables[sec[0]] == nil {
			tables[sec[0]] = map[byte]bool{}
		}
		tables[sec[0]][sec[6]] = true
		for _, id := range eventIDs(sec) {
			ids[id] = true
		}
	}
	if !tables[tablePF][0] || !tables[tablePF][1] {
		t.Fatalf("p/f がありません: %v", tables[tablePF])
	}
	if len(tables[tableSchedule]) == 0 || len(tables[tableSchedule+1]) == 0 {
		t.Fatalf("schedule がありません: %v", tables)
	}
	for _, id := range []int{100, 101, 102} {
		if !ids[id] {
			t.Fatalf("番組 %d がありません", id)
		}
	}
	if ids[200] {
		t.Fatal("別の局の番組が混ざっています")
	}
}

// 取得が遅い間も、ほかの要求が epgMu で待たされない (取得はロックの外)。
// 1 本目は 2 本目が到着するまで返らないので、時間ではなく順序で確かめる
func TestEPGProgramsFetchesWithoutHoldingTheLock(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	e.mk.mu.Lock()
	e.mk.programsStart = started
	e.mk.programsRelease = release
	e.mk.mu.Unlock()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()

	first := make(chan error, 1)
	go func() {
		_, err := e.b.epgPrograms(context.Background())
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("取得が始まりません")
	}
	second := make(chan error, 1)
	go func() {
		_, err := e.b.epgPrograms(context.Background())
		second <- err
	}()
	// 1 本目がバリアで止まっている間に 2 本目が返れば、ロックで待たされていない
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("2 本目の取得がロックで待たされています")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls, peak := e.mk.programsStats(); calls != 2 || peak != 2 {
		t.Fatalf("取得が直列化されています: calls=%d peak=%d", calls, peak)
	}
}

// 先に始まって遅く終わった取得が、後から始まって先に終わった取得の控えを上書きしない
func TestEPGProgramsKeepsNewerSnapshot(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	e.mk.mu.Lock()
	e.mk.programsStart = started
	e.mk.programsRelease = release
	e.mk.programSets = [][]epgProgram{{{EventID: 1}}, {{EventID: 2}}}
	e.mk.mu.Unlock()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()

	first := make(chan error, 1)
	go func() {
		_, err := e.b.epgPrograms(context.Background())
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("取得が始まりません")
	}
	second := make(chan error, 1)
	go func() {
		_, err := e.b.epgPrograms(context.Background())
		second <- err
	}()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("2 本目の取得が返りません")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}

	got, err := e.b.epgPrograms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EventID != 2 {
		t.Fatalf("古い取得で控えを上書きしています: %+v", got)
	}
}

// 呼び出し元が切れても、共有する取得は止めない
func TestEPGProgramsIgnoresCallerCancel(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	started := make(chan struct{}, 1)
	e.mk.mu.Lock()
	e.mk.programsDelay = 300 * time.Millisecond
	e.mk.programsStart = started
	e.mk.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.b.epgPrograms(ctx)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("取得が始まりません")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("呼び出し元の切断で取得が止まりました: %v", err)
	}
	// 次の要求は控えから返り、取得は増えない
	calls, _ := e.mk.programsStats()
	if _, err := e.b.epgPrograms(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after, _ := e.mk.programsStats(); after != calls {
		t.Fatalf("控えを使っていません: %d -> %d", calls, after)
	}
}

// 対象局の番組が 1 件も無ければ、空の TS を返さず従来の中継に落ちる
func TestSyntheticEPGFallsBackWithoutPrograms(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.cfg.EPGMode = "synthetic" })
	e.mk.mu.Lock()
	e.mk.programs = []epgProgram{}
	e.mk.mu.Unlock()
	o := open(t, e.base, q("GR", "T27", "3", "epg T27"))
	if o.status != 200 {
		t.Fatal(o.status)
	}
	until(t, func() bool { return e.mk.users(0) == 1 })
	o.cancel()
	e.b.mu.Lock()
	found := slices.ContainsFunc(e.b.logs, func(l logLine) bool { return strings.Contains(l.Text, "番組がありません") })
	e.b.mu.Unlock()
	if !found {
		t.Fatal("フォールバックのログがありません")
	}
}

// synthetic でも live・rec・scan はチューナーを使う (合成するのは use=epg だけ)
func TestSyntheticModeLeavesLiveRecScan(t *testing.T) {
	e := setup(t, twoGR, grChannels, "", 9, func(b *Bridge) { b.cfg.EPGMode = "synthetic" })
	open(t, e.base, q("GR", "T27", "9", "live a"))
	until(t, func() bool { return e.mk.users(0) == 1 })
	open(t, e.base, q("GR", "T21", "10", "rec 1"))
	until(t, func() bool { return e.mk.users(1) == 1 })
	open(t, e.base, q("GR", "T27", "3", "scan T27"))
	until(t, func() bool { return e.mk.users(0) == 2 })
}

// 局の情報が引けなければ、番組表も従来のストリーム中継に落ちる
func TestSyntheticEPGFallsBackWithoutServices(t *testing.T) {
	e := setup(t, oneGR, []MirakurunChannel{{Type: "GR", Channel: "27"}}, "", 9, func(b *Bridge) { b.cfg.EPGMode = "synthetic" })
	o := open(t, e.base, q("GR", "T27", "3", "epg T27"))
	if o.status != 200 {
		t.Fatal(o.status)
	}
	until(t, func() bool { return e.mk.users(0) == 1 })
	o.cancel()
}

// /api/programs が読めなければ、従来のストリーム中継に落ちる
func TestSyntheticEPGFallback(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.cfg.EPGMode = "synthetic" })
	e.mk.mu.Lock()
	e.mk.noPrograms = true
	e.mk.mu.Unlock()
	o := open(t, e.base, q("GR", "T27", "3", "epg T27"))
	if o.status != 200 {
		t.Fatal(o.status)
	}
	until(t, func() bool { return e.mk.users(0) == 1 })
	o.cancel()
	e.b.mu.Lock()
	found := slices.ContainsFunc(e.b.logs, func(l logLine) bool { return strings.Contains(l.Text, "合成して送ります") })
	e.b.mu.Unlock()
	if found {
		t.Fatal("フォールバックしたのに「合成して送ります」のログが出ています")
	}
}
