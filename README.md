# denpa-mirakurun-bridge

[denpa](https://github.com/danything/denpa) のチューナーエージェント API (`/denpa/*`) を [Mirakurun](https://github.com/Chinachu/Mirakurun) に中継するブリッジ。
denpa 付属のエージェントを置き換え、他のアプリと Mirakurun を共有する。

```text
他のアプリ ─────────────┐
                        ├→ Mirakurun → チューナー
denpa → このブリッジ ───┘
```

## 導入

- 認証なしで復号済みの TS を返すため、アクセスは denpa に限定する
- TS の LAN 転送を 1 回に抑えるため、denpa と同じマシンで動かす

### 既存の Mirakurun を使う場合

denpa の `compose.override.yml` で `tuner-agent` を差し替える。denpa 側の設定変更は不要。

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

以下を `compose.yml` に保存する。

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
    volumes:
      - bridge-config:/config

  denpa:
    image: ghcr.io/danything/denpa:latest
    restart: unless-stopped
    stop_grace_period: 6h5m
    depends_on: [tuner-agent]
    environment:
      TZ: Asia/Tokyo
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
| `ALLOWED_HOSTS` | なし | IP アドレス・`localhost`・ドットなしの名前・`.local` など以外でアクセスする際の許可ホスト |
| `DENPA_VERSION` | `bridge-<バージョン>` | `/denpa/about` が返すバージョン。denpa と揃えると不一致の通知を抑制 |
| `MESSAGE_LANG` | `ja` | エラー応答とログの言語 (`ja` / `en`) |

## 動作と制約

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
