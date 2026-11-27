package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

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

// denpaRawProbeVerdict は denpa v2.51.0 の probeRaw (scramble.ts) の判定を写したもの。
// 結果は visible / invisible / unknown のいずれか。
// 本文が JSON でなければ unknown。404 で error が文字列かつ "not found" 以外なら invisible。
func denpaRawProbeVerdict(status int, body map[string]any, jsonBody, shaMatches bool) string {
	if !jsonBody {
		return "unknown"
	}
	if status == 200 {
		if _, ok := body["sha256"].(string); ok {
			if shaMatches {
				return "visible"
			}
			return "invisible"
		}
	}
	if status == 404 {
		if text, ok := body["error"].(string); ok && text != "not found" {
			return "invisible"
		}
	}
	return "unknown"
}

func TestAgentAPIsAddedInV251(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", e.base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out
	}
	// 置き場は見ないので「確かめられない」。「見えない (警告になる)」ではない
	status, body := post("/denpa/raw/probe", `{"path":".denpa/probe-1-abc"}`)
	if status != 501 || denpaRawProbeVerdict(status, body, true, false) != "unknown" {
		t.Fatalf("raw/probe: %d %v", status, body)
	}
	// 判定の写しが denpa と一致すること (404 は本文で意味が変わる)
	for _, c := range []struct {
		status           int
		body             map[string]any
		json, shaMatches bool
		want             string
	}{
		{404, map[string]any{"error": "not found"}, true, false, "unknown"},
		{404, map[string]any{"error": "missing"}, true, false, "invisible"},
		{404, map[string]any{"error": ""}, true, false, "invisible"},
		{404, map[string]any{}, true, false, "unknown"},
		{200, map[string]any{"sha256": "abc"}, true, true, "visible"},
		{200, map[string]any{"sha256": "abc"}, true, false, "invisible"},
		{200, map[string]any{}, true, false, "unknown"},
		{501, map[string]any{"error": "no"}, true, false, "unknown"},
		{200, nil, false, false, "unknown"},
	} {
		if got := denpaRawProbeVerdict(c.status, c.body, c.json, c.shaMatches); got != c.want {
			t.Fatalf("%d %v: %s", c.status, c.body, got)
		}
	}
	// 仮のカードを 1 枚名乗る。source が remote だと denpa の getActiveCardId は readers の ids を見ない
	res, err := http.Get(e.base + "/denpa/card")
	if err != nil {
		t.Fatal(err)
	}
	var card struct {
		OK      bool   `json:"ok"`
		Source  string `json:"source"`
		Readers []struct {
			Name         string
			Card, Active bool
			IDs          []string `json:"ids"`
		} `json:"readers"`
	}
	err = json.NewDecoder(res.Body).Decode(&card)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || !card.OK || card.Source != "local" || len(card.Readers) != 1 ||
		card.Readers[0].Name == "" || !card.Readers[0].Card || !card.Readers[0].Active ||
		!slices.Equal(card.Readers[0].IDs, []string{e.b.cardID}) {
		t.Fatalf("card: %v %d %+v", err, res.StatusCode, card)
	}
	// 仮のリーダーは止められない (denpa は本文を設定画面に出す)
	req, _ := http.NewRequest("PUT", e.base+"/denpa/card/readers", strings.NewReader(`{"disabled":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 405 {
		t.Fatalf("card/readers: %v %v", err, res)
	} else {
		res.Body.Close()
	}
	status, body = post("/denpa/card/confirm", `{"verification":"AA==","date":61000}`)
	if status != 501 || body["ok"] != false {
		t.Fatalf("card/confirm: %d %v", status, body)
	}
}

func TestAboutNameAndRuntime(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.cfg.AgentName = " 書斎 " })
	res, err := http.Get(e.base + "/denpa/about")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["name"] != "書斎" {
		t.Fatalf("name: %v", out["name"])
	}
	runtime, _ := out["runtime"].(string)
	if !strings.HasPrefix(runtime, "denpa-mirakurun-bridge "+version+" / Go ") || !strings.HasSuffix(runtime, " / 上流 "+e.mk.URL) {
		t.Fatalf("runtime: %q", runtime)
	}
	en := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.msg = messages{en: true} })
	resEN, err := http.Get(en.base + "/denpa/about")
	if err != nil {
		t.Fatal(err)
	}
	defer resEN.Body.Close()
	outEN := map[string]any{}
	if err := json.NewDecoder(resEN.Body).Decode(&outEN); err != nil {
		t.Fatal(err)
	}
	if runtime, _ := outEN["runtime"].(string); !strings.HasSuffix(runtime, " / upstream "+en.mk.URL) {
		t.Fatalf("runtime (en): %q", runtime)
	}

	// 空なら name を返さない (denpa は URL のホスト名で埋める)
	plain := setup(t, oneGR, grChannels, "", 9)
	res, err = http.Get(plain.base + "/denpa/about")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out = map[string]any{}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["name"]; ok {
		t.Fatalf("name must be omitted: %v", out["name"])
	}
}

// 平文にはペアリングが無いので、DELETE /denpa/pair は断る
func TestUnpairRequiresTLS(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	req, _ := http.NewRequest("DELETE", e.base+"/denpa/pair", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("status: %d", res.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if msg, _ := body["error"].(string); !strings.Contains(msg, "TLS") {
		t.Fatalf("error: %v", body)
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
