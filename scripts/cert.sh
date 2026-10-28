#!/usr/bin/env bash
# cert.sh — 簽發 maild mTLS 憑證（ECDSA P-256）。完整流程見 doc/certificates.md。
#
# 所有私鑰與憑證都在簽發機（放 ca.key 與本腳本的機器）產生，打包成 tar.gz 後再配置到
# gateway 與各呼叫端。
#
# 用法：
#   ./cert.sh ca [-k ca.key] [CN]                建立 CA：ca.key、ca.crt（效期 CA_DAYS，預設 3650 天）
#   ./cert.sh ca-renew [-k ca.key]               CA 憑證續期：以同一把 key、同一個 subject 重簽，產生 ca.renewed.crt
#                                                （與 ca.crt 同資料夾）；已簽出的 server／client 憑證不必重簽
#   ./cert.sh server [-k ca.key] <DNS 或 IP>...  簽發 gateway 憑證，產生 server.tar.gz（server.key、server.crt、ca.crt）
#                                                參數全部寫進 SAN，第一個同時當 CN
#   ./cert.sh client [-k ca.key] <CN>            簽發呼叫端憑證，產生 <CN>.tar.gz（client.key、client.crt、ca.crt）
#                                                CN 就是 maild.toml 的 [clients] key
#   ./cert.sh show <file.crt | file.tar.gz>      顯示憑證的 CN、SAN、用途與到期日
#
# CA 選項（ca、ca-renew、server、client）：
#   -k, --ca-key <file>   ca.key 的位置
#   -c, --ca-crt <file>   ca.crt 的位置；只給 -k 時與 ca.key 同資料夾、同主檔名（xxx.key → xxx.crt）
#
# 環境變數：
#   CA_KEY / CA_CRT   未給 -k／-c 時的 CA 檔案位置（預設為目前目錄的 ca.key、ca.crt）
#   CA_DAYS           CA 效期天數（預設 3650）
#   CERT_DAYS         server／client 憑證效期天數（預設 365）
#
# tar.gz 產生在目前目錄，ca、ca-renew 的檔案放在 CA 檔案的位置。tar.gz 內含私鑰，權限為 600，
# 請以加密管道傳送，配置完成後刪除。ca.key 有它就能簽出任何 client 憑證，只放在簽發機。

set -euo pipefail

CA_KEY="${CA_KEY:-ca.key}"
CA_CRT="${CA_CRT:-ca.crt}"
CA_DAYS="${CA_DAYS:-3650}"
CERT_DAYS="${CERT_DAYS:-365}"

# CA 憑證的 extension：只能簽發憑證。ca 與 ca-renew 共用，續期後的憑證才會與原本一致。
CA_EXT="basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash"

# 暫存資料夾：放產生中的私鑰與 CSR，腳本結束時（含錯誤中止）一律刪除。
WORK_DIR=""
cleanup() {
    [[ -z "$WORK_DIR" ]] || rm -rf "$WORK_DIR"
}
trap cleanup EXIT

usage() {
    awk 'NR == 1 { next } /^#/ { print; next } { exit }' "$0"
    exit 1
}

die() {
    echo "error: $*" >&2
    exit 1
}

# 不覆寫既有檔案，避免誤把正在使用的憑證或私鑰蓋掉。
require_absent() {
    for f in "$@"; do
        [[ ! -e "$f" ]] || die "$f already exists; move it away first"
    done
}

# parse_ca_opts "$@"：解析 -k／-c 並設定 CA_KEY、CA_CRT，其餘參數依序放進 ARGS。
ARGS=()
parse_ca_opts() {
    local key="" crt=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -k|--ca-key) [[ $# -ge 2 ]] || die "$1 requires a file"; key="$2"; shift 2 ;;
            --ca-key=*) key="${1#*=}"; shift ;;
            -c|--ca-crt) [[ $# -ge 2 ]] || die "$1 requires a file"; crt="$2"; shift 2 ;;
            --ca-crt=*) crt="${1#*=}"; shift ;;
            --) shift; ARGS+=("$@"); break ;;
            -*) die "unknown option: $1" ;;
            *) ARGS+=("$1"); shift ;;
        esac
    done
    if [[ -n "$key" ]]; then
        CA_KEY="$key"
        if [[ -z "$crt" ]]; then
            [[ "$key" == *.key ]] || die "cannot derive the CA certificate path from $key; specify it with -c"
            crt="${key%.key}.crt"
        fi
    fi
    [[ -z "$crt" ]] || CA_CRT="$crt"
}

