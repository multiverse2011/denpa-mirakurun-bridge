package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	MirakurunURL  string
	PriorityMap   map[int]int
	ChannelsFile  string
	AllowedHosts  []string
	Version       string
	KeylessTuners int
	English       bool
	Pairing       *Pairing // nil なら平文の HTTP (v2.27.0 以前の denpa 向け)
}

// messages は応答とログの言葉を選ぶ。コードには英語を先に書き、既定 (en=false) では日本語を出す
type messages struct{ en bool }

func (m messages) t(en, ja string, args ...any) string {
	if !m.en {
		en = ja
	}
	return fmt.Sprintf(en, args...)
}

func parsePriorityMap(text string, m messages) (map[int]int, error) {
	out := map[int]int{}
	for _, pair := range strings.Split(text, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		from, to, ok := strings.Cut(pair, ":")
		f, err1 := strconv.Atoi(from)
		t, err2 := strconv.Atoi(to)
		if !ok || err1 != nil || err2 != nil {
			return nil, errors.New(m.t("cannot parse PRIORITY_MAP: %s (e.g. 10:2,9:1)", "PRIORITY_MAP を読めません: %s (例: 10:2,9:1)", pair))
		}
		out[f] = t
	}
	return out, nil
}

// Mirakurun の HTTP API は 0 未満を 400 で断る
func mapPriority(p int, m map[int]int) int {
	if v, ok := m[p]; ok {
		p = v
	}
	return max(p, 0)
}

var reKeyless = regexp.MustCompile(`^(epg|logo|scan) `)

// 掛かっていない SI しか読まない利用者 (denpa の Sink.WantsKeys と同じ)
func keyless(use string) bool { return reKeyless.MatchString(use) }

