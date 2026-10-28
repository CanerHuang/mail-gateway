# mTLS 憑證

maild 只接受 mTLS 連線：呼叫端必須出示由 gateway 信任的 CA 簽發的 client 憑證，gateway 以憑證的 CN 識別呼叫端，再依 `maild.toml` 的 `[clients]` 決定能不能呼叫、能用哪些寄件者。本文件說明如何用 `scripts/cert.sh` 建立 CA、簽發與換發憑證。

## 基本觀念

- **憑證是 CA 簽的，不是 gateway 發的。** gateway 與呼叫端都只是「信任這個 CA」：
  - 呼叫端用 `ca.crt` 確認 gateway 的 server 憑證是這個 CA 簽的、而且名稱符合要連的位址。
  - gateway 用 `ca.crt` 確認 client 憑證是這個 CA 簽的，再從 CN 得知是哪個服務。
- **CN 就是呼叫端名稱。** CN 寫在 client 憑證裡、受 CA 的簽名保護，改了任何一個字簽名就失效，所以無法造假。可信與否取決於兩件事：`ca.key` 不外洩，以及簽發前確認 CSR 的 CN。
- **gateway 不存放各呼叫端的憑證。** 新增呼叫端時，gateway 只需要在 `[clients]` 加上它的 CN。
- 金鑰一律為 ECDSA P-256；gateway 只接受 TLS 1.3。

## 檔案與存放位置

| 檔案 | 放在哪裡 | 說明 |
|---|---|---|
| `ca.key` | 只放在簽憑證的機器，**不放 gateway** | 最機密：有它就能簽出任何 client 憑證 |
| `ca.crt` | gateway 與所有呼叫端 | 公開，用來驗證對方的憑證 |
| `server.key`、`server.crt` | gateway 的 `certs/` | 私鑰權限 600 |
| `<CN>.key`、`<CN>.crt` | 各呼叫端，一個服務一組 | 私鑰不離開呼叫端的機器 |

憑證用途分開限制：CA 憑證只能簽發憑證，server 憑證只能當 server（`serverAuth`），client 憑證只能當 client（`clientAuth`）。client 憑證被偷也無法用來假冒 gateway。

效期：CA 10 年（`CA_DAYS`），server 與 client 憑證 1 年（`CERT_DAYS`）。

## 1. 建立 CA（管理者，只做一次）

```bash
mkdir -p ~/maild-ca && cd ~/maild-ca
/opt/maild/scripts/cert.sh ca            # 產生 ca.key、ca.crt；CN 預設為 maild-ca
```

這個 CA 只給 mail gateway 用，不要跟其他用途共用。`ca.key` 請妥善備份並限制存取。

## 2. 簽 gateway 的 server 憑證（管理者）

參數是呼叫端連線用的 DNS 名稱或 IP，全部寫進 SAN，第一個同時當 CN：

```bash
cd ~/maild-ca
/opt/maild/scripts/cert.sh server mailgw.example.com 10.0.0.5
```

把 `server.crt`、`server.key`、`ca.crt` 放到 gateway 的 `/opt/maild/certs/`：

```bash
install -o maild -g maild -m 600 server.key /opt/maild/certs/server.key
install -o maild -g maild -m 644 server.crt ca.crt /opt/maild/certs/
```

> Go 驗證主機名稱只看 SAN、不看 CN。呼叫端連線用的名稱或 IP 沒寫進 SAN，握手就會失敗（`x509: certificate is valid for ..., not ...`）。

## 3. 申請 client 憑證（呼叫端）

在呼叫端自己的機器上產生私鑰與 CSR（憑證申請檔），CN 填服務名稱。CN 建議只用英數字與連字號（例如 `order-service`），含 `.` 等字元時 `[clients]` 的 key 要加引號。

```bash
./cert.sh csr order-service              # 產生 order-service.key、order-service.csr
```

只把 `order-service.csr` 交給管理者；`order-service.key` 留在原機器。

## 4. 簽 client 憑證（管理者）

```bash
cd ~/maild-ca
/opt/maild/scripts/cert.sh sign order-service.csr
```

