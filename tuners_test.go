package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"denpa-mirakurun-bridge/internal/channels"
)

func TestBusyBecomes409(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	open(t, e.base, q("GR", "T27", "10", "rec 1"))
	if s := open(t, e.base, q("GR", "T21", "10", "rec 2")).status; s != 409 {
		t.Fatal(s)
	}
}

// Mirakurun がチューナーを止めている最中 (isAvailable=false, isFault=false) は「無効」にしない。
// 異常 (isFault) のときだけ無効にする
func TestStoppingTunerIsNotDisabled(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	state := func(available, fault bool) map[string]any {
		e.mk.mu.Lock()
		e.mk.devices[0].available, e.mk.devices[0].fault = available, fault
		e.mk.mu.Unlock()
		return getTuners(t, e.base)[0]
	}

	if got := state(true, false); got["disabled"] != false || got["rank"] != 0.0 {
		t.Fatalf("空き: disabled=%v rank=%v", got["disabled"], got["rank"])
	}
	// 止めている最中は無効にせず、channel を返さず、空きに数えられないよう仮の利用者を 1 件入れる
	stopping := state(false, false)
	if stopping["disabled"] != false || stopping["rank"] != nil || stopping["channel"] != nil {
		t.Fatalf("止めている最中: disabled=%v rank=%v channel=%v", stopping["disabled"], stopping["rank"], stopping["channel"])
	}
	users, _ := stopping["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("止めている最中の users: %v", stopping["users"])
	}
	if u, _ := users[0].(map[string]any); u["use"] != "mirakurun 停止中" || u["priority"] != -1.0 {
		t.Fatalf("止めている最中の仮の利用者: %v", u)
	}

	if got := state(false, true); got["disabled"] != true {
		t.Fatalf("異常: disabled=%v", got["disabled"])
	}
	if got := state(true, true); got["disabled"] != true {
		t.Fatalf("異常 (空きと報告): disabled=%v", got["disabled"])
	}
	if users, _ := state(false, true)["users"].([]any); len(users) != 0 {
		t.Fatalf("異常の users: %v", users)
	}

	// Mirakurun がまだ利用者を返している止める途中でも、channel は返さず、
	// 仮の利用者を先頭にして実利用者をその後ろに並べる
	e.mk.mu.Lock()
	e.mk.devices[0].available, e.mk.devices[0].fault = false, false
	e.mk.devices[0].users = append(e.mk.devices[0].users, &mockUser{id: "stopping", priority: -1, agent: "Mirakurun:getServices()"})
	e.mk.mu.Unlock()
	got := getTuners(t, e.base)[0]
	if got["channel"] != nil {
		t.Fatalf("利用者があるときの channel: %v", got["channel"])
	}
	users, _ = got["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("利用者があるときの users: %v", users)
	}
	if u, _ := users[0].(map[string]any); u["use"] != "mirakurun 停止中" || u["priority"] != -1.0 {
		t.Fatalf("仮の利用者が先頭にありません: %v", users[0])
	}
	if use, _ := users[1].(map[string]any)["use"].(string); use != "mirakurun Mirakurun:getServices()" {
		t.Fatalf("実利用者が後ろにありません: %q", use)
	}

	// MESSAGE_LANG=en では "mirakurun stopping"
	en := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.msg = messages{en: true} })
	en.mk.mu.Lock()
	en.mk.devices[0].available, en.mk.devices[0].fault = false, false
	en.mk.mu.Unlock()
	enUsers, _ := getTuners(t, en.base)[0]["users"].([]any)
	if len(enUsers) != 1 {
		t.Fatalf("EN の users: %v", enUsers)
	}
	if u, _ := enUsers[0].(map[string]any); u["use"] != "mirakurun stopping" || u["priority"] != -1.0 {
		t.Fatalf("EN の仮の利用者: %v", u)
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
	mkChannels := append(slices.Clone(bsChannels), channels.Mirakurun{Type: "CS", Channel: "CS2", Services: services(6, 296)})
	e := setup(t, [][]string{{"BS", "CS"}, {"BS", "CS"}}, mkChannels, "", 1)
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
