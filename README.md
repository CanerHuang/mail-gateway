# mail-gateway

`mail-gateway` 讓其他服務透過 gRPC 寄信：SMTP 連線與寄件帳號統一由 gateway 管理，各服務不必再各自設定 mail。連線採 mTLS，以 client 憑證的 CN 識別呼叫端。服務介面定義於 [`proto/v1/mail.proto`](proto/v1/mail.proto)，編譯後的執行檔為 `maild`。

- **同步寄送 `SendMail`**：等 SMTP server 收下才回應，當下就知道成功或失敗；只寄一次，不重試。
- **非同步寄送 `QueueMail`**：存入佇列就回應，寄送與失敗重試由 gateway 在背景處理；可用 `send_at` 排程。回應成功只代表 gateway 已收下，寄送結果記在 gateway log。
- 兩者的「成功」都只代表 SMTP server 收下，不代表進了收件匣，之後仍可能退信。
- gateway 不提供查詢郵件，也不做防重複寄送。

## 文件

- [doc/errors.md](doc/errors.md)：gRPC 錯誤碼對照、各 RPC 可能回傳的錯誤與呼叫端建議的處理方式
- [doc/certificates.md](doc/certificates.md)：建 CA、簽 server／client 憑證、換發、撤銷與換 CA 的流程

## 專案結構

```text
mail-gateway/
├── main.go                     # 啟動流程：載入設定、啟動背景寄送與 gRPC server、處理 SIGHUP／SIGTERM
├── conf/maild.toml             # 啟動期設定（read-only），各欄位說明見檔案內註解
├── proto/v1/                   # gRPC 介面定義 (mail.proto)
├── internal/
│   ├── api/mailServer/         # gRPC server、mTLS 與憑證 reload、呼叫端名單檢查、protobuf <-> service 轉換
│   ├── config/                 # maild.toml 嚴格載入與驗證
│   ├── message/                # 地址與 header 檢查、MIME 郵件組成（串流寫出）
│   ├── service/                # 核心邏輯：驗證、同步寄送、佇列、寄送量上限、寄件帳號暫停
│   ├── smtpclient/             # 串流寄出一封郵件並分類 SMTP 錯誤
│   ├── smtptest/               # 測試用的 SMTP server
│   └── spool/                  # 非同步佇列的檔案格式與讀寫
├── pkg/
│   ├── grpc/mail/              # protoc 生成碼
│   └── version/                # 版本資訊
├── scripts/cert.sh             # 簽發 mTLS 憑證
└── systemd/maild.service       # systemd 單位檔
```

## 建置與執行

建置（同時重新產生 protobuf 生成碼並打包）：

```bash
./build.sh            # 產出 dist/maild/
./build.sh --tar      # 另外打包成 dist/maild.tar.gz
```

執行：

```bash
./maild                                   # 讀取 <exe-dir>/conf/maild.toml
./maild -config /etc/maild/maild.toml
./maild -version
```

設定檔必須存在，且除註明可省略者外所有欄位都要通過驗證；任何缺漏或不合理的值都會 fatal exit。

測試：

```bash
go test -race ./...
```

## 佈署 Layout

`build.sh` 產出的 `maild.tar.gz` 解開後即符合 `maild` 的預設路徑解析，建議佈署到 `/opt/maild/`：

```text
/opt/maild/
├── maild                       # build.sh 產出的 binary
├── conf/maild.toml             # 啟動期設定（由 maild.toml.example 改名，含 SMTP 密碼，權限 600）
├── certs/                      # server.crt、server.key、ca.crt（不放 ca.key）
├── scripts/cert.sh             # 簽發憑證（可在別台機器使用）
└── data/spool/                 # 非同步佇列：tmp/（寫入中）、queue/（等待寄送）
```

設定檔裡的相對路徑（`certs/...`、`data/spool`）以啟動時的工作目錄為準，systemd 以 `WorkingDirectory=/opt/maild` 設定；手動執行時先 `cd /opt/maild`。

安裝步驟：

```bash
# 1. 建立執行使用者（不允許登入）
useradd --system --home /opt/maild --shell /usr/sbin/nologin maild

# 2. 解開 tarball、改名 .example、放入憑證（簽發方式見 doc/certificates.md）
tar -xzf maild.tar.gz -C /opt
mv /opt/maild/conf/maild.toml.example /opt/maild/conf/maild.toml
mkdir -p /opt/maild/data
chown -R maild:maild /opt/maild
chmod 600 /opt/maild/conf/maild.toml /opt/maild/certs/server.key

# 3. 安裝並啟動 systemd unit
cp systemd/maild.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now maild
```

## 設定檔

`conf/maild.toml` 是唯一的設定檔，程式只讀不寫。完整欄位與說明見檔案內的註解，重點如下：

