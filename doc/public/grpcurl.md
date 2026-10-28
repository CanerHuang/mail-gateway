# Mail Gateway API：grpcurl 使用範例

Mail Gateway 讓你的服務透過 gRPC 寄信，不必自行設定 SMTP。本文件以 [grpcurl](https://github.com/fullstorydev/grpcurl) 示範每個 API 的呼叫方式，方便在串接前先手動測試；程式串接時，請求與回應的欄位與本文件相同。

API 提供兩種寄送方式：

| RPC | 方式 | 回應成功代表 |
|---|---|---|
| `SendMail` | 同步：等郵件伺服器收下才回應 | 郵件伺服器已收下這封信 |
| `QueueMail` | 非同步：排入佇列就回應，由 gateway 在背景寄送與重試 | gateway 已收下這封信，不保證寄出 |

兩種方式的「成功」都不代表信已進入收件匣，之後仍可能退信。gateway 不提供查詢郵件的 API，也不做防重複寄送：沒收到回應就重送時，可能寄出兩封。

## 事前準備

### 1. 安裝 grpcurl

依 [grpcurl 的安裝說明](https://github.com/fullstorydev/grpcurl#installation) 安裝，或使用 Go：

```bash
go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
```

### 2. 向管理者取得

| 項目 | 說明 |
|---|---|
| gateway 位址 | 例如 `mail-gateway.example.com:20050` |
| `mail.proto` | API 定義檔；gateway 未開放 reflection，每次呼叫都要帶上 |
| 憑證包（`<服務名稱>.tar.gz`） | 你的服務專用的 client 憑證，服務名稱寫在憑證裡，作為呼叫端的身分 |
| 可用的寄件者 | 也可以用下面的 `ListSenders` 查詢 |

### 3. 解開憑證包

gateway 只接受 mTLS 連線（TLS 1.3）：每個呼叫端都要出示由管理者簽發的 client 憑證。管理者提供的憑證包內含三個檔案：

| 檔案 | 說明 |
|---|---|
| `client.key` | 私鑰（權限 600）：不要提交到版本控制，也不要交給其他服務使用 |
| `client.crt` | client 憑證 |
| `ca.crt` | 用來驗證 gateway 的 CA 憑證 |

```bash
tar -xzf order-service.tar.gz    # 產生 client.key、client.crt、ca.crt
rm order-service.tar.gz
```

憑證效期為 1 年，到期前請向管理者索取新的憑證包，解開覆蓋舊檔即可。

### 4. 共用參數

以下範例假設目前目錄有 `mail.proto`、`ca.crt`、`client.crt`、`client.key`，並先設定兩個變數：

```bash
GATEWAY="mail-gateway.example.com:20050"
G="grpcurl -cacert ca.crt -cert client.crt -key client.key -import-path . -proto mail.proto"
```

請求的 JSON 欄位可以用 proto 的名稱（`text_body`）或 lowerCamelCase（`textBody`）；回應一律是 lowerCamelCase。

## 確認連線：`VersionGet`

只需要通過 mTLS 就能呼叫，適合用來確認憑證與網路設定是否正確。

```bash
$G "$GATEWAY" mail.api.v1.MailAPI/VersionGet
```

```json
{
  "version": "v1.0.0",
  "commit": "a1b2c3d",
  "date": "261028"
}
```

## 查詢可用的寄件者：`ListSenders`

列出你的服務可以使用的寄件者。寄信時以 `sender` 欄位填寄件者別名；留空就使用 `isDefault` 為 `true` 的那一個。

```bash
$G "$GATEWAY" mail.api.v1.MailAPI/ListSenders
```

```json
{
  "senders": [
    {
      "alias": "billing",
      "address": "billing@example.com",
      "name": "Example Billing"
    },
    {
      "alias": "service",
      "address": "service@example.com",
      "name": "Example Service",
      "isDefault": true
    }
  ]
}
```

## 同步寄送：`SendMail`

等郵件伺服器收下才回應，通常需要數秒，請設定足夠的等待時間（`-max-time`）。失敗時直接回錯誤，gateway 不會自動重試。

```bash
$G -max-time 60 -d '{
  "mail": {
    "to": [{"address": "user@example.com", "name": "王小明"}],
    "subject": "訂單已出貨",
    "text_body": "您好，您的訂單已出貨。",
    "labels": {"order_id": "A1234"}
  }
}' "$GATEWAY" mail.api.v1.MailAPI/SendMail
```

```json
{
  "mailId": "01JBXK3Q9V8M2T4N6P0R5S7W9Y",
  "headerMessageId": "01JBXK3Q9V8M2T4N6P0R5S7W9Y@example.com",
  "smtpResponse": "250 2.0.0 OK queued as 4A1B2C",
  "sentAt": "2026-10-28T03:15:02.120Z"
}
```

| 回應欄位 | 說明 |
|---|---|
| `mailId` | gateway 產生的郵件 ID；向管理者查詢寄送紀錄時請提供這個值 |
| `headerMessageId` | 郵件 `Message-ID` header 的值（不含 `<>`），可用來比對收件端的紀錄 |
| `smtpResponse` | 郵件伺服器收下郵件時的回應 |
| `sentAt` | 寄出時間（UTC） |

指定寄件者、副本、密件副本、回覆地址，並同時提供純文字與 HTML 內文：

```bash
$G -max-time 60 -d '{
  "mail": {
    "sender": "billing",
    "sender_name": "帳務中心",
    "to": [{"address": "user@example.com", "name": "王小明"}],
    "cc": [{"address": "manager@example.com"}],
    "bcc": [{"address": "audit@example.com"}],
    "reply_to": [{"address": "support@example.com", "name": "客服"}],
    "subject": "十月份帳單",
    "text_body": "您好，附上十月份帳單。",
    "html_body": "<p>您好，附上<b>十月份</b>帳單。</p>"
  }
}' "$GATEWAY" mail.api.v1.MailAPI/SendMail
```

`bcc` 的收件者會收到信，但不會出現在郵件 header 裡。`text_body` 與 `html_body` 都有時，收件端會依自己的設定顯示其中一種。

## 非同步寄送：`QueueMail`

排入佇列就回應，不用等待；寄送與失敗重試由 gateway 在背景處理。郵件內容的欄位與 `SendMail` 完全相同。

```bash
$G -d '{
  "mail": {
    "to": [{"address": "user@example.com"}],
    "subject": "密碼已變更",
    "text_body": "您的密碼已於剛才變更。"
  }
}' "$GATEWAY" mail.api.v1.MailAPI/QueueMail
```

```json
{
  "mailId": "01JBXK4R2M7Q8N3P5T6V9W0X1Y",
  "headerMessageId": "01JBXK4R2M7Q8N3P5T6V9W0X1Y@example.com"
}
```

### 排程寄送

以 `send_at` 指定寄送時間（RFC 3339 格式，可帶時區）；留空或早於現在則立即寄送。最晚可以排到多久以後由管理者設定，超過會回 `InvalidArgument`。

```bash
$G -d '{
  "mail": {
    "to": [{"address": "user@example.com"}],
    "subject": "活動提醒",
    "text_body": "活動將於明天上午九點開始。"
  },
  "send_at": "2026-11-01T09:00:00+08:00"
}' "$GATEWAY" mail.api.v1.MailAPI/QueueMail
```

## 附件

附件內容直接放在請求裡，`content` 為 base64 編碼。內容較長時，用 `-d @` 從標準輸入讀取 JSON 比較方便：

```bash
$G -max-time 60 -d @ "$GATEWAY" mail.api.v1.MailAPI/SendMail <<EOF
{
  "mail": {
    "to": [{"address": "user@example.com"}],
    "subject": "報價單",
    "text_body": "您好，附件為報價單。",
    "attachments": [
      {"filename": "報價單.pdf", "content": "$(base64 -w0 report.pdf)"}
    ]
  }
}
EOF
```

`content_type` 留空時依檔名的副檔名推斷，無法推斷時使用 `application/octet-stream`。macOS 的 `base64` 不支援 `-w0`，請改用 `base64 -i report.pdf`。

### 內嵌圖片

設定 `content_id` 的附件會以 inline 方式附加，HTML 內文用 `cid:` 引用：

```bash
$G -max-time 60 -d @ "$GATEWAY" mail.api.v1.MailAPI/SendMail <<EOF
{
  "mail": {
    "to": [{"address": "user@example.com"}],
    "subject": "電子報",
    "html_body": "<p><img src=\"cid:logo\"></p><p>本月消息</p>",
    "attachments": [
      {"filename": "logo.png", "content_type": "image/png", "content": "$(base64 -w0 logo.png)", "content_id": "logo"}
    ]
  }
}
EOF
```

## `mail` 欄位說明

| 欄位 | 必填 | 說明 |
|---|---|---|
| `sender` | | 寄件者別名（見 `ListSenders`）；留空使用預設寄件者 |
| `sender_name` | | 寄件者顯示名稱；留空使用該寄件者預設的名稱 |
| `to`、`cc`、`bcc` | 合計至少一位 | 收件者，每位包含 `address` 與選填的 `name` |
| `reply_to` | | 收件者按「回覆」時的地址，不計入收件者人數 |
| `subject` | | 主旨 |
| `text_body`、`html_body` | 至少擇一 | 純文字與 HTML 內文 |
| `attachments` | | 附件，每個包含 `filename`（必填）、`content`、`content_type`、`content_id` |
| `labels` | | 自訂的對帳資訊，例如 `{"order_id": "A1234"}`；不會寫進郵件，只記在 gateway 的寄送紀錄 |

格式規則：

- 地址只接受英文（ASCII）地址，不含顯示名稱；顯示名稱請放在 `name`。
- 主旨、顯示名稱、附件檔名不可含換行等控制字元。中文可以正常使用。
- `content_id` 只能用英數字與 `. _ @ -`，同一封信內不可重複。
- `labels` 的 key 只能用英數字與 `_ . -`；value 不可含換行。
- 收件者人數、附件合計大小、`labels` 的數量與長度都有上限，由管理者設定；超過時回 `InvalidArgument`，訊息會說明是哪一項。

## 錯誤處理

錯誤時 grpcurl 會顯示 gRPC code 與訊息，例如：

```text
ERROR:
  Code: InvalidArgument
  Message: invalid argument: to[0] "not-an-address": invalid address
```

| Code | 訊息開頭 | 原因 | 建議 |
|---|---|---|---|
| `InvalidArgument` | `invalid argument` | 郵件內容不符合規則，訊息會說明哪一項 | 修正內容後再送 |
| `PermissionDenied` | `client not allowed` | 憑證有效，但服務不在 gateway 的呼叫端名單上 | 聯絡管理者 |
| `PermissionDenied` | `sender not allowed` | 你的服務不能使用這個寄件者 | 改用 `ListSenders` 列出的寄件者 |
| `ResourceExhausted` | `rate limit exceeded` | 超過你的服務的寄送量上限 | 稍後再送 |
| `ResourceExhausted` | `queue full` | gateway 的佇列已滿（只有 `QueueMail`） | 稍後再送，並通知管理者 |
| `ResourceExhausted` | `sender daily quota exceeded` | 寄件帳號已達每日寄送上限（只有 `SendMail`） | 改用其他寄件者，或改用 `QueueMail`（gateway 會等額度恢復後再寄） |
| `ResourceExhausted` | `grpc: received message larger than max` | 請求太大（通常是附件太大） | 縮小附件 |
| `Unavailable` | `smtp temporary failure` | 郵件伺服器暫時無法寄送（只有 `SendMail`） | 稍後重送，或改用 `QueueMail` 交給 gateway 重試 |
| `Unavailable` | `sender authentication failed` | 寄件帳號設定有問題（只有 `SendMail`） | 改用其他寄件者，並通知管理者 |
| `FailedPrecondition` | `smtp permanent failure` | 郵件伺服器拒收，例如收件者不存在（只有 `SendMail`） | 修正內容前不要原封不動重送 |
| `DeadlineExceeded` | — | 等待時間到了，`SendMail` 已中止 | 不確定是否已寄出；重送可能寄出兩封 |
| `Internal` | `internal server error` | gateway 內部錯誤 | 稍後再送，持續發生請通知管理者 |

`QueueMail` 不會回郵件伺服器相關的錯誤：回應成功只代表 gateway 已收下，寄送結果由 gateway 記錄。

## 常見問題

**連線時出現 `context deadline exceeded`、`connection closed` 或 `bad certificate`**

TLS 握手失敗，常見原因：

- 沒有帶 `-cert` 與 `-key`，或兩者不是同一組。
- client 憑證已過期，或不是管理者簽發的。
- grpcurl 版本太舊，不支援 TLS 1.3。

可以先用 `openssl x509 -in client.crt -noout -subject -enddate` 確認憑證的 CN 與到期日。

**出現 `x509: certificate is valid for ..., not ...`**

連線用的主機名稱不在 gateway 憑證的名稱清單裡。請使用管理者提供的主機名稱連線；必須用 IP 或其他名稱連線時，可以加上 `-servername <憑證上的主機名稱>`。

**出現 `x509: certificate signed by unknown authority`**

`-cacert` 指定的不是憑證包裡的 `ca.crt`。

**`QueueMail` 成功了，要怎麼知道有沒有寄出？**

gateway 不提供查詢 API。需要追查時，請提供回應中的 `mailId`（或寄送時填的 `labels`）給管理者查詢寄送紀錄。需要當下知道結果時，請改用 `SendMail`。