// DNS リバインディング避け。denpa のエージェントと同じ決まり
func hostAllowed(host string, extra []string) bool {
	if host == "" {
		return false
	}
	if strings.HasPrefix(host, "[") {
		return true
	}
	name := strings.ToLower(host)
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	if net.ParseIP(name) != nil || name == "localhost" || !strings.Contains(name, ".") {
		return true
	}
	for _, s := range []string{".localhost", ".local", ".lan", ".home.arpa", ".internal", ".svc"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	for _, e := range extra {
		x := strings.ToLower(strings.TrimSpace(e))
		if x == "" {
			continue
		}
		if strings.HasPrefix(x, ".") && (name == x[1:] || strings.HasSuffix(name, x)) || name == x {
			return true
		}
	}
	return false
}

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

type logLine struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

// listener は SSE の購読者 1 人。種類ごとに 1 つだけ溜め、読み直しを重ねない
type listener struct{ tuners, channels chan struct{} }

type Bridge struct {
	cfg       Config
	msg       messages
	client    *http.Client
	mu        sync.Mutex
	keylessMu sync.Mutex
	saveMu    sync.Mutex
	active    map[*active]struct{}
	pending   map[*pendingReq]struct{}
	listeners map[*listener]struct{}
	stored    []json.RawMessage
	parsed    []DenpaChannel
	logs      []logLine

	recording atomic.Int32  // 開いている録画の選局 (Mirakurun の応答待ちも数える)
	stopping  chan struct{} // 閉じたら SSE の購読者を帰す
	stopOnce  sync.Once     // Shutdown を呼ぶたびに Stop も呼ばれるので、閉じるのは一度だけ

	stallTimeout time.Duration // Mirakurun から何も来ないまま待つ長さ
	writeTimeout time.Duration // 受け手が読まないまま待つ長さ
	heartbeat    time.Duration // SSE の死活確認の間隔
	eventDelay   time.Duration // Mirakurun の通知をまとめる長さ
	drainPoll    time.Duration // 止まる前に録画の終わりを見に行く間隔
}

func NewBridge(cfg Config) (*Bridge, error) {
	b := &Bridge{
		cfg:          cfg,
		msg:          messages{cfg.English},
		client:       &http.Client{},
		active:       map[*active]struct{}{},
		pending:      map[*pendingReq]struct{}{},
		listeners:    map[*listener]struct{}{},
		stallTimeout: 30 * time.Second,
		writeTimeout: 30 * time.Second,
		heartbeat:    30 * time.Second,
		eventDelay:   100 * time.Millisecond,
		drainPoll:    time.Second,
		stopping:     make(chan struct{}),
	}
	if _, err := url.Parse(cfg.MirakurunURL); err != nil {
		return nil, errors.New(b.msg.t("MIRAKURUN_URL is not a URL: %v", "MIRAKURUN_URL が URL として読めません: %v", err))
	}
	data, err := os.ReadFile(cfg.ChannelsFile)
	if err == nil {
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", cfg.ChannelsFile, err)
		}
		b.setStored(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return b, nil
}

func (b *Bridge) setStored(raw []json.RawMessage) {
	parsed := make([]DenpaChannel, len(raw))
	for i, r := range raw {
		_ = json.Unmarshal(r, &parsed[i])
	}
	b.stored, b.parsed = raw, parsed
}

// logf は英語と日本語の書式を受け取り、設定の言葉で書く
func (b *Bridge) logf(en, ja string, args ...any) {
	text := b.msg.t(en, ja, args...)
	log.Print("[bridge] " + text)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs = append(b.logs, logLine{time.Now().UnixMilli(), text})
	if len(b.logs) > 2000 {
		b.logs = b.logs[1:]
	}
}

func (b *Bridge) emit(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for l := range b.listeners {
		c := l.tuners
		if name == "channels" {
			c = l.channels
		}
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (b *Bridge) mk(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.MirakurunURL+path, nil)
	if err != nil {
		return err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return err
	}
	// 読み切ってから閉じると、Mirakurun への接続が使い回せる
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Mirakurun %s -> %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
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
	var channels []MirakurunChannel
	if err := b.mk(ctx, "/api/tuners", &list); err != nil {
		return nil, err
	}
	if err := b.mk(ctx, "/api/channels", &channels); err != nil {
		return nil, err
	}
	mine := b.ours()
	b.mu.Lock()
	stored := b.parsed
	b.mu.Unlock()

	out := []map[string]any{}
	for _, t := range list {
		var channel, rank, errText any
		for _, u := range t.Users {
			if c := u.StreamSetting; c != nil && c.Channel != nil {
				channel = map[string]string{"type": c.Channel.Type, "channel": toDenpa(c.Channel.Type, c.Channel.Channel, stored, channels)}
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
		out = append(out, map[string]any{
			"index": t.Index, "name": t.Name, "types": t.Types, "disabled": !t.IsAvailable, "rank": rank,
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

func (b *Bridge) stream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ, channel, use := q.Get("type"), q.Get("channel"), q.Get("use")
	if use == "" {
		use = "unknown"
	}
	// 見分け方は denpa のエージェントの TunerPool.Recording と同じ
	if use == "rec" || strings.HasPrefix(use, "rec ") {
		b.recording.Add(1)
		defer b.recording.Add(-1)
	}
	priority, err := strconv.Atoi(q.Get("priority"))
	if q.Get("priority") == "" {
		priority, err = 0, nil
	}
	if !slices.Contains([]string{"GR", "BS", "CS", "SKY"}, typ) || channel == "" || err != nil {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("check type, channel and priority", "type・channel・priority を確かめてください")})
		return
	}

	var mkChannels []MirakurunChannel
	if err := b.mk(r.Context(), "/api/channels", &mkChannels); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	mkChannel := toMirakurun(typ, channel, b.parsed, mkChannels)
	var leaving []*active
	for a := range b.active {
		if slices.Contains(q["after"], a.use) {
			leaving = append(leaving, a)
		}
	}
	b.mu.Unlock()
	if mkChannel == "" {
		writeJSON(w, 404, map[string]string{"error": b.msg.t("Mirakurun has no channel matching %s %s", "Mirakurun に %s %s に当たるチャンネルがありません", typ, channel)})
		return
	}

	// 別のチャンネルへの引き継ぎは先に離して空きを見せる。同じチャンネルなら選局を共有したまま、繋がってから離す
	for _, a := range leaving {
		if a.typ != typ || a.mkChannel != mkChannel {
			a.handOff()
		}
	}
	release := sync.OnceFunc(func() {
		for _, a := range leaving {
			a.handOff()
		}
	})
	defer release()

	// 数えてから pending に載せるまでを 1 本ずつ通す。同時に来た 2 本がどちらも空きと読まないように。
	// pending は利用者として数えられる (active に載る) まで残す
	asking := &pendingReq{typ, mkChannel}
	if keyless(use) {
		b.keylessMu.Lock()
		ok, err := b.keylessMayTune(r.Context(), typ, mkChannel)
		if err == nil && ok {
			b.mu.Lock()
			b.pending[asking] = struct{}{}
			b.mu.Unlock()
		}
		b.keylessMu.Unlock()
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, 409, map[string]string{"error": b.msg.t("at most %d tuners may be used for EPG, logos and scans (KEYLESS_TUNERS)",
				"番組表・ロゴ・スキャンに使えるチューナーは %d 台までです (KEYLESS_TUNERS)", b.cfg.KeylessTuners)})
			return
		}
	}
	donePending := func() {
		b.mu.Lock()
		delete(b.pending, asking)
		b.mu.Unlock()
	}
	defer donePending()

	decode := "1"
	if keyless(use) {
		decode = "0"
	}
	mkPriority := mapPriority(priority, b.cfg.PriorityMap)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/channels/%s/%s/stream?decode=%s", b.cfg.MirakurunURL, typ, url.PathEscape(mkChannel), decode), nil)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("X-Mirakurun-Priority", strconv.Itoa(mkPriority))
	req.Header.Set("User-Agent", fmt.Sprintf("denpa-mirakurun-bridge/%s (%s)", b.cfg.Version, use))
	// 何も送ってこない Mirakurun・読まない受け手に、チューナーをいつまでも握らせない (応答ヘッダ待ちも含む)
	var stalled atomic.Bool
	idle := time.AfterFunc(b.stallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer idle.Stop()
	res, err := b.client.Do(req)
	release()
	if err != nil {
		if stalled.Load() {
			b.logf("gave up on %s %s (%s): no response from Mirakurun for %v", "%s %s (%s) は Mirakurun から %v 応答が無いので諦めました",
				typ, channel, use, b.stallTimeout)
		}
		if r.Context().Err() == nil {
			writeJSON(w, 502, map[string]string{"error": b.msg.t("cannot reach Mirakurun: %v", "Mirakurun に繋がりません: %v", err)})
		}
		return
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(res.Body, 500))
		status := 500
		switch res.StatusCode {
		case 503: // Mirakurun の「空きが無い」。denpa は 409 を空き待ちとして掛け直す
			status = 409
		case 404:
			status = 404
		}
		b.logf("cannot open %s %s (%s, priority %d→%d): Mirakurun %d", "%s %s (%s, priority %d→%d) を開けません: Mirakurun %d",
			typ, channel, use, priority, mkPriority, res.StatusCode)
		writeJSON(w, status, map[string]string{"error": fmt.Sprintf("Mirakurun %d: %s", res.StatusCode, text)})
		return
	}

	me := &active{use: use, priority: priority, typ: typ, mkChannel: mkChannel,
		mkUserID: res.Header.Get("X-Mirakurun-Tuner-User-ID"), cancel: cancel}
	b.mu.Lock()
	b.active[me] = struct{}{}
	delete(b.pending, asking) // active への移し替えは 1 回のロックで (keylessMayTune の写しと食い違わないように)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.active, me)
		b.mu.Unlock()
		b.emit("tuners")
	}()
	b.emit("tuners")

	w.Header().Set("Content-Type", "video/MP2T")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{}) //nolint:errcheck // keep-alive で次の要求に期限を持ち越さない
	idle.Reset(b.stallTimeout)
	buf := make([]byte, 64*1024)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			idle.Reset(b.stallTimeout)
			_ = rc.SetWriteDeadline(time.Now().Add(b.writeTimeout))
			_, werr := w.Write(buf[:n])
			if werr == nil {
				werr = rc.Flush()
			}
			if werr != nil {
				if errors.Is(werr, os.ErrDeadlineExceeded) {
					b.logf("closed %s: not read for %v", "%s が %v 読まないので切りました", use, b.writeTimeout)
				}
				return
			}
		}
		if err != nil {
			break
		}
	}
	if stalled.Load() {
		b.logf("closed %s %s (%s): nothing from Mirakurun for %v", "%s %s (%s) は Mirakurun から %v 何も来ないので切りました",
			typ, channel, use, b.stallTimeout)
	}
	// Mirakurun 側から終わった (取り上げ・失敗) なら、正常終了と読まれないよう接続ごと壊す
	if !me.handedOff.Load() && r.Context().Err() == nil {
		panic(http.ErrAbortHandler)
	}
}

