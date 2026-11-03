# denpa-mirakurun-bridge

[denpa](https://github.com/danything/denpa) のチューナーエージェント API (`/denpa/*`) を [Mirakurun](https://github.com/Chinachu/Mirakurun) に中継するブリッジ。
denpa 同梱のエージェントの代わりに置き、他のアプリと同じ Mirakurun を共有する。

```text
他のアプリ ─────────────┐
                        ├→ Mirakurun → チューナー
denpa → このブリッジ ───┘
```

## 使い方

denpa の `compose.override.yml` で `tuner-agent` を差し替える。denpa 側の設定は変えない。

```yaml
services:
  tuner-agent:
    image: ghcr.io/multiverse2011/denpa-mirakurun-bridge:latest
    cap_add: !reset []
    device_cgroup_rules: !reset []
    stop_grace_period: 10s
    volumes: !override
      - bridge-config:/config
    environment:
      MIRAKURUN_URL: http://mirakurun.lan:40772
      PRIORITY_MAP: "10:2,9:1,8:0,5:0,4:0,3:0,1:0"
volumes:
  bridge-config:
```

- 認証は無い。復号済みの TS を返すので、ポートは denpa からしか届かないようにする
- denpa と同じマシンに置く (TS が LAN を渡るのが 1 回で済む)

手元で動かす場合:

```sh
go build && MIRAKURUN_URL=http://mirakurun.lan:40772 ./denpa-mirakurun-bridge
go test -race ./...
```

## 設定

| 環境変数 | 既定 | |
| --- | --- | --- |
| `MIRAKURUN_URL` | `http://127.0.0.1:40772` | |
| `HOST` / `PORT` | `127.0.0.1` / `25252` | イメージでは `HOST=0.0.0.0` |
| `PRIORITY_MAP` | なし | denpa の優先度 → Mirakurun の優先度 (`10:2,9:1`)。書かない値はそのまま、0 未満は 0 |
| `KEYLESS_TUNERS` | `1` | 番組表・ロゴ・スキャンが新しく取れるチューナー数 |
| `CHANNELS_FILE` | `data/channels.json` | denpa が預ける局一覧。イメージでは `/config/channels.json` |
| `ALLOWED_HOSTS` | なし | IP・`localhost`・ドット無し・`.local` などの家の中の名前以外で呼ぶとき |
| `DENPA_VERSION` | `bridge-<版>` | `/denpa/about` の版。denpa と揃えると版違いの通知が消える |

## 動き

- **優先度**: denpa の値を `PRIORITY_MAP` で写して `X-Mirakurun-Priority` に渡すだけ。共有・取り上げは Mirakurun が決める。503 は 409 (空き待ち) にする
- **終わり方**: Mirakurun 側から終わったストリームは接続ごと壊して返す (denpa が正常終了と読まないように)。`after` で引き継ぐ利用者だけ正常に閉じる
- **チャンネル**: 地上波は物理チャンネル番号 (`T27` ↔ `27`)。衛星は Mirakurun が TSID を出さないので、denpa が預けた局のサービス ID で引く (`BS15:16625` ↔ `BS15_0` など)
- **`KEYLESS_TUNERS`**: `epg`・`logo`・`scan` の要求は、優先度 0 以上の誰かが開いているチャンネルへの相乗りなら通し、新しく取るのは上限まで。BS と CS は合わせて数える。復号もさせない (`decode=0`)
- **`rank`**: Mirakurun の `tuners.yml` の順
- **カード・復号**: 復号は Mirakurun の decoder。`/denpa/card` はリーダー無しで正常、`/denpa/decode` は 501
- `PUT /denpa/tuners` は 405 (チューナーは Mirakurun で設定する)

## 限界

- 他のアプリの予約は denpa に見えない。重なれば優先度の高いほうが残る
- Mirakurun は空きが無いと約 12.5 秒選び直してから 503 を返すので、その間 denpa は待たされる
- 地上波と衛星の兼用チューナーがあると、denpa の予約の割り振りと Mirakurun の割り当てがずれることがある
- Mirakurun の `channels.yml` は、地上波が物理チャンネル番号 (`27`)、衛星が中継器の番号で始まる名前 (`BS15_0`・`CS2`) である前提
- 録画後の復号はしない
