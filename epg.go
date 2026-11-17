package main

// 実験的機能 (EPG_MODE=synthetic)。use=epg を Mirakurun の番組表から合成して返す。

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Mirakurun の番組表を取り直す間隔。EPG の取得は数分おきなので、これで足りる
const epgProgramsTTL = time.Minute

func isEPGUse(use string) bool {
	return use == "epg" || strings.HasPrefix(use, "epg ")
}

// epgServicesFor は denpa の要求チャンネルに載っている局を返す。
// 地上波は中継 (Mirakurun のチャンネル) の中の局だけ。衛星は denpa がネットワーク全局を
// 期待するので、同じ networkId の局を全部含める。
func epgServicesFor(typ, mkChannel string, channels []MirakurunChannel) []epgService {
	var self *MirakurunChannel
	for i := range channels {
		if channels[i].Type == typ && channels[i].Channel == mkChannel {
			self = &channels[i]
			break
		}
	}
	if self == nil || len(self.Services) == 0 {
		return nil
	}
	networks := map[int]bool{}
	for _, s := range self.Services {
		networks[s.NetworkID] = true
	}
	seen := map[[2]int]bool{}
	var out []epgService
	add := func(s MirakurunService) {
		key := [2]int{s.NetworkID, s.ServiceID}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, epgService{NetworkID: s.NetworkID, ServiceID: s.ServiceID, Type: s.Type, Name: s.Name})
	}
	if typ == "GR" {
		for _, s := range self.Services {
			add(s)
		}
	} else {
		for _, c := range channels {
			if c.Type != typ {
				continue
			}
			for _, s := range c.Services {
				if networks[s.NetworkID] {
					add(s)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NetworkID != out[j].NetworkID {
			return out[i].NetworkID < out[j].NetworkID
		}
		return out[i].ServiceID < out[j].ServiceID
	})
	return out
}

// epgPrograms は Mirakurun の番組表を返す。一定の間隔だけ覚えておく
func (b *Bridge) epgPrograms(ctx context.Context) ([]epgProgram, error) {
	b.epgMu.Lock()
	cached, at := b.epgCached, b.epgCachedAt
	b.epgMu.Unlock()
	if cached != nil && time.Since(at) < epgProgramsTTL {
		return cached, nil
	}
	// 控えは全要求で共有するので、取得は呼び出し元の切断に引きずられない。
	// 同時に 2 本取っても害はない。期限は b.mk が付ける
	var list []epgProgram
	if err := b.mk(context.WithoutCancel(ctx), "/api/programs", &list); err != nil {
		return nil, err
	}
	b.epgMu.Lock()
	b.epgCached, b.epgCachedAt = list, time.Now()
	b.epgMu.Unlock()
	return list, nil
}

// syntheticEPG は use=epg に Mirakurun の番組表から組んだ EIT を返す。
// チューナーを使わないので、keyless の予算にも /denpa/tuners の users にも載らない。
// 組めなければ false を返し、呼び側が従来のストリーム中継に落とす
func (b *Bridge) syntheticEPG(w http.ResponseWriter, r *http.Request, typ, channel, mkChannel string, mkChannels []MirakurunChannel) bool {
	services := epgServicesFor(typ, mkChannel, mkChannels)
	if len(services) == 0 {
		b.logf("cannot synthesize EPG for %s %s: Mirakurun has no services for the channel", "%s %s の番組表を合成できません: Mirakurun にこのチャンネルの局がありません", typ, channel)
		return false
	}
	programs, err := b.epgPrograms(r.Context())
	if err != nil {
		b.logf("cannot synthesize EPG for %s %s (%v); relaying Mirakurun's stream", "%s %s の番組表を合成できないので Mirakurun のストリームを中継します (%v)", typ, channel, err)
		return false
	}
	byService := map[[2]int][]epgProgram{}
	for _, p := range programs {
		key := [2]int{p.NetworkID, p.ServiceID}
		byService[key] = append(byService[key], p)
	}

	// 同じ内容を何度か流す。denpa は番組が増えたときに「揃った」かを見るので、
	// 1 回だけだと空のセクションが最後に届いたときに判定されないことがある
	now := time.Now().UnixMilli()
	ts := newTSWriter()
	w.Header().Set("Content-Type", "video/MP2T")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	for pass := 0; pass < 3; pass++ {
		if pass > 0 {
			select {
			case <-r.Context().Done():
				return true
			case <-time.After(200 * time.Millisecond):
			}
		}
		var buf bytes.Buffer
		tsid := services[0].NetworkID // Mirakurun は TSID を返さないので、ネットワーク ID で代用する
		buf.Write(ts.section(pidPAT, patSection(tsid, services)))
		buf.Write(ts.section(pidSDT, sdtSection(tsid, services[0].NetworkID, services)))
		for _, svc := range services {
			for _, section := range serviceEITSections(svc, byService[[2]int{svc.NetworkID, svc.ServiceID}], now) {
				buf.Write(ts.section(pidEIT, section))
			}
		}
		_ = rc.SetWriteDeadline(time.Now().Add(b.writeTimeout))
		if _, err := w.Write(buf.Bytes()); err != nil {
			return true
		}
		if err := rc.Flush(); err != nil {
			return true
		}
	}
	b.logf("served EPG for %s %s from Mirakurun's programs (%d services, no tuner)", "%s %s の番組表を Mirakurun の番組表から合成しました (%d 局、チューナー不使用)", typ, channel, len(services))
	return true
}
