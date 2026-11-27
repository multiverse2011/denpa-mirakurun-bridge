package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"denpa-mirakurun-bridge/internal/channels"
)

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

	var mkChannels []channels.Mirakurun
	if err := b.mk(r.Context(), "/api/channels", &mkChannels); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	mkChannel := channels.ToMirakurun(typ, channel, b.parsed, mkChannels)
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

	// 実験的機能。合成できなければ、下の従来のストリーム中継に落ちる
	if b.cfg.EPGMode == "synthetic" && isEPGUse(use) {
		if b.syntheticEPG(w, r, typ, channel, mkChannel, mkChannels) {
			return
		}
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
	if b.yielding(typ, priority) {
		writeJSON(w, 409, map[string]string{"error": b.msg.t("a higher-priority request is taking a tuner", "優先度の高い要求がチューナーを取り上げている最中です")})
		return
	}

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

	unclaim, err := b.preempt(r.Context(), typ, mkChannel, priority)
	defer unclaim()
	if err != nil && r.Context().Err() == nil {
		// 取り上げられなくても Mirakurun に頼むことはできる。断られたら 409 になる
		b.logf("cannot look for a tuner to take over for %s %s (%v); tuning without taking one", "%s %s のために取り上げるチューナーを探せませんでした (%v)。取り上げずに選局します", typ, channel, err)
	}

	decode := "1"
	if keyless(use) && !b.cfg.DecodeKeyless {
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
		b.logf("cannot open %s %s (%s, priority denpa %d / Mirakurun %d): Mirakurun returned %d", "%s %s (%s, 優先度 denpa %d / Mirakurun %d) を開けません: Mirakurun が %d を返しました",
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
	unclaim() // 繋がれば Mirakurun の利用者として見えるので、もう要らない
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
					b.logf("closed %s: the receiver read nothing for %v", "%s の受け手が %v 読まないので切りました", use, b.writeTimeout)
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
