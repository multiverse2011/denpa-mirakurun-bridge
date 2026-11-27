package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

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