func (b *Bridge) putChannels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channels []json.RawMessage `json:"channels"`
		Scanned  []string          `json:"scanned"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("cannot parse the body as JSON", "本文を JSON として読めません")})
		return
	}
	if body.Channels == nil || len(body.Scanned) == 0 {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("channels and scanned are required", "channels と scanned が必要です")})
		return
	}
	// 空で上書きすると録れていた局まで消える (エージェントと同じ断り方)
	if len(body.Channels) == 0 {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("no channels given", "チャンネルが 1 件もありません")})
		return
	}
	// 保存は saveMu で 1 本ずつ通し、b.mu はディスク I/O (fsync) の間握らない
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	b.mu.Lock()
	var next []json.RawMessage
	for i, raw := range b.stored {
		if !slices.Contains(body.Scanned, b.parsed[i].Type) {
			next = append(next, raw)
		}
	}
	b.mu.Unlock()
	next = append(next, body.Channels...)
	data, _ := json.MarshalIndent(next, "", "  ")
	err := os.MkdirAll(filepath.Dir(b.cfg.ChannelsFile), 0o755)
	if err == nil {
		err = writeSynced(b.cfg.ChannelsFile+".tmp", data, 0o644)
	}
	if err == nil {
		err = os.Rename(b.cfg.ChannelsFile+".tmp", b.cfg.ChannelsFile)
	}
	if err == nil {
		err = syncDir(filepath.Dir(b.cfg.ChannelsFile))
	}
	if err == nil {
		b.mu.Lock()
		b.setStored(next)
		b.mu.Unlock()
	}
	if err != nil {
		b.logf("cannot save channels: %v", "チャンネルを保存できません: %v", err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	b.emit("channels")
	writeJSON(w, 200, next)
}

// rename の前に中身をディスクへ落とし、電源断で空の channels.json が残らないようにする
func writeSynced(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	// 書きかけが前から残っていると OpenFile の perm は効かない
	err = f.Chmod(perm)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// rename もディスクへ落とす。落とさないと 200 を返した保存が電源断で前の中身に戻りうる
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func (b *Bridge) events(w http.ResponseWriter, r *http.Request) {
	l := &listener{make(chan struct{}, 1), make(chan struct{}, 1)}
	b.mu.Lock()
	b.listeners[l] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.listeners, l)
		b.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{}) //nolint:errcheck
	// 時々コメント行を送る。黙って消えた購読者は書いてみないと分からず、
	// denpa (Bun の fetch) は無通信が続くと TimeoutError で切るため
	ping := time.NewTicker(b.heartbeat)
	defer ping.Stop()
	send := func(text string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(b.writeTimeout))
		_, err := io.WriteString(w, text)
		return err == nil && rc.Flush() == nil
	}
	ok := send("")
	for ok {
		select {
		case <-r.Context().Done():
			return
		case <-b.stopping:
			return
		case <-l.tuners:
			ok = send("event: tuners\ndata: {}\n\n")
		case <-l.channels:
			ok = send("event: channels\ndata: {}\n\n")
		case <-ping.C:
			ok = send(":\n\n")
		}
	}
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TLS ではクライアント証明書が要り、ブラウザは繋げないので、名前と Content-Type は見ない (denpa のエージェントと同じ)
	if b.cfg.Pairing != nil {
		if !b.gate(w, r) {
			return
		}
	} else if !hostAllowed(r.Host, b.cfg.AllowedHosts) {
		b.logf("refused unknown host %q (403); add it to ALLOWED_HOSTS",
			"知らない名前「%s」で呼ばれたので断りました (403)。ALLOWED_HOSTS に足してください", r.Host)
		writeJSON(w, 403, map[string]string{"error": "unknown host"})
		return
	}
	// ブラウザが下調べ無しに送れる形 (text/plain など) の書き込みは受けない
	if b.cfg.Pairing == nil && (r.Method == "PUT" || r.Method == "POST") && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, 415, map[string]string{"error": b.msg.t("only application/json is accepted", "application/json だけ受け付けます")})
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /denpa/stream":
		b.stream(w, r)
	case "GET /denpa/tuners":
		list, err := b.tuners(r.Context())
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"tuners": list, "detected": false})
	case "PUT /denpa/tuners":
		writeJSON(w, 405, map[string]string{"error": b.msg.t("change tuners in Mirakurun (tuners.yml)",
			"チューナーの定義は Mirakurun 側 (tuners.yml) で変えてください")})
	case "GET /denpa/channels":
		b.mu.Lock()
		list := b.stored
		b.mu.Unlock()
		if list == nil {
			list = []json.RawMessage{}
		}
		writeJSON(w, 200, list)
	case "PUT /denpa/channels":
		b.putChannels(w, r)
	case "GET /denpa/events":
		b.events(w, r)
	case "GET /denpa/card":
		// 復号は Mirakurun の decoder が選局時に行う
		writeJSON(w, 200, map[string]any{"ok": true, "message": "", "source": "local", "readers": []any{}})
	case "POST /denpa/decode":
		writeJSON(w, 501, map[string]any{"ok": false, "error": b.msg.t("decoding after recording is not supported (Mirakurun's decoder decodes while tuning)",
			"録画後の復号には対応していません (復号は選局時に Mirakurun の decoder が行います)")})
	case "GET /denpa/logs":
		b.mu.Lock()
		lines := slices.Clone(b.logs)
		b.mu.Unlock()
		if lines == nil {
			lines = []logLine{}
		}
		writeJSON(w, 200, map[string]any{"lines": lines})
	case "GET /denpa/about":
		osName := runtime.GOOS
		if osName == "darwin" {
			osName = "macos"
		}
		arch := runtime.GOARCH
		if arch == "amd64" {
			arch = "x64"
		}
		writeJSON(w, 200, map[string]any{
			"version": b.cfg.Version, "os": osName, "arch": arch,
			"runtime": fmt.Sprintf("Go %s / Mirakurun %s", runtime.Version(), b.cfg.MirakurunURL), "ccidDriver": nil,
		})
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

// Drain は録画の選局が全部終わるまで (limit まで) 待つ。その間もふだんどおり受け、止まる途中に来た録画も流す。
// denpa は録画が終わるまで待ってから止まるので、流しているこちらが先に消えると録画が切れる (denpa のエージェントの Drain と同じ)
func (b *Bridge) Drain(limit time.Duration) {
	if limit <= 0 || b.recording.Load() == 0 {
		return
	}
	b.logf("asked to stop; waiting for recordings to finish (up to %v)", "止まれの合図。録画が終わるまで待ちます (上限 %v)", limit)
	until := time.Now().Add(limit)
	for b.recording.Load() > 0 && time.Now().Before(until) {
		time.Sleep(min(b.drainPoll, time.Until(until))) // 上限を越えて待たない
	}
	if b.recording.Load() > 0 {
		b.logf("gave up waiting; stopping", "待ちきれないので畳みます")
	} else {
		b.logf("recordings finished; stopping", "録画が終わったので畳みます")
	}
}

// Stop は SSE の購読者を帰す。denpa は開きっぱなしにするので、帰さないと http.Server.Shutdown が期限まで待つ
func (b *Bridge) Stop() { b.stopOnce.Do(func() { close(b.stopping) }) }

// Mirakurun のチューナーの出来事を tuners として流す。中身は読まない (denpa が /denpa/tuners を読み直す)。
// 1 回の選局で出来事は続けて届くので、eventDelay のあいだはまとめて 1 回にする
func (b *Bridge) Watch(ctx context.Context) {
	var armed atomic.Bool
	notify := func() {
		if armed.CompareAndSwap(false, true) {
			time.AfterFunc(b.eventDelay, func() {
				armed.Store(false)
				b.emit("tuners")
			})
		}
	}
	wait := time.Second
	for ctx.Err() == nil {
		err := func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.MirakurunURL+"/api/events/stream?resource=tuner", nil)
			if err != nil {
				return err
			}
			res, err := b.client.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("status %d", res.StatusCode)
			}
			wait = time.Second
			buf := make([]byte, 4096)
			for {
				if _, err := res.Body.Read(buf); err != nil {
					return err
				}
				notify()
			}
		}()
		if ctx.Err() != nil {
			return
		}
		b.logf("cannot follow Mirakurun events (retrying in %v): %v", "Mirakurun の通知に繋げません (%v 後に再試行): %v", wait, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}
