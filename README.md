# denpa-mirakurun-bridge

[denpa](https://github.com/danything/denpa) のチューナーエージェント API (`/denpa/*`) を [Mirakurun](https://github.com/Chinachu/Mirakurun) に中継するブリッジ。
denpa 付属のエージェントを置き換え、他のアプリと Mirakurun を共有する。

```text
他のアプリ ─────────────┐
                        ├→ Mirakurun → チューナー
denpa → このブリッジ ───┘
```

## 導入

- denpa (v2.28.0 以降) とは TLS で繋ぎ、組んだ denpa (クライアント証明書) にしか答えない (denpa の [docs/agent.md](https://github.com/danything/denpa/blob/main/docs/agent.md)「denpa とは TLS で組む」と同じ)。組むには合言葉 (`AGENT_PAIR_TOKEN`) を両方に渡す
- v2.27.0 以前の denpa は HTTP でしか繋がらないため、`AGENT_TLS: "0"` を足す。この場合は認証なしで復号済みの TS を返すので、アクセスを denpa に限定する
- TS の LAN 転送を 1 回に抑えるため、denpa と同じマシンで動かす

### 既存の Mirakurun を使う場合

denpa の `compose.override.yml` で `tuner-agent` を差し替える。denpa 側の設定変更は不要 (denpa の compose が `.env` の `AGENT_PAIR_TOKEN` を `tuner-agent` と denpa の `TUNER_AGENT_URL` の両方に渡している)。

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

### Mirakurun も同時に起動する場合

以下を `compose.yml` に保存し、同じ場所に合言葉を書いた `.env` を作る。

```sh
echo "AGENT_PAIR_TOKEN=$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" > .env
```

```yaml
name: denpa

services:
  mirakurun:
    image: chinachu/mirakurun:latest
    restart: always
    cap_add: [SYS_ADMIN, SYS_NICE]
    device_cgroup_rules: ["c *:* rmw"]
    environment:
      TZ: Asia/Tokyo
    volumes:
      - ./mirakurun/config:/app-config
      - ./mirakurun/data:/app-data
      - /dev:/dev
    tmpfs: [/tmp]
    ports:
      - "40772:40772" # 他のアプリ用。不要なら削除

  # denpa の既定の接続先は tuner-agent:25252
  tuner-agent:
    image: ghcr.io/multiverse2011/denpa-mirakurun-bridge:latest
    restart: unless-stopped
    stop_grace_period: 10s
    depends_on: [mirakurun]
    environment:
      MIRAKURUN_URL: http://mirakurun:40772
      PRIORITY_MAP: "10:2,9:1,8:0,5:0,4:0,3:0,1:0"
      AGENT_PAIR_TOKEN: ${AGENT_PAIR_TOKEN:?}
    volumes:
      - bridge-config:/config

  denpa:
    image: ghcr.io/danything/denpa:latest
    restart: unless-stopped
    stop_grace_period: 6h5m
    depends_on: [tuner-agent]
    environment:
      TZ: Asia/Tokyo
      TUNER_AGENT_URL: https://tuner-agent:25252#pair=${AGENT_PAIR_TOKEN:?}
      TRUSTED_NETWORKS: 192.168.0.0/16,10.0.0.0/8
    volumes:
      - denpa-data:/data
      - denpa-recorded:/media/raw
      - denpa-library:/media/encoded
    ports:
      - "3000:3000"

volumes:
  bridge-config:
  denpa-data:
  denpa-recorded:
  denpa-library:
```

Mirakurun のデバイス・カードリーダー・`network_mode: host` などは [Mirakurun の compose](https://github.com/Chinachu/Mirakurun/blob/master/docker/docker-compose.yml)、denpa の設定は [denpa の compose.prod.yml](https://github.com/danything/denpa/blob/main/compose.prod.yml) を参照。

## 環境変数

| 環境変数 | 既定値 | 説明 |
| --- | --- | --- |
| `MIRAKURUN_URL` | `http://127.0.0.1:40772` | Mirakurun の接続先 |
| `HOST` / `PORT` | `127.0.0.1` / `25252` | Docker イメージでは `HOST=0.0.0.0` |
| `PRIORITY_MAP` | なし | denpa から Mirakurun への優先度変換 (`10:2,9:1`)。未指定の値はそのまま、負の値は 0 |
| `KEYLESS_TUNERS` | `1` | 番組表・ロゴ取得・スキャンで新規に使用できるチューナー数 |
| `CHANNELS_FILE` | `data/channels.json` | denpa から受け取るチャンネル一覧。Docker イメージでは `/config/channels.json` |
| `AGENT_TLS` | `1` | `0` で TLS を使わず HTTP で待ち受ける (v2.27.0 以前の denpa 向け) |
| `AGENT_PAIR_TOKEN` | なし | denpa と組むときの合言葉。未指定なら `CHANNELS_FILE` の隣の `tls/pair-token` に作る |
| `ALLOWED_HOSTS` | なし | `AGENT_TLS=0` のとき、IP アドレス・`localhost`・ドットなしの名前・`.local` など以外でアクセスする際の許可ホスト |
| `DENPA_VERSION` | `bridge-<バージョン>` | `/denpa/about` が返すバージョン。denpa と揃えると不一致の通知を抑制 |
| `MESSAGE_LANG` | `ja` | エラー応答とログの言語 (`ja` / `en`) |

## 動作と制約

- **TLS**: 鍵・自己署名の証明書・合言葉・組んだ denpa の指紋を `CHANNELS_FILE` の隣の `tls/` (Docker イメージでは `/config/tls`) に置く。証明書は作り直さない。まだ誰とも組んでいなければ、起動時に標準出力へ組むための URL (`https://<ホスト名>:25252#pair=<合言葉>`) を出す。合言葉を変えるには `pair-token` を消して再起動し、組んだ denpa を外すには `clients` からその行を消して再起動する。TLS では Host 名と `Content-Type` を見ない (ブラウザはクライアント証明書を出せず繋げないため)

- **優先度**: denpa の値を `PRIORITY_MAP` で変換し、`X-Mirakurun-Priority` に渡す。チューナーの共有・割り込みは Mirakurun が制御する。503 は 409 (空き待ち) に変換する
- **ストリーム終了**: Mirakurun 側で終了した場合は、denpa が正常終了と判定しないよう接続を切断する。`after` による引き継ぎ時のみ正常終了する
- **チャンネル**: 地上波は物理チャンネル番号 (`T27` ↔ `27`)。衛星は Mirakurun が TSID を返さないため、denpa から受け取ったチャンネルのサービス ID で照合する (`BS15:16625` ↔ `BS15_0` など)
- **`KEYLESS_TUNERS`**: `epg`・`logo`・`scan` の要求は、優先度 0 以上で使用中のチャンネルを共有できる。新規のチューナー使用は上限までとし、BS と CS は合算する。復号は無効 (`decode=0`)
- **途切れの検出**: Mirakurun から 30 秒データが来ない、または denpa が 30 秒読まない配信は切断し、チューナーを解放する。`/denpa/events` は 30 秒ごとに空のコメント行を送り、無通信による切断を防ぐ
- **`rank`**: Mirakurun の `tuners.yml` の定義順
- **カード・復号**: 復号は Mirakurun の decoder が担当する。`/denpa/card` はカードリーダーなしとして正常応答し、`/denpa/decode` は 501 を返す
- `PUT /denpa/tuners` は 405 (チューナーは Mirakurun で設定する)

- denpa は他のアプリの予約を把握できない。競合時は優先度の高い要求が優先される
- Mirakurun は空きがないと約 12.5 秒再試行してから 503 を返すため、その間 denpa は待機する
- 地上波と衛星の兼用チューナーがあると、denpa の予約の割り振りと Mirakurun の割り当てがずれることがある
- Mirakurun の `channels.yml` は、地上波が物理チャンネル番号 (`27`)、衛星が中継器の番号で始まる名前 (`BS15_0`・`CS2`) である必要がある
- 録画後の復号には非対応

## 開発

ビルド・実行:

```sh
go build
MIRAKURUN_URL=http://mirakurun.lan:40772 ./denpa-mirakurun-bridge
```

テスト:

```sh
go test -race ./...
```

## ライセンス

[Apache License 2.0](LICENSE)

ただし [tls.go](tls.go) だけは denpa のチューナーエージェント (`agent/Denpa.Agent/AgentTls.cs`) から移したため、元と同じ [MPL-2.0](https://mozilla.org/MPL/2.0/) です。
このファイルを直して配るなら、そのファイルは MPL-2.0 で公開してください。
