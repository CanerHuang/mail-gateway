# 錯誤回應

service 層只回傳 sentinel error（`internal/service/errors.go`），由 `internal/api/mailServer/handler.go` 的 `toStatusError` 統一轉成 gRPC status code 回給呼叫端。本文件說明對照關係、各 RPC 可能出現的錯誤，以及呼叫端該怎麼處理。

## 設計原則

- **集中轉換**：handler 一律用 `errors.Is` 比對 sentinel error 後轉成 gRPC code，不在各 handler 內各自決定。
- **未對應 = Internal**：沒被 `toStatusError` 比對到的 error 都會落到 `codes.Internal`，以 `error` level 記一筆 log，對外只回 `internal server error`（不洩漏內部細節）。
- **訊息寫明原因**：呼叫端都是自己的服務，錯誤訊息會說明是哪一項不通過（例如 `invalid argument: to[2] "x": invalid address`）；SMTP 錯誤附上 SMTP server 的原始回應。
- **固定的訊息開頭**：同一個 code 有多種原因時（例如 `ResourceExhausted`），訊息以固定字串開頭，呼叫端可以用 `strings.HasPrefix` 分辨。

## 連線層（請求到不了 RPC）

| 情況 | 呼叫端看到的結果 |
|---|---|
| 沒有出示 client 憑證、憑證過期、不是 gateway 信任的 CA 簽的 | TLS 握手失敗，gRPC 回 `Unavailable`（連線建立失敗） |
| 只支援 TLS 1.2 以下 | TLS 握手失敗，同上 |
| request 超過 `server.max_request_size_mib` | `ResourceExhausted`，由 gRPC 自動產生（`received message larger than max`） |

## gRPC code 對照表

| gRPC code | sentinel error | 訊息開頭 | 意義 |
|---|---|---|---|
| `PermissionDenied` | `ErrClientNotAllowed` | `client not allowed` | 憑證有效，但 CN 不在 `[clients]` |
| `PermissionDenied` | `ErrSenderNotAllowed` | `sender not allowed` | `Mail.sender` 不在這個呼叫端可用的寄件者清單 |
| `InvalidArgument` | `ErrInvalidArgument` | `invalid argument` | 郵件內容驗證失敗，訊息會說明哪一項 |
| `ResourceExhausted` | `ErrRateLimited` | `rate limit exceeded` | 超過這個呼叫端的寄送量上限（`rate_limit`／`rate_limit_window`） |
| `ResourceExhausted` | `ErrQueueFull` | `queue full` | 佇列已滿（`queue.max_queued_mails`），只有 `QueueMail` |
| `ResourceExhausted` | `ErrSenderQuota` | `sender daily quota exceeded` | 寄件帳號達到每日上限（Gmail `550 5.4.5`），只有 `SendMail` |
| `Unavailable` | `ErrSenderAuth` | `sender authentication failed` | 寄件帳號登入失敗（`535`），只有 `SendMail` |
| `Unavailable` | `ErrSMTPTemporary` | `smtp temporary failure` | SMTP 暫時失敗（4xx、連不上、逾時），只有 `SendMail` |
| `FailedPrecondition` | `ErrSMTPPermanent` | `smtp permanent failure` | SMTP 永久失敗（5xx、部分收件者被拒），只有 `SendMail` |
| `DeadlineExceeded` | `context.DeadlineExceeded` | — | 呼叫端 deadline 到期，`SendMail` 已中止 |
| `Canceled` | `context.Canceled` | — | 呼叫端取消 |
| `Internal` | （fallback） | `internal server error` | 未預期錯誤（例如寫入佇列檔失敗），細節只記於 server log |

## `InvalidArgument` 的檢查項目

| 項目 | 規則 |
|---|---|
| 收件者 | to／cc／bcc 合計至少一位，最多 `mail.max_recipients` 位 |
| 地址（含 `reply_to`） | 只接受 ASCII、不含顯示名稱、能由 `net/mail` 解析；`reply_to` 不計入收件者上限 |
| 顯示名稱、主旨、`sender_name` | 不可含換行等控制字元（防止 header 注入） |
| 內文 | `text_body` 與 `html_body` 至少擇一 |
| 附件 | 合計不超過 `mail.max_attachments_size_mib`；`filename` 必填且不含控制字元；`content_type` 留空時依副檔名推斷，填寫時必須是合法的 media type 且不可是 `multipart/*`；`content_id` 只能用英數字與 `. _ @ -`，不可重複 |
| labels | 最多 `mail.max_labels` 個；key 只能用英數字與 `_ . -`，最多 `max_label_key_length` 字；value 最多 `max_label_value_length` 字（中文一個字算一個），不可含控制字元 |
| `send_at`（`QueueMail`） | 不可晚於現在加上 `queue.max_schedule_ahead`；時間格式不合法也回 `InvalidArgument` |

## 各 RPC 可能回傳的錯誤

除 `VersionGet` 外，所有 RPC 都可能因 CN 不在 `[clients]` 回 `PermissionDenied`，下表不再重複列出。

| RPC | 可能的錯誤 |
|---|---|
| `SendMail` | `PermissionDenied`（寄件者不允許）、`InvalidArgument`、`ResourceExhausted`（寄送量上限、寄件帳號每日上限）、`Unavailable`（SMTP 暫時失敗、寄件帳號登入失敗）、`FailedPrecondition`（SMTP 永久失敗）、`DeadlineExceeded`、`Internal` |
| `QueueMail` | `PermissionDenied`（寄件者不允許）、`InvalidArgument`、`ResourceExhausted`（寄送量上限、佇列已滿）、`Internal` |
| `ListSenders` | 無（只有共通的 `PermissionDenied`） |
| `VersionGet` | 無（只需通過 mTLS 握手，恆成功） |

`QueueMail` 不會回 SMTP 相關的錯誤：回應成功只代表 gateway 已收下，寄送結果記在 gateway log。

## 呼叫端建議的處理方式

| 錯誤 | 建議 |
|---|---|
| `InvalidArgument`、`PermissionDenied` | 修正內容或設定，原封不動重送不會成功 |
| `ResourceExhausted` + `rate limit exceeded` | 稍後再送，或請管理者調高 `rate_limit` |
| `ResourceExhausted` + `queue full` | 稍後再送；佇列滿通常代表 SMTP 長時間寄不出去，應通知管理者 |
| `ResourceExhausted` + `sender daily quota exceeded` | 改用其他寄件者，或改用 `QueueMail` 排隊（gateway 會等額度恢復後再寄） |
| `Unavailable` | 可以稍後重送，或改用 `QueueMail` 交給 gateway 重試 |
| `FailedPrecondition` | SMTP server 拒收（例如收件者不存在），修正內容前不要原封不動重送 |
| `DeadlineExceeded` | 不確定是否已寄出：若中斷時郵件內容已送完，gateway log 會標記 `result_unknown=true`；重送可能寄出兩封 |

gateway 不做防重複寄送：呼叫端沒收到回應就重送時，可能寄出（或排入）兩封相同的郵件。
