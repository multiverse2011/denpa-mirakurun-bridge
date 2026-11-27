package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"denpa-mirakurun-bridge/internal/channels"
)

func TestDecodeKeyless(t *testing.T) {
	e := setup(t, twoGR, grChannels, "", 9, func(b *Bridge) { b.cfg.DecodeKeyless = true })
	for _, c := range []struct{ ch, use string }{{"T27", "epg T27"}, {"T21", "scan T21"}, {"T27", "logo GR/T27"}} {
		open(t, e.base, q("GR", c.ch, "3", c.use))
		if r := e.mk.lastRequest(); r.Decode != "1" || !strings.HasSuffix(r.Agent, "("+c.use+")") {
			t.Fatalf("%s: %+v", c.use, r)
		}
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

func TestGRMatchedByNumberNotServices(t *testing.T) {
	// 中継局: T16 と T20 は同じ局を流す
	mk := []channels.Mirakurun{{Type: "GR", Channel: "16"}, {Type: "GR", Channel: "20", Services: services(32736, 1024)}}
	var stored []channels.Denpa
	for _, ch := range []string{"T16", "T20"} {
		var d channels.Denpa
		_ = json.Unmarshal([]byte(`{"type":"GR","channel":"`+ch+`","networkId":32736,"services":[{"serviceId":1024}]}`), &d)
		stored = append(stored, d)
	}
	if channels.ToMirakurun("GR", "T16", stored, mk) != "16" || channels.ToMirakurun("GR", "T20", stored, mk) != "20" || channels.ToDenpa("GR", "20", stored, mk) != "T20" {
		t.Fatal("GR mapping")
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
