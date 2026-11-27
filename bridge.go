package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"denpa-mirakurun-bridge/internal/arib"
	"denpa-mirakurun-bridge/internal/atomicfile"
	"denpa-mirakurun-bridge/internal/channels"
)

type logLine struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

type Bridge struct {
	cfg       Config
	msg       messages
	client    *http.Client
	mu        sync.Mutex
	keylessMu sync.Mutex
	saveMu    sync.Mutex
	active    map[*active]struct{}
	pending   map[*pendingReq]struct{}
	claims    map[*claim]struct{}
	listeners map[*listener]struct{}
	stored    []json.RawMessage
	parsed    []channels.Denpa
	logs      []logLine

	epgMu       sync.Mutex // Mirakurun の番組表の控え
	epgCached   []arib.Program
	epgCachedAt time.Time

	recording atomic.Int32  // 開いている録画の選局 (Mirakurun の応答待ちも数える)
	stopping  chan struct{} // 閉じたら SSE の購読者を帰す
	stopOnce  sync.Once     // Shutdown を呼ぶたびに Stop も呼ばれるので、閉じるのは一度だけ

	stallTimeout time.Duration // Mirakurun から何も来ないまま待つ長さ
	writeTimeout time.Duration // 受け手が読まないまま待つ長さ
	heartbeat    time.Duration // SSE の死活確認の間隔
	eventDelay   time.Duration // Mirakurun の通知をまとめる長さ
	drainPoll    time.Duration // 止まる前に録画の終わりを見に行く間隔

	cardID string // /denpa/card で名乗る仮のカードの番号 (loadCardID)
}

// loadCardID は /denpa/card で名乗る仮のカードの番号 (10 進 16 桁) を dir/card-id から読む。無ければ乱数で作って書く。
// denpa は契約の結果をこの番号で覚えるので、再起動や MIRAKURUN_URL の書き換えで変えず、ブリッジごとに違える
// (同じ番号のブリッジ同士は、片方で確かめた結果がもう片方にも効いてしまう)。
// 2^48 以上にして実物の B-CAS 番号 (48 ビット) と重ならないようにする。denpa のデータ放送の getIRDID もこれを渡さない。
// 読めない・壊れている・書けないときも、この起動の間だけ使う番号を返す (err はその理由)。壊れたファイルは上書きしない
func loadCardID(dir string) (string, error) {
	const lo, hi = 1 << 48, 10_000_000_000_000_000
	name := filepath.Join(dir, "card-id")
	n, err := rand.Int(rand.Reader, big.NewInt(hi-lo))
	if err != nil {
		return "", err
	}
	fresh := fmt.Sprintf("%016d", lo+n.Int64())
	data, err := os.ReadFile(name)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if n, err := strconv.ParseUint(id, 10, 64); err != nil || len(id) != 16 || n < lo {
			return fresh, fmt.Errorf("%s: %q", name, id)
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fresh, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fresh, err
	}
	return fresh, atomicfile.WritePrivate(name, []byte(fresh+"\n"))
}

func NewBridge(cfg Config) (*Bridge, error) {
	b := &Bridge{
		cfg:          cfg,
		msg:          messages{cfg.English},
		client:       &http.Client{},
		active:       map[*active]struct{}{},
		pending:      map[*pendingReq]struct{}{},
		claims:       map[*claim]struct{}{},
		listeners:    map[*listener]struct{}{},
		stallTimeout: 30 * time.Second,
		writeTimeout: 30 * time.Second,
		heartbeat:    30 * time.Second,
		eventDelay:   100 * time.Millisecond,
		drainPoll:    time.Second,
		stopping:     make(chan struct{}),
	}
	// scheme を書き忘れた "mirakurun:40772" も url.Parse は通す (scheme が mirakurun になる)
	if u, err := url.Parse(cfg.MirakurunURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New(b.msg.t("MIRAKURUN_URL must be an http(s) URL such as http://mirakurun:40772: %s",
			"MIRAKURUN_URL は http://mirakurun:40772 のような http(s) の URL です: %s", cfg.MirakurunURL))
	}
	// 仮のカードは補助なので、番号を保存できなくても止まらない
	var err error
	if b.cardID, err = loadCardID(filepath.Dir(cfg.ChannelsFile)); b.cardID == "" {
		return nil, err
	} else if err != nil {
		b.logf("could not keep the stand-in card number (%v); using %s until restart. Decode checks in denpa will not carry over after a restart",
			"仮のカードの番号を保存できません (%v)。この起動の間は %s を使います。再起動すると番号が変わり、denpa で確かめた復号の結果は引き継がれません", err, b.cardID)
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
	parsed := make([]channels.Denpa, len(raw))
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

// Drain は録画の選局が全部終わるまで (limit まで) 待つ。その間もふだんどおり受け、止まる途中に来た録画も流す。
// denpa は録画が終わるまで待ってから止まるので、流しているこちらが先に消えると録画が切れる (denpa のエージェントの Drain と同じ)
func (b *Bridge) Drain(limit time.Duration) {
	if limit <= 0 || b.recording.Load() == 0 {
		return
	}
	b.logf("asked to stop; waiting up to %v for recordings to finish", "停止を求められました。録画が終わるまで最大 %v 待ちます", limit)
	until := time.Now().Add(limit)
	for b.recording.Load() > 0 && time.Now().Before(until) {
		time.Sleep(min(b.drainPoll, time.Until(until))) // 上限を越えて待たない
	}
	if b.recording.Load() > 0 {
		b.logf("recordings still running after %v; stopping anyway", "%v 待っても録画が終わらないので止まります", limit)
	} else {
		b.logf("recordings finished; stopping", "録画が終わったので止まります")
	}
}

// Stop は SSE の購読者を帰す。denpa は開きっぱなしにするので、帰さないと http.Server.Shutdown が期限まで待つ
func (b *Bridge) Stop() { b.stopOnce.Do(func() { close(b.stopping) }) }
