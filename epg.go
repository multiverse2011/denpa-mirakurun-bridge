package main

// 実験的機能 (EPG_MODE=synthetic)。use=epg を Mirakurun の番組表から合成して返す。

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"denpa-mirakurun-bridge/internal/arib"
	"denpa-mirakurun-bridge/internal/channels"
)

// Mirakurun の番組表を取り直す間隔。EPG の取得は数分おきなので、これで足りる
const epgProgramsTTL = time.Minute

func isEPGUse(use string) bool {
	return use == "epg" || strings.HasPrefix(use, "epg ")
}

// epgServicesFor は denpa の要求チャンネルに載っている局を返す。
// 地上波は中継 (Mirakurun のチャンネル) の中の局だけ。衛星は denpa がネットワーク全局を
// 期待するので、同じ networkId の局を全部含める。
func epgServicesFor(typ, mkChannel string, mkChannels []channels.Mirakurun) []arib.Service {
	var self *channels.Mirakurun
	for i := range mkChannels {
		if mkChannels[i].Type == typ && mkChannels[i].Channel == mkChannel {
			self = &mkChannels[i]
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
	var out []arib.Service
	add := func(s channels.MirakurunService) {
		key := [2]int{s.NetworkID, s.ServiceID}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, arib.Service{NetworkID: s.NetworkID, ServiceID: s.ServiceID, Type: s.Type, Name: s.Name})
	}
	if typ == "GR" {
		for _, s := range self.Services {
			add(s)
		}
	} else {
		for _, c := range mkChannels {
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

// addStoredGroundServices は合成 SDT の地上波の顔ぶれを denpa のスキャン結果に合わせる。
// 預かったチャンネル (channels.json) の局のうち、Mirakurun の services に無いものを足す。
// 足す局の種別は預かった値を使い、種別が無い (0) 局は足さない。名前は空にする
// (denpa の widenServices が古い channels.json の名前で DB の局名を半角に戻すのを避ける。
// compareGround は名前を見ない)。
// 見つかったときは、預かったチャンネルの transportStreamId も返す (0 なら呼び側は今の値を使う)
func addStoredGroundServices(channel string, services []arib.Service, stored []channels.Denpa) ([]arib.Service, int) {
	var self *channels.Denpa
	for i := range stored {
		if stored[i].Type == "GR" && stored[i].Channel == channel {
			self = &stored[i]
			break
		}
	}
	if self == nil {
		return services, 0
	}
	have := map[int]bool{}
	for _, s := range services {
		have[s.ServiceID] = true
	}
	out := slices.Clone(services)
	for _, s := range self.Services {
		if s.ServiceID == 0 || s.ServiceType == 0 || have[s.ServiceID] {
			continue
		}
		have[s.ServiceID] = true
		out = append(out, arib.Service{NetworkID: self.NetworkID, ServiceID: s.ServiceID, Type: s.ServiceType})
	}
	return out, self.TransportStreamID
}

// epgPrograms は Mirakurun の番組表を返す。一定の間隔だけ覚えておく
func (b *Bridge) epgPrograms(ctx context.Context) ([]arib.Program, error) {
	b.epgMu.Lock()
	cached, at := b.epgCached, b.epgCachedAt
	b.epgMu.Unlock()
	if cached != nil && time.Since(at) < epgProgramsTTL {
		return cached, nil
	}
	// 控えは全要求で共有するので、取得は呼び出し元の切断に引きずられない。
	// 期限は b.mk が付ける。同時に 2 本取ることがあるので、控えは取得を始めた時刻が
	// 新しいものだけを残す (遅れて終わった古い取得で新しい控えを潰さない)
	started := time.Now()
	var list []arib.Program
	if err := b.mk(context.WithoutCancel(ctx), "/api/programs", &list); err != nil {
		return nil, err
	}
	b.epgMu.Lock()
	if started.After(b.epgCachedAt) {
		b.epgCached, b.epgCachedAt = list, started
	}
	list = b.epgCached
	b.epgMu.Unlock()
	return list, nil
}

// syntheticEPG は use=epg に Mirakurun の番組表から組んだ EIT を返す。
// チューナーを使わないので、keyless の予算にも /denpa/tuners の users にも載らない。
// 組めなければ false を返し、呼び側が従来のストリーム中継に落とす
func (b *Bridge) syntheticEPG(w http.ResponseWriter, r *http.Request, typ, channel, mkChannel string, mkChannels []channels.Mirakurun) bool {
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
	byService := map[[2]int][]arib.Program{}
	for _, p := range programs {
		key := [2]int{p.NetworkID, p.ServiceID}
		byService[key] = append(byService[key], p)
	}

	now := time.Now().UnixMilli()
	empty := true
	for _, svc := range services {
		if arib.HasSynthesizablePrograms(byService[[2]int{svc.NetworkID, svc.ServiceID}], now) {
			empty = false
			break
		}
	}
	if empty {
		b.logf("cannot synthesize EPG for %s %s: Mirakurun returned no programs for its services; relaying Mirakurun's stream", "%s %s の番組表を合成できないので Mirakurun のストリームを中継します: 対象の局の番組がありません", typ, channel)
		return false
	}

	// 地上波の SDT だけは、denpa が預けたチャンネルの局も載せる (denpa はスキャン結果と顔ぶれを比べる)。
	// EIT の services と PAT の services は Mirakurun のまま (PAT の TSID は SDT と揃える)。
	// EIT は arib.ServiceEITSections が svc.NetworkID を transport_stream_id に使うので、
	// networkId と TSID が違う実機では TS 全体で 2 種類になり得る (実機の地上波では同じ)。
	// denpa の EpgReader は EIT の TSID を見ず serviceId で読むため、今は実害が無い
	sdtServices := services
	tsid := services[0].NetworkID // Mirakurun は TSID を返さないので、ネットワーク ID で代用する
	if typ == "GR" {
		b.mu.Lock()
		stored := slices.Clone(b.parsed)
		b.mu.Unlock()
		var storedTSID int
		sdtServices, storedTSID = addStoredGroundServices(channel, services, stored)
		if storedTSID != 0 {
			tsid = storedTSID
		}
	}

	// 同じ内容を何度か流す。denpa は番組が増えたときに「揃った」かを見るので、
	// 1 回だけだと空のセクションが最後に届いたときに判定されないことがある
	ts := arib.NewTSWriter()
	w.Header().Set("Content-Type", "video/MP2T")
	w.WriteHeader(200)
	// denpa は揃った時点で切るので、最後まで送れたかではなく、送り始めたことを残す
	b.logf("synthesizing EPG for %s %s from Mirakurun's programs (%d services, no tuner)", "%s %s の番組表を合成して送ります (%d 局、チューナー不使用)", typ, channel, len(services))
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
		buf.Write(ts.Section(arib.PIDPAT, arib.PATSection(tsid, services)))
		buf.Write(ts.Section(arib.PIDSDT, arib.SDTSection(tsid, services[0].NetworkID, sdtServices)))
		for _, svc := range services {
			for _, section := range arib.ServiceEITSections(svc, byService[[2]int{svc.NetworkID, svc.ServiceID}], now) {
				buf.Write(ts.Section(arib.PIDEIT, section))
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
	return true
}