require_ca() {
    [[ -f "$CA_KEY" && -f "$CA_CRT" ]] || die "CA not found ($CA_KEY, $CA_CRT); run '$0 ca' first or specify them with -k / -c"
    # ca.key 與 ca.crt 不是同一組時 openssl 簽發會失敗，先在這裡給出明確的錯誤。
    local key_pub crt_pub
    key_pub="$(openssl pkey -in "$CA_KEY" -pubout 2>/dev/null)" || die "cannot read $CA_KEY"
    crt_pub="$(openssl x509 -in "$CA_CRT" -noout -pubkey 2>/dev/null)" || die "cannot read $CA_CRT"
    [[ "$key_pub" == "$crt_pub" ]] || die "$CA_KEY does not match $CA_CRT"
}

new_key() {
    (umask 077 && openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1")
}

# CN 是 maild.toml 的 [clients] key，只接受英數字、底線、連字號與點；含點時 key 要加引號。
check_cn() {
    [[ "$1" =~ ^[A-Za-z0-9_.-]+$ ]] || die "CN '$1' may only contain letters, digits, '_', '-' and '.'"
    if [[ "$1" == *.* ]]; then
        echo "warning: CN '$1' contains '.'; its [clients] key in maild.toml must be quoted, e.g. [clients.\"$1\"]" >&2
    fi
}

# sign_with_ca <csr> <crt> <extensions>：以 CA 簽發憑證，只套用這裡指定的 extension，
# 不沿用 CSR 裡自帶的 extension。
sign_with_ca() {
    local csr="$1" crt="$2" ext="$3" out
    out="$(openssl x509 -req -in "$csr" -CA "$CA_CRT" -CAkey "$CA_KEY" \
        -set_serial "0x$(openssl rand -hex 16)" -days "$CERT_DAYS" -sha256 \
        -extfile <(printf '[v3]\n%s\n' "$ext") -extensions v3 -out "$crt" 2>&1)" || die "signing $(basename "$crt") failed: $out"
}

# make_bundle <out.tar.gz> <name> <CN> <extensions>：在暫存資料夾產生私鑰並以 CA 簽發憑證，
# 連同 CA 憑證打包成 <name>.key、<name>.crt、ca.crt。
make_bundle() {
    local out="$1" name="$2" cn="$3" ext="$4"
    WORK_DIR="$(mktemp -d)"
    new_key "$WORK_DIR/$name.key"
    openssl req -new -key "$WORK_DIR/$name.key" -subj "/CN=$cn" -out "$WORK_DIR/$name.csr"
    sign_with_ca "$WORK_DIR/$name.csr" "$WORK_DIR/$name.crt" "$ext"
    # gateway 與呼叫端的設定固定讀 ca.crt，不論 CA 檔案原本叫什麼，包裡都叫 ca.crt。
    cp "$CA_CRT" "$WORK_DIR/ca.crt"
    chmod 644 "$WORK_DIR/$name.crt" "$WORK_DIR/ca.crt"
    (umask 077 && tar -czf "$WORK_DIR/bundle.tar.gz" -C "$WORK_DIR" "$name.key" "$name.crt" ca.crt)
    mv "$WORK_DIR/bundle.tar.gz" "$out"
}

cmd_ca() {
    [[ $# -le 1 ]] || die "usage: $0 ca [-k ca.key] [CN]"
    local cn="${1:-maild-ca}"
    require_absent "$CA_KEY" "$CA_CRT"
    new_key "$CA_KEY"
    openssl req -x509 -new -key "$CA_KEY" -days "$CA_DAYS" -sha256 -out "$CA_CRT" -config <(cat <<EOF
[req]
distinguished_name = dn
prompt = no
x509_extensions = v3
[dn]
CN = $cn
[v3]
$CA_EXT
EOF
)
    echo "created $CA_KEY (keep it on the signing machine only) and $CA_CRT"
    cmd_show "$CA_CRT"
}

# 續期後的 CA 憑證與原本同一把 key、同一個 subject，已簽出的憑證拿新的 CA 憑證一樣驗證得過。
cmd_ca_renew() {
    [[ $# -eq 0 ]] || die "usage: $0 ca-renew [-k ca.key] [-c ca.crt]"
    require_ca
    local out="${CA_CRT%.crt}.renewed.crt"
    require_absent "$out"

    # 用 x509toreq 從舊憑證複製 subject，而不是重新輸入 CN：Go 以 subject 的原始位元組比對
    # 憑證與 CA，字串編碼不同就會對不上。
    local res old_end
    WORK_DIR="$(mktemp -d)"
    old_end="$(openssl x509 -in "$CA_CRT" -noout -enddate | cut -d= -f2)"
    openssl x509 -x509toreq -in "$CA_CRT" -signkey "$CA_KEY" -out "$WORK_DIR/ca.csr" 2>/dev/null || die "cannot read $CA_CRT"
    res="$(openssl x509 -req -in "$WORK_DIR/ca.csr" -signkey "$CA_KEY" \
        -set_serial "0x$(openssl rand -hex 16)" -days "$CA_DAYS" -sha256 \
        -extfile <(printf '[v3]\n%s\n' "$CA_EXT") -extensions v3 -out "$out" 2>&1)" || die "signing $out failed: $res"

    echo "created $out with the same key and subject as $CA_CRT (expires $old_end);"
    echo "certificates signed by this CA stay valid. Before $CA_CRT expires, replace ca.crt"
    echo "on the gateway and every client with $out, then run: systemctl reload maild"
    cmd_show "$out"
}

cmd_server() {
    [[ $# -gt 0 ]] || die "usage: $0 server [-k ca.key] <DNS or IP>..."
    require_ca
    local out="$PWD/server.tar.gz"
    require_absent "$out"

    local san=() name
    for name in "$@"; do
        if [[ "$name" =~ ^[0-9.]+$ || "$name" == *:* ]]; then
            san+=("IP:$name")
        else
            san+=("DNS:$name")
        fi
    done
    local san_list
    san_list="$(IFS=,; echo "${san[*]}")"

    make_bundle "$out" server "$1" "basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = $san_list
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid"
    cmd_show "$out"
    echo
    echo "extract it into the gateway's certs/ directory, then run: systemctl reload maild"
    echo "created $out (server.key, server.crt, ca.crt)"
}

cmd_client() {
    [[ $# -eq 1 ]] || die "usage: $0 client [-k ca.key] <CN>"
    local cn="$1" out="$PWD/$1.tar.gz"
    check_cn "$cn"
    require_ca
    require_absent "$out"

    make_bundle "$out" client "$cn" "basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid"
    cmd_show "$out"
    echo
    echo "deliver it to $cn and extract it into its certs/ directory;"
    echo "add [clients.$cn] to maild.toml (if new) and run: systemctl reload maild"
    echo "created $out (client.key, client.crt, ca.crt)"
}

# show_crt：從標準輸入讀一張憑證並顯示重點欄位。
show_crt() {
    openssl x509 -noout -subject -issuer -enddate \
        -ext subjectAltName,extendedKeyUsage,basicConstraints 2>/dev/null
}

cmd_show() {
    [[ $# -eq 1 ]] || die "usage: $0 show <file.crt | file.tar.gz>"
    local f="$1" member
    [[ -f "$f" ]] || die "$f not found"
    if [[ "$f" == *.tar.gz ]]; then
        member="$(tar -tzf "$f" | grep -E '^(server|client)\.crt$')" || die "$f contains no server.crt or client.crt"
        tar -xzOf "$f" "$member" | show_crt || die "cannot read $member in $f"
    else
        show_crt <"$f" || die "cannot read certificate $f"
    fi
}

[[ $# -ge 1 ]] || usage
command="$1"
shift
# ${ARGS[@]+...}：ARGS 為空時展開成零個參數（bash 4.4 以前在 set -u 下直接展開空陣列會報錯）。
case "$command" in
    ca) parse_ca_opts "$@"; cmd_ca ${ARGS[@]+"${ARGS[@]}"} ;;
    ca-renew) parse_ca_opts "$@"; cmd_ca_renew ${ARGS[@]+"${ARGS[@]}"} ;;
    server) parse_ca_opts "$@"; cmd_server ${ARGS[@]+"${ARGS[@]}"} ;;
    client) parse_ca_opts "$@"; cmd_client ${ARGS[@]+"${ARGS[@]}"} ;;
    show) cmd_show "$@" ;;
    -h|--help|help) usage ;;
    *) echo "unknown command: $command" >&2; usage ;;
esac
