package channels

import (
	"encoding/json"
	"testing"
)

func services(networkID int, ids ...int) []MirakurunService {
	var out []MirakurunService
	for _, id := range ids {
		out = append(out, MirakurunService{ServiceID: id, NetworkID: networkID, Type: 1})
	}
	return out
}

// denpa が預けた局を組み立てる (サービスは serviceId だけ)
func storedChannel(t *testing.T, typ, name string, networkID int, serviceIDs ...int) Denpa {
	t.Helper()
	services := make([]map[string]int, len(serviceIDs))
	for i, id := range serviceIDs {
		services[i] = map[string]int{"serviceId": id}
	}
	body, _ := json.Marshal(map[string]any{"type": typ, "channel": name, "networkId": networkID, "services": services})
	var d Denpa
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGRMatchStored(t *testing.T) {
	// 同じ局が 2 つの物理チャンネルにある (16 と 20)
	both := []Mirakurun{
		{Type: "GR", Channel: "16", Services: services(32736, 1024)},
		{Type: "GR", Channel: "20", Services: services(32736, 1024)},
	}
	// 16 は別の局、20 が預かった局
	changed := []Mirakurun{
		{Type: "GR", Channel: "16", Services: services(1111, 999)},
		{Type: "GR", Channel: "20", Services: services(32736, 1024)},
	}
	storedT16 := storedChannel(t, "GR", "T16", 32736, 1024)
	storedT20 := storedChannel(t, "GR", "T20", 32736, 1024)
	storedT27 := storedChannel(t, "GR", "T27", 32736, 1024)
	storedC13 := storedChannel(t, "GR", "C13", 32736, 1024)

	for _, c := range []struct {
		name    string
		request string
		stored  []Denpa
		mk      []Mirakurun
		want    string
	}{
		// 番号も局も一致する中継局を選ぶ
		{"番号どおり", "T20", []Denpa{storedT16, storedT20}, both, "20"},
		// 番号が一致するチャンネルが無ければ、同じ局の別の中継局を選ぶ
		{"同じ局の別の中継局", "T27", []Denpa{storedT27}, changed, "20"},
		// 番号が一致するチャンネルが別の局なら断る
		{"番号は合うが別の局", "T16", []Denpa{storedT16}, changed, ""},
		// 数字でない名前でも、預かった局で引く
		{"数字でない名前", "C13", []Denpa{storedC13},
			[]Mirakurun{{Type: "GR", Channel: "MX", Services: services(32736, 1024)}}, "MX"},
		// 数字でない名前で、預かった局が無ければ今までどおり (完全一致)
		{"数字でない名前・預かり無し", "MX", nil,
			[]Mirakurun{{Type: "GR", Channel: "MX", Services: services(32736, 1024)}}, "MX"},
		// services が空の番号一致は、今までどおり番号で選ぶ
		{"services が空", "T27", []Denpa{storedT27},
			[]Mirakurun{{Type: "GR", Channel: "27"}, {Type: "GR", Channel: "20", Services: services(32736, 1024)}}, "27"},
		// 名前が完全一致し services が空なら、同じ局の別名 (MX) より指定どおり
		{"exact の services が空", "C13", []Denpa{storedC13},
			[]Mirakurun{{Type: "GR", Channel: "C13"}, {Type: "GR", Channel: "MX", Services: services(32736, 1024)}}, "C13"},
		// 数字の名前でも、Mirakurun に同じ名前で services 空があれば exact
		{"exact の services が空 (T27)", "T27", []Denpa{storedT27},
			[]Mirakurun{{Type: "GR", Channel: "T27"}}, "T27"},
		// 同じ局が複数あり、番号一致が無ければ設定順の先頭
		{"同じ局が複数", "T27", []Denpa{storedT27}, both, "16"},
		// 預かった局が無ければ番号で選ぶ
		{"預かり無し", "T20", nil, changed, "20"},
		// 預かった局で同じ局が無ければ、別の局を選ばない
		{"同じ局が無い", "T27", []Denpa{storedT27}, changed[:1], ""},
	} {
		if got := ToMirakurun("GR", c.request, c.stored, c.mk); got != c.want {
			t.Errorf("%s: %q != %q", c.name, got, c.want)
		}
	}
}

func TestGRToDenpaPrefersStored(t *testing.T) {
	mk20same := []Mirakurun{{Type: "GR", Channel: "20", Services: services(32736, 1024)}}
	mkMXsame := []Mirakurun{{Type: "GR", Channel: "MX", Services: services(32736, 1024)}}
	mk16same := []Mirakurun{{Type: "GR", Channel: "16", Services: services(32736, 1024)}}
	for _, c := range []struct {
		name   string
		mkName string
		stored []Denpa
		mk     []Mirakurun
		want   string
	}{
		// 実機相当: 16 のストリームは T16
		{"実機相当", "16", []Denpa{storedChannel(t, "GR", "T16", 32736, 1024)}, mk16same, "T16"},
		// 同じ局が複数の denpa チャンネルにあるときは番号が一致するもの
		{"番号が一致", "20", []Denpa{
			storedChannel(t, "GR", "T16", 32736, 1024),
			storedChannel(t, "GR", "T20", 32736, 1024),
		}, mk20same, "T20"},
		// 数字でない Mirakurun 名でも、同じ局の denpa 名を返す
		{"数字でない名前", "MX", []Denpa{storedChannel(t, "GR", "C13", 32736, 1024)}, mkMXsame, "C13"},
		// 番号は一致しないが、同じ局なら denpa 名を返す
		{"番号が違う denpa 名", "20", []Denpa{storedChannel(t, "GR", "C13", 32736, 1024)}, mk20same, "C13"},
		// 対象のチャンネルの services が空なら従来どおり (番号)
		{"対象の services が空", "20", []Denpa{storedChannel(t, "GR", "T16", 32736, 1024)},
			[]Mirakurun{{Type: "GR", Channel: "20"}}, "T20"},
		// 預かった局の services が空なら従来どおり (番号)
		{"預かった局の services が空", "16", []Denpa{storedChannel(t, "GR", "T16", 0)}, mk16same, "T16"},
		// 預かった局に同じ局が無ければ今までどおり
		{"一致しない", "20", []Denpa{storedChannel(t, "GR", "C13", 1111, 999)}, mk20same, "T20"},
		{"預かり無し", "20", nil, mk20same, "T20"},
	} {
		if got := ToDenpa("GR", c.mkName, c.stored, c.mk); got != c.want {
			t.Errorf("%s: %q != %q", c.name, got, c.want)
		}
	}
}
