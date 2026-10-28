# mTLS 憑證

maild 只接受 mTLS 連線：呼叫端必須出示由 gateway 信任的 CA 簽發的 client 憑證，gateway 以憑證的 CN 識別呼叫端，再依 `maild.toml` 的 `[clients]` 決定能不能呼叫、能用哪些寄件者。

所有私鑰與憑證都由**簽發機**（放 `ca.key` 與 `scripts/cert.sh` 的機器）產生，打包成 tar.gz 後再配置到 gateway 與各呼叫端。本文件說明如何建立 CA、簽發、換發與撤銷憑證。

## 基本觀念

- **憑證是 CA 簽的，不是 gateway 發的。** gateway 與呼叫端都只是「信任這個 CA」：
  - 呼叫端用 `ca.crt` 確認 gateway 的 server 憑證是這個 CA 簽的、而且名稱符合要連的位址。
  - gateway 用 `ca.crt` 確認 client 憑證是這個 CA 簽的，再從 CN 得知是哪個服務。
- **CN 就是呼叫端名稱。** CN 寫在 client 憑證裡、受 CA 的簽名保護，改了任何一個字簽名就失效，所以無法造假。可信與否取決於 `ca.key` 不外洩，以及簽發時 CN 給對了服務。
- **gateway 不存放各呼叫端的憑證。** 新增呼叫端時，gateway 只需要在 `[clients]` 加上它的 CN。
- 金鑰一律為 ECDSA P-256；gateway 只接受 TLS 1.3。

## 檔案與存放位置

| 檔案 | 放在哪裡 | 內容 |
|---|---|---|
| `ca.key` | 只放簽發機，**不放 gateway** | 最機密：有它就能簽出任何 client 憑證 |
| `ca.crt` | 簽發機；另外包在每個 tar.gz 裡 | 公開，用來驗證對方的憑證 |
| `server.tar.gz` | 簽發機產生，解到 gateway 的 `/opt/maild/certs/` | `server.key`、`server.crt`、`ca.crt` |
| `<CN>.tar.gz` | 簽發機產生，解到該呼叫端的 `certs/` | `client.key`、`client.crt`、`ca.crt` |

tar.gz 內含私鑰（權限 600），請用 scp 等加密管道傳送，配置完成後從簽發機與傳輸途中的位置刪除。換發時會產生新的私鑰，舊的 tar.gz 不需要保留。

憑證用途分開限制：CA 憑證只能簽發憑證，server 憑證只能當 server（`serverAuth`），client 憑證只能當 client（`clientAuth`）。client 憑證被偷也無法用來假冒 gateway。

效期：CA 10 年（`CA_DAYS`），server 與 client 憑證 1 年（`CERT_DAYS`）。

## 1. 準備簽發機、建立 CA（只做一次）

在簽發機上準備一個資料夾，放 `cert.sh`（取自專案的 `scripts/cert.sh`，不隨 gateway 的安裝包發布）並建立 CA：

```bash
mkdir -p ~/maild-ca && cd ~/maild-ca
./cert.sh ca                             # 產生 ca.key、ca.crt；CN 預設為 maild-ca
./cert.sh ca -k /path/to/ca.key          # 或直接指定產生的位置（ca.crt 放在同一個資料夾）
```

這個 CA 只給 mail gateway 用，不要跟其他用途共用。`ca.key` 請妥善備份並限制存取。

之後簽發（`server`、`client`）都用 `-k` 指定 `ca.key`，`ca.crt` 預設取同一個資料夾、同主檔名的檔案（`xxx.key` → `xxx.crt`），不同時用 `-c` 另外指定。指令可以在任何資料夾執行，tar.gz 產生在目前目錄。沒給 `-k` 時使用環境變數 `CA_KEY`／`CA_CRT`，再沒有就找目前目錄的 `ca.key`、`ca.crt`。腳本簽發前會確認 `ca.key` 與 `ca.crt` 是同一組；tar.gz 已存在時不會覆寫。

## 2. 簽發 gateway 憑證

參數是呼叫端連線用的 DNS 名稱或 IP，全部寫進 SAN，第一個同時當 CN：

```bash
~/maild-ca/cert.sh server -k ~/maild-ca/ca.key mailgw.example.com 10.0.0.5    # 產生 server.tar.gz
```

> Go 驗證主機名稱只看 SAN、不看 CN。呼叫端連線用的名稱或 IP 沒寫進 SAN，握手就會失敗（`x509: certificate is valid for ..., not ...`）。

把 `server.tar.gz` 傳到 gateway，以 root 解到 `/opt/maild/certs/`：

```bash
mkdir -p /opt/maild/certs
tar -xzf server.tar.gz -C /opt/maild/certs          # server.key、server.crt、ca.crt
chown maild:maild /opt/maild/certs/{server.key,server.crt,ca.crt}
rm server.tar.gz
systemctl reload maild                              # 首次安裝時改為啟動服務
```

## 3. 簽發呼叫端憑證

CN 填服務名稱，只能用英數字、底線、連字號與點（例如 `order-service`）；含點時 `[clients]` 的 key 要加引號。

```bash
~/maild-ca/cert.sh client -k ~/maild-ca/ca.key order-service    # 產生 order-service.tar.gz
```

**一個服務一個 CN，不可重複。** 拿到 tar.gz 的人就能以這個 CN 的身分呼叫 gateway，只交給該服務的負責人。

把 tar.gz 交給呼叫端，解到服務的 `certs/`：