腳本會先顯示 CSR 的 CN 並要求確認。**簽發前務必確認 CN 屬於提出申請的服務**：簽了別人的名字，就等於讓對方冒用那個服務。一個服務一個 CN，不可重複。

把 `order-service.crt` 與 `ca.crt` 交回呼叫端。

## 5. 加入呼叫端名單（管理者）

在 `/opt/maild/conf/maild.toml` 加上：

```toml
[clients.order-service]
senders = ['service']
default_sender = 'service'
rate_limit = 200
rate_limit_window = '1h'
```

然後重新載入（不需要重啟）：

```bash
systemctl reload maild
```

## 6. 呼叫端的 Go 設定

`GetClientCertificate` 在每次 TLS 握手時重新讀取憑證檔，換發後不必重啟呼叫端的服務：

```go
caPEM, err := os.ReadFile("certs/ca.crt")
if err != nil {
	return err
}
pool := x509.NewCertPool()
pool.AppendCertsFromPEM(caPEM)

creds := credentials.NewTLS(&tls.Config{
	MinVersion: tls.VersionTLS13,
	RootCAs:    pool,
	ServerName: "mailgw.example.com", // 必須在 server 憑證的 SAN 裡
	GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair("certs/order-service.crt", "certs/order-service.key")
		return &cert, err
	},
})
conn, err := grpc.NewClient("mailgw.example.com:30053", grpc.WithTransportCredentials(creds))
```

gateway 設有 `max_connection_age`（預設 1 小時），連線會定期重建，換發的憑證最晚在這段時間後開始使用。

## 7. 換發（到期前）

gateway 在憑證剩餘有效期少於 `tls.expiry_warning`（預設 30 天）時會記 warning log（`certificate expires soon`，同一張憑證每天最多一次），可以用來安排換發。查看到期日：

```bash
./cert.sh show order-service.crt
```

- **client 憑證**：呼叫端把舊的 `order-service.key`／`.csr` 移開後重新執行步驟 3，管理者重新執行步驟 4，**沿用同一個 CN**，`[clients]` 不用改。呼叫端換檔即可，不需重啟。
- **server 憑證**：管理者把舊的 `server.key`／`server.crt` 移開後重新執行步驟 2，換掉 gateway 上的檔案，再執行 `systemctl reload maild`。

## 8. 撤銷呼叫端

Go 不會自動檢查 CRL／OCSP。撤銷的做法是把呼叫端從 `[clients]` 刪掉，再執行 `systemctl reload maild`。名單在每次 RPC 都會檢查，reload 後立即生效，已建立的連線也一樣。

## 9. 換 CA

`client_ca_file` 可以串接多張 CA，換 CA 期間讓新舊並存：

1. 建立新 CA（換一個資料夾，或用 `CA_KEY`／`CA_CRT` 指定新的檔名）。
2. 把新舊 CA 串接成同一個檔案並 reload：
   ```bash
   cat new-ca.crt old-ca.crt > /opt/maild/certs/ca.crt
   systemctl reload maild
   ```
3. 用新 CA 重簽 server 憑證與所有 client 憑證，並把新的 `ca.crt` 發給所有呼叫端。
4. 全部換完後，`/opt/maild/certs/ca.crt` 只留新 CA，再 reload 一次。

## 常見錯誤

| 現象 | 原因 |
|---|---|
| 呼叫端看到 `x509: certificate is valid for ..., not ...` | 連線用的名稱不在 server 憑證的 SAN |
| 呼叫端看到 `certificate signed by unknown authority` | 呼叫端的 `ca.crt` 不是簽 server 憑證的那個 CA |
| gateway log `TLS handshake failed`：`client didn't provide a certificate` | 呼叫端沒有設定 client 憑證（需開啟 `server.connection_log` 才會記錄） |
| gateway log `TLS handshake failed`：`x509: certificate has expired or is not yet valid` | client 憑證過期 |
| gateway log `TLS handshake failed`：`x509: certificate signed by unknown authority` | client 憑證不是 gateway 信任的 CA 簽的 |
| RPC 回 `PermissionDenied: client not allowed` | 憑證有效，但 CN 不在 `[clients]` |
