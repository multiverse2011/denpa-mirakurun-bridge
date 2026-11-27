package main

import (
	"context"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"denpa-mirakurun-bridge/internal/channels"
)

var reKeyless = regexp.MustCompile(`^(epg|logo|scan) `)

// 掛かっていない SI しか読まない利用者 (denpa の Sink.WantsKeys と同じ)
func keyless(use string) bool { return reKeyless.MatchString(use) }

type mkUser struct {
	ID            string `json:"id"`
	Priority      int    `json:"priority"`
	Agent         string `json:"agent"`
	StreamSetting *struct {
		Channel *struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
		} `json:"channel"`
	} `json:"streamSetting"`
}

func (u mkUser) on(typ, ch string) bool {
	return u.StreamSetting != nil && u.StreamSetting.Channel != nil &&
		u.StreamSetting.Channel.Type == typ && u.StreamSetting.Channel.Channel == ch
}

type mkTuner struct {
	Index       int      `json:"index"`
	Name        string   `json:"name"`
	Types       []string `json:"types"`
	Pid         *int     `json:"pid"`
	Users       []mkUser `json:"users"`
	IsAvailable bool     `json:"isAvailable"`
	IsFault     bool     `json:"isFault"`
}

type active struct {
	use       string
	priority  int
	typ       string
	mkChannel string
	mkUserID  string
	handedOff atomic.Bool
	cancel    context.CancelFunc
}

// 引き継ぎ (after)。前の利用者は正常に終わらせる
func (a *active) handOff() {
	a.handedOff.Store(true)
	a.cancel()
}

type pendingReq struct{ typ, mkChannel string }

// claim は取り上げ中の要求。切られた側の掛け直しに、空いたチューナーを先に取らせないために置く
type claim struct {
	types    []string // 取り上げたチューナーが受けられる種別 (BS と CS は同じチューナーを使う)
	priority int
}

func (b *Bridge) ours() map[string]*active {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.oursLocked()
}

func (b *Bridge) oursLocked() map[string]*active {
	m := map[string]*active{}
	for a := range b.active {
		if a.mkUserID != "" {
			m[a.mkUserID] = a
		}
	}
	return m
}

func (b *Bridge) tuners(ctx context.Context) ([]map[string]any, error) {
	var list []mkTuner
	var mkChannels []channels.Mirakurun
	if err := b.mk(ctx, "/api/tuners", &list); err != nil {
		return nil, err
	}
	if err := b.mk(ctx, "/api/channels", &mkChannels); err != nil {
		return nil, err
	}
	mine := b.ours()
	b.mu.Lock()
	stored := b.parsed
	b.mu.Unlock()

	out := []map[string]any{}
	for _, t := range list {
		stopping := !t.IsAvailable && !t.IsFault
		var channel, rank, errText any
		for _, u := range t.Users {
			if c := u.StreamSetting; c != nil && c.Channel != nil {
				channel = map[string]string{"type": c.Channel.Type, "channel": channels.ToDenpa(c.Channel.Type, c.Channel.Channel, stored, mkChannels)}
				break
			}
		}
		// Mirakurun は空きを定義の順に使う
		if t.IsAvailable {
			rank = t.Index
		}
		if t.IsFault {
			errText = b.msg.t("Mirakurun reports a fault on this tuner", "Mirakurun がこのチューナーの異常を報告しています")
		}
		users := []map[string]any{}
		// 止めている最中は、空きに数えられないよう仮の利用者を先頭に 1 件入れる。
		// denpa のロゴの空き数 (logo.ts の countFree) は users が空かで数える。
		// Mirakurun がまだ利用者を返している間も、その後ろにそのまま並べる
		if stopping {
			users = append(users, map[string]any{"use": b.msg.t("mirakurun stopping", "mirakurun 停止中"), "priority": -1})
		}
		for _, u := range t.Users {
			if a, ok := mine[u.ID]; ok {
				users = append(users, map[string]any{"use": a.use, "priority": a.priority})
			} else {
				name := u.Agent
				if name == "" {
					name = u.ID
				}
				users = append(users, map[string]any{"use": "mirakurun " + name, "priority": u.Priority})
			}
		}
		// 止めている最中は channel を返さない。denpa の保留表示や相乗りの判定は channel で
		// 一致を見るので、止める途中のチューナーに載せない
		if stopping {
			channel = nil
		}
		// isAvailable=false は「止めている最中」のことが多い (Mirakurun は止め終わるまで false)。
		// 無効は異常 (isFault) のときだけにする。止めている最中は rank を返さない
		// pin は denpa v2.30.0 から必須 (device が無ければ name)。無いと設定画面の一覧のキーが重複して描画が落ちる。
		// shortcut: tuner=<pin> での名指しは受けず Mirakurun に選ばせる、名指しが要るなら stream で t.Name を見る
		out = append(out, map[string]any{
			"index": t.Index, "name": t.Name, "pin": t.Name, "types": t.Types, "disabled": t.IsFault, "rank": rank,
			"device": nil, "lnb": nil, "channel": channel, "users": users, "pid": t.Pid, "error": errText,
		})
	}
	return out, nil
}