```bash
mkdir -p certs && tar -xzf order-service.tar.gz -C certs    # certs/client.key、client.crt、ca.crt
rm order-service.tar.gz
```

## 4. 加入呼叫端名單（gateway）

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

## 5. 呼叫端的 Go 設定

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
		cert, err := tls.LoadX509KeyPair("certs/client.crt", "certs/client.key")
		return &cert, err
	},
})
conn, err := grpc.NewClient("mailgw.example.com:20050", grpc.WithTransportCredentials(creds))
```

gateway 設有 `max_connection_age`（預設 1 小時），連線會定期重建，換發的憑證最晚在這段時間後開始使用。

## 6. 換發（到期前）

gateway 在 server 或 client 憑證剩餘有效期少於 `tls.expiry_warning`（預設 30 天）時會記 warning log（`certificate expires soon`，同一張憑證每天最多一次），可以用來安排換發。查看到期日：

```bash
./cert.sh show order-service.tar.gz     # tar.gz 裡的憑證
./cert.sh show certs/client.crt         # 已配置的憑證
```

- **client 憑證**：在簽發機重新執行步驟 3，**沿用同一個 CN**，`[clients]` 不用改。呼叫端解開新的 tar.gz 覆蓋舊檔即可，不需重啟。
- **server 憑證**：在簽發機重新執行步驟 2，解到 gateway 覆蓋舊檔，再執行 `systemctl reload maild`。
- **CA 憑證**：gateway 的到期提醒不檢查 CA 憑證，請用 `./cert.sh show ~/maild-ca/ca.crt` 自行確認。到期前以同一把 `ca.key`、同一個 subject 重簽：

  ```bash
  ~/maild-ca/cert.sh ca-renew -k ~/maild-ca/ca.key    # 產生 ~/maild-ca/ca.renewed.crt
  ```

  在簽發機把 `ca.renewed.crt` 改名成 `ca.crt` 取代舊檔，再把新的 `ca.crt` 複製到 gateway 的 `/opt/maild/certs/` 與所有呼叫端的 `certs/`，gateway 執行 `systemctl reload maild`。新舊 CA 憑證驗證的是同一個簽章，換的順序不拘、不必串接，已簽出的 server／client 憑證也都不用重簽；之後產生的 tar.gz 會帶新的 `ca.crt`。

## 7. 撤銷呼叫端

Go 不會自動檢查 CRL／OCSP。撤銷的做法是把呼叫端從 `[clients]` 刪掉，再執行 `systemctl reload maild`。名單在每次 RPC 都會檢查，reload 後立即生效，已建立的連線也一樣。

## 8. 換 CA

換 CA 用於 `ca.key` 外洩或要換金鑰；只是 CA 憑證快到期、`ca.key` 沒有外洩時，用步驟 6 的 `ca-renew` 即可。

換 CA 期間新舊 CA 要並存，讓還沒換的一方仍連得上。以下都在 `~/maild-ca` 執行：

1. 建立新 CA，並把新舊 CA 串接成一個檔案：
   ```bash
   ./cert.sh ca -k ~/maild-ca/new-ca.key          # 產生 new-ca.key、new-ca.crt
   cat new-ca.crt ca.crt > both-ca.crt
   ```
2. gateway 的 `/opt/maild/certs/ca.crt` 換成 `both-ca.crt` 並 reload：新舊 CA 簽的 client 憑證都接受。
3. 逐一換呼叫端：`./cert.sh client -k ~/maild-ca/new-ca.key <CN>`，解到呼叫端的 `certs/` 後，再把呼叫端的 `certs/ca.crt` 換成 `both-ca.crt`（gateway 的 server 憑證此時還是舊 CA 簽的）。
4. 所有呼叫端都換完後，換 gateway 的 server 憑證：`./cert.sh server -k ~/maild-ca/new-ca.key ...`，只解出私鑰與憑證，保留步驟 2 串接的 `ca.crt`，再 reload：
   ```bash
   tar -xzf server.tar.gz -C /opt/maild/certs server.key server.crt
   ```
5. 收尾：gateway 與所有呼叫端的 `ca.crt` 換成只有新 CA 的 `new-ca.crt`，gateway 再 reload 一次。簽發機把 `new-ca.key`／`new-ca.crt` 改名成 `ca.key`／`ca.crt`，舊的 `ca.key` 銷毀。

因 `ca.key` 外洩而換 CA 時，持有舊 `ca.key` 的人可以簽出 `[clients]` 裡任何 CN 的憑證，從 `[clients]` 撤銷無法阻擋；步驟 2 到 5 請盡快完成。

## 常見錯誤

| 現象 | 原因 |
|---|---|
| 呼叫端看到 `x509: certificate is valid for ..., not ...` | 連線用的名稱不在 server 憑證的 SAN |
| 呼叫端看到 `certificate signed by unknown authority` | 呼叫端的 `ca.crt` 不是簽 server 憑證的那個 CA |
| gateway log `TLS handshake failed`：`client didn't provide a certificate` | 呼叫端沒有設定 client 憑證（需開啟 `server.connection_log` 才會記錄） |
| gateway log `TLS handshake failed`：`x509: certificate has expired or is not yet valid` | client 憑證過期 |
| gateway log `TLS handshake failed`：`x509: certificate signed by unknown authority` | client 憑證不是 gateway 信任的 CA 簽的 |
| RPC 回 `PermissionDenied: client not allowed` | 憑證有效，但 CN 不在 `[clients]` |