| 區塊 | 內容 |
|---|---|
| `[server]`、`[server.keepalive]` | 監聽位址、逾時、request 大小上限、連線存活時間 |
| `[tls]` | server 憑證、client CA、憑證到期提醒 |
| `[smtp]` | 所有寄件帳號共用的寄送時限、同步寄送連線數、寄件帳號暫停時間 |
| `[queue]` | 佇列資料夾、掃描間隔、背景寄送連線數、重試次數與間隔、佇列容量、`send_at` 上限 |
| `[mail]` | 收件者、附件、labels 上限 |
| `[log]` | log 文字欄位的最大顯示字數 |
| `[senders.<別名>]` | 寄件帳號；各自的 SMTP server 與帳密在 `[senders.<別名>.smtp]` |
| `[clients.<CN>]` | 呼叫端名單：可用的寄件者、預設寄件者、寄送量上限 |

### 重新載入

`systemctl reload maild`（SIGHUP）只重新載入下列內容，其他欄位修改後需重啟：

- `[tls]` 指向的憑證、私鑰與 CA 檔案內容
- `[senders]` 與 `[clients]`

重新載入或驗證失敗時沿用舊設定並記 error log，不會因此停機。呼叫端名單在每次 RPC 都會檢查，從 `[clients]` 刪掉的呼叫端在 reload 後立即被拒絕。

## 寄送行為

- **寄件帳號暫停**：Gmail 每日額度用完（`550 5.4.5`）或登入失敗（`535`）時，暫停整個寄件帳號 `smtp.sender_pause`（預設 1 小時）。暫停期間同步寄送直接回錯誤，佇列中的郵件延後到暫停結束且不扣重試次數；到期後先試寄一封，成功才恢復。修正密碼並 reload 會立即解除登入失敗的暫停。
- **佇列重試**：暫時失敗（4xx、連不上）依 `retry_initial_interval` 每次加倍重試，最多 `max_attempts` 次；永久失敗（5xx）或次數用完即刪除佇列檔並記 error log。
- **重啟**：佇列檔留在 `data/spool/queue/`，重啟後照常寄送；寄送途中停掉的郵件重啟後會再寄一次，可能寄出兩封。

## Log

log 輸出到 stdout，由 journald 保存；寄送結果只記在 log，追查時以 `mail_id` 或 labels 搜尋：

```bash
journalctl -u maild | grep 01JBXK3Q9V8M2T4N6P0R5S7W9Y
journalctl -u maild | grep 'order_id":"A1234'
```

| 訊息 | 等級 | 時機 |
|---|---|---|
| `mail queued` | info | `QueueMail` 收進佇列 |
| `mail sent` | info | 寄出成功（同步、非同步） |
| `mail retry scheduled` | warn | 寄送失敗、排定重試 |
| `mail failed` | error | 判定失敗（永久錯誤、次數用完、設定已變更） |
| `mail rejected` | warn | 請求被拒（驗證失敗、超過寄送量、佇列已滿） |
| `sender paused`／`sender resumed` | warn（登入失敗為 error） | 寄件帳號暫停與恢復 |
| `certificate expires soon` | warn | 憑證剩餘有效期少於 `tls.expiry_warning` |

## 以 grpcurl 呼叫 API

server 未啟用 reflection，每次呼叫都要帶 `-proto`；連線需要 client 憑證。下面範例假設工作目錄為專案根、server 監聽 `mailgw.example.com:30053`，`certs/` 裡有 `ca.crt` 與呼叫端的 `order-service.crt`／`.key`。

```bash
G="grpcurl -cacert certs/ca.crt -cert certs/order-service.crt -key certs/order-service.key \
  -import-path proto/v1 -proto mail.proto"

# 版本資訊（只需通過 mTLS）
$G mailgw.example.com:30053 mail.api.v1.MailAPI/VersionGet

# 可用的寄件者
$G mailgw.example.com:30053 mail.api.v1.MailAPI/ListSenders

# 同步寄送
$G -d '{"mail":{"to":[{"address":"a@example.com","name":"王小明"}],"subject":"訂單已出貨","text_body":"您好","labels":{"order_id":"A1234"}}}' \
  mailgw.example.com:30053 mail.api.v1.MailAPI/SendMail

# 非同步寄送，指定寄件者並排程
$G -d '{"mail":{"sender":"billing","to":[{"address":"a@example.com"}],"subject":"帳單","html_body":"<p>本月帳單</p>"},"send_at":"2026-11-01T01:00:00Z"}' \
  mailgw.example.com:30053 mail.api.v1.MailAPI/QueueMail
```

附件的 `content` 是 bytes，JSON 裡以 base64 表示，例如 `"attachments":[{"filename":"報價單.pdf","content":"JVBERi0xLjQK..."}]`。

## Protobuf 編譯

`build.sh` 每次都會重新產生，也可以手動執行（於專案根目錄，生成碼輸出至 `pkg/grpc/mail/`）：

```bash
protoc -I proto/v1 \
  --go_out=pkg/grpc/mail --go_opt=paths=source_relative \
  --go-grpc_out=pkg/grpc/mail --go-grpc_opt=paths=source_relative \
  mail.proto
```