// 番組表・ロゴ・スキャンに新しくチューナーを取らせてよいか。選ぶのは Mirakurun で、ここは取りに行かせるかだけ決める。
// 優先度 0 以上の利用者だけを塞いでいるとみなす (Mirakurun 自身の番組表取得 -1 は誰にでも取り上げられる)。
func (b *Bridge) keylessMayTune(ctx context.Context, typ, mkChannel string) (bool, error) {
	// 自分の利用者と pending は Mirakurun に聞く前に、同じロックの中で写す。別々に写すと pending から active へ
	// 移ったものがどちらにも載らない (上限を超える) か、両方に載る (偽の 409) ことがある
	b.mu.Lock()
	mine := b.oursLocked()
	pending := slices.Collect(maps.Keys(b.pending))
	b.mu.Unlock()
	var list []mkTuner
	if err := b.mk(ctx, "/api/tuners", &list); err != nil {
		return false, err
	}
	held := 0
	sharing := map[string]bool{}
	for _, t := range list {
		var blocking []mkUser
		for _, u := range t.Users {
			if u.Priority >= 0 {
				blocking = append(blocking, u)
			}
		}
		for _, u := range blocking {
			if u.on(typ, mkChannel) {
				return true, nil
			}
		}
		if !slices.Contains(t.Types, typ) {
			continue
		}
		for _, s := range t.Types {
			sharing[s] = true
		}
		if len(blocking) > 0 && !slices.ContainsFunc(blocking, func(u mkUser) bool {
			a, ok := mine[u.ID]
			return !ok || !keyless(a.use)
		}) {
			held++
		}
	}
	waiting := map[pendingReq]bool{}
	for _, p := range pending {
		if sharing[p.typ] {
			waiting[*p] = true
		}
	}
	return held+len(waiting) < b.cfg.KeylessTuners, nil
}

// denpa の約束 2 (優先度の低い利用者しかいないチューナーは取り上げる) のうち、こちらの利用者どうしの分。
// Mirakurun は相手の優先度が自分より小さいときしか取り上げないので、PRIORITY_MAP で同じ値に潰すと
// ライブ (9) が番組表 (3) を待ち続ける。比べるのは denpa の優先度。空きか同じチャンネルの選局があれば何もしない。
// 取り上げたときは claim を置く。返す unclaim は要求が決着したら (繋がった・断られた) 呼ぶ。
func (b *Bridge) preempt(ctx context.Context, typ, mkChannel string, priority int) (unclaim func(), err error) {
	unclaim = func() {}
	var list []mkTuner
	if err := b.mk(ctx, "/api/tuners", &list); err != nil {
		return unclaim, err
	}
	mine := b.ours()
	var victims []*active
	var victimTypes []string
	lowest := 0
	for _, t := range list {
		if !t.IsAvailable || !slices.Contains(t.Types, typ) {
			continue
		}
		var here []*active
		top, takable := math.MinInt, true
		for _, u := range t.Users {
			if u.Priority < 0 { // Mirakurun 自身の番組表取得は Mirakurun が退かす
				continue
			}
			if u.on(typ, mkChannel) {
				return unclaim, nil
			}
			a, ok := mine[u.ID]
			if !ok || a.priority >= priority {
				takable = false
				continue
			}
			here = append(here, a)
			top = max(top, a.priority)
		}
		if takable && len(here) == 0 {
			return unclaim, nil
		}
		if takable && (victims == nil || top < lowest) {
			victims, victimTypes, lowest = here, t.Types, top
		}
	}
	if victims == nil {
		return unclaim, nil
	}
	// 切る前に置く。切られた側の掛け直しは、置いたあとに来る
	c := &claim{victimTypes, priority}
	b.mu.Lock()
	b.claims[c] = struct{}{}
	b.mu.Unlock()
	unclaim = func() {
		b.mu.Lock()
		delete(b.claims, c)
		b.mu.Unlock()
	}
	gone := map[string]bool{}
	for _, a := range victims {
		b.logf("taking a tuner from %s (priority %d) for priority %d", "優先度 %[3]d のため %[1]s (優先度 %[2]d) からチューナーを取り上げます",
			a.use, a.priority, priority)
		gone[a.mkUserID] = true
		a.cancel()
	}
	// Mirakurun が利用者を外すまで待つ。待たずに頼むと、空きを探して 12.5 秒粘った末の 503 になりうる
	for range 50 {
		select {
		case <-ctx.Done():
			return unclaim, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if err := b.mk(ctx, "/api/tuners", &list); err != nil {
			return unclaim, err
		}
		if !slices.ContainsFunc(list, func(t mkTuner) bool {
			return slices.ContainsFunc(t.Users, func(u mkUser) bool { return gone[u.ID] })
		}) {
			return unclaim, nil
		}
	}
	var uses []string
	for _, a := range victims {
		uses = append(uses, a.use)
	}
	b.logf("%s still on the tuner 5s after being taken off; tuning without waiting", "取り上げた %s が 5 秒たってもチューナーから外れません。待たずに選局します",
		strings.Join(uses, ", "))
	return unclaim, nil
}

// typ のチューナーを、priority より高い要求が取り上げている最中か。
// shortcut: その間は別のチューナーに空きや相乗りがあっても 409 にする (denpa が掛け直す)、困るなら claim にチューナー名を持たせて絞る
func (b *Bridge) yielding(typ string, priority int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.claims {
		if c.priority > priority && slices.Contains(c.types, typ) {
			return true
		}
	}
	return false
}
