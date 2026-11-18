package main

import (
	"regexp"
	"strconv"
)

// DenpaChannel は denpa が PUT /denpa/channels で預ける局のうち、対応付けに使う項目。
type DenpaChannel struct {
	Type              string `json:"type"`
	Channel           string `json:"channel"`
	NetworkID         int    `json:"networkId"`
	TransportStreamID int    `json:"transportStreamId"`
	Services          []struct {
		ServiceID   int    `json:"serviceId"`
		ServiceType int    `json:"serviceType"`
		Name        string `json:"name"`
	} `json:"services"`
}

type MirakurunService struct {
	ServiceID int    `json:"serviceId"`
	NetworkID int    `json:"networkId"`
	Type      int    `json:"type"`
	Name      string `json:"name"`
}

type MirakurunChannel struct {
	Type     string             `json:"type"`
	Channel  string             `json:"channel"`
	Services []MirakurunService `json:"services"`
}

var (
	reUHF         = regexp.MustCompile(`^T0*(\d+)$`)
	reDigits      = regexp.MustCompile(`^\d+$`)
	reSatellite   = regexp.MustCompile(`^(?:BS|CS)0*(\d+)(:\d+)?$`)
	reTransponder = regexp.MustCompile(`(?i)^(?:BS|CS)?0*(\d+)(?:\D|$)`)
)

func sameStream(d DenpaChannel, m MirakurunChannel) bool {
	if d.Type != m.Type {
		return false
	}
	for _, ms := range m.Services {
		for _, ds := range d.Services {
			if ms.NetworkID == d.NetworkID && ms.ServiceID == ds.ServiceID {
				return true
			}
		}
	}
	return false
}

func transponder(name string) int {
	m := reTransponder.FindStringSubmatch(name)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func number(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// grByName は預かった局が無いときの従来の引き方 (T<番号> は数字のチャンネル、無ければ完全一致)
func grByName(name string, ofType []MirakurunChannel, exact func() string) string {
	if m := reUHF.FindStringSubmatch(name); m != nil {
		for _, c := range ofType {
			if reDigits.MatchString(c.Channel) && number(c.Channel) == number(m[1]) {
				return c.Channel
			}
		}
	}
	return exact()
}

// toMirakurun は denpa のチャンネル名を Mirakurun のチャンネル名に引く。無ければ ""。
// 地上波は、預かった局と services (networkId と serviceId) のいずれかが一致するチャンネルを選ぶ。
// 頼まれた番号 (T27→27) または名前の完全一致を「指定どおり」とし、指定どおりの中継局が
// 別の局になっていれば断る。指定どおりのチャンネルの services が空なら、そのチャンネルを選ぶ。
// 指定どおりが無ければ同じ局の別の中継局 (複数なら Mirakurun の並び順で先頭) を選ぶ。
// 預かった局が無い (または services が空) ときは、番号、無ければ名前の完全一致で引く。
// 衛星は Mirakurun が TSID を出さないので、預かった局のサービスで引く。
func toMirakurun(typ, name string, stored []DenpaChannel, mk []MirakurunChannel) string {
	var ofType []MirakurunChannel
	for _, c := range mk {
		if c.Type == typ {
			ofType = append(ofType, c)
		}
	}
	exact := func() string {
		for _, c := range ofType {
			if c.Channel == name {
				return c.Channel
			}
		}
		return ""
	}

	if typ == "GR" {
		// 預かった同じ名前の局 (services のあるもの) を探す
		var want *DenpaChannel
		for i := range stored {
			if stored[i].Type == "GR" && stored[i].Channel == name && len(stored[i].Services) > 0 {
				want = &stored[i]
				break
			}
		}
		if want == nil {
			return grByName(name, ofType, exact)
		}
		// 「指定どおり」の Mirakurun のチャンネル: 番号一致 (T27→27) と名前の完全一致 (C13)
		var specified []MirakurunChannel
		seen := map[string]bool{}
		if m := reUHF.FindStringSubmatch(name); m != nil {
			for _, c := range ofType {
				if reDigits.MatchString(c.Channel) && number(c.Channel) == number(m[1]) && !seen[c.Channel] {
					specified = append(specified, c)
					seen[c.Channel] = true
				}
			}
		}
		for _, c := range ofType {
			if c.Channel == name && !seen[c.Channel] {
				specified = append(specified, c)
				seen[c.Channel] = true
			}
		}
		// 1. 指定どおりで、同じ局の中継局
		for _, c := range specified {
			if sameStream(*want, c) {
				return c.Channel
			}
		}
		if len(specified) > 0 {
			// 3. 指定どおりだが Mirakurun に局があり、預かった局と違う (別の局になった)
			for _, c := range specified {
				if len(c.Services) > 0 {
					return ""
				}
			}
			// 4. services が空の指定どおりのチャンネルは、同じ局の別の中継局より優先
			return specified[0].Channel
		}
		// 2. 指定どおりが無い: 同じ局のチャンネル (複数なら Mirakurun の並び順で先頭)
		for _, c := range ofType {
			if sameStream(*want, c) {
				return c.Channel
			}
		}
		// 預かった局があるのに同じ局が無い。別の局を選ばない
		return ""
	}

	for _, d := range stored {
		if d.Type == typ && d.Channel == name {
			for _, c := range ofType {
				if sameStream(d, c) {
					return c.Channel
				}
			}
		}
	}

	m := reSatellite.FindStringSubmatch(name)
	if m == nil {
		return exact()
	}
	// BS は 1 中継器に TS が複数ある。違う TS を流すより断る
	if m[2] != "" && typ == "BS" {
		return ""
	}
	for _, c := range ofType {
		if transponder(c.Channel) == number(m[1]) {
			return c.Channel
		}
	}
	return ""
}

// toDenpa は Mirakurun のチャンネル名を denpa のチャンネル名に引く。
// 地上波は同じ局の denpa チャンネルを先に返し、複数あれば番号が一致するものを優先する
func toDenpa(typ, mkName string, stored []DenpaChannel, mk []MirakurunChannel) string {
	if typ == "GR" {
		var target *MirakurunChannel
		for i := range mk {
			if mk[i].Type == "GR" && mk[i].Channel == mkName {
				target = &mk[i]
				break
			}
		}
		if target != nil {
			var matched []DenpaChannel
			for _, d := range stored {
				if d.Type == "GR" && sameStream(d, *target) {
					matched = append(matched, d)
				}
			}
			if len(matched) > 0 {
				// 同じ局が複数の denpa チャンネルにあるときは、番号が一致するものを優先する
				if reDigits.MatchString(mkName) {
					n := number(mkName)
					for _, d := range matched {
						if m := reUHF.FindStringSubmatch(d.Channel); m != nil && number(m[1]) == n {
							return d.Channel
						}
					}
				}
				return matched[0].Channel
			}
		}
		if reDigits.MatchString(mkName) {
			return "T" + strconv.Itoa(number(mkName))
		}
		return mkName
	}
	for _, c := range mk {
		if c.Type == typ && c.Channel == mkName {
			for _, d := range stored {
				if sameStream(d, c) {
					return d.Channel
				}
			}
		}
	}
	return mkName
}
