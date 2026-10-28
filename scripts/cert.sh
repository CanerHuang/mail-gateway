#!/usr/bin/env bash
# cert.sh — 簽發 maild mTLS 憑證（ECDSA P-256）。完整流程見 doc/certificates.md。
#
# 用法：
#   ./cert.sh ca [CN]                 建立 CA：ca.key、ca.crt（效期 CA_DAYS，預設 3650 天）
#   ./cert.sh server <DNS 或 IP>...   簽 gateway 的 server 憑證：server.key、server.crt
#                                     參數全部寫進 SAN，第一個同時當 CN
#   ./cert.sh csr <CN>                【呼叫端執行】產生 <CN>.key 與 <CN>.csr，把 .csr 交給管理者
#   ./cert.sh sign <file.csr>         簽 client 憑證：先顯示 CN 讓管理者確認，產生 <CN>.crt
#   ./cert.sh show <file.crt>         顯示憑證的 CN、SAN、用途與到期日
#
# 環境變數：
#   CA_KEY / CA_CRT   CA 檔案位置（預設為目前目錄的 ca.key、ca.crt）
#   CA_DAYS           CA 效期天數（預設 3650）
#   CERT_DAYS         server／client 憑證效期天數（預設 365）
#
# 產出的檔案都放在目前目錄；私鑰權限為 600。ca.key 有它就能簽出任何 client 憑證，
# 只放在簽憑證的機器，不要放到 gateway 上。

set -euo pipefail

CA_KEY="${CA_KEY:-ca.key}"
CA_CRT="${CA_CRT:-ca.crt}"
CA_DAYS="${CA_DAYS:-3650}"
CERT_DAYS="${CERT_DAYS:-365}"

usage() {
    sed -n '2,20p' "$0"
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

require_ca() {
    [[ -f "$CA_KEY" && -f "$CA_CRT" ]] || die "CA not found ($CA_KEY, $CA_CRT); run '$0 ca' first or set CA_KEY / CA_CRT"
}

new_key() {
    (umask 077 && openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1")
}

# CN 只建議用英數字、底線與連字號：CN 是 maild.toml 的 [clients] key，含 . 等字元時 key 要加引號。
check_cn() {
    [[ -n "$1" ]] || die "CN must not be empty"
    if [[ ! "$1" =~ ^[A-Za-z0-9_-]+$ ]]; then
        echo "warning: CN '$1' contains characters other than letters, digits, '_' and '-';" >&2
        echo "         its [clients] key in maild.toml must be quoted, e.g. [clients.\"$1\"]" >&2
    fi
}

# sign_with_ca <csr> <crt> <extensions>：以 CA 簽發憑證，只套用這裡指定的 extension，
# 不沿用 CSR 裡自帶的 extension。
sign_with_ca() {
    local csr="$1" crt="$2" ext="$3"
    openssl x509 -req -in "$csr" -CA "$CA_CRT" -CAkey "$CA_KEY" \
        -set_serial "0x$(openssl rand -hex 16)" -days "$CERT_DAYS" -sha256 \
        -extfile <(printf '[v3]\n%s\n' "$ext") -extensions v3 -out "$crt" 2>/dev/null
}

cmd_ca() {
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
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
EOF
)
    echo "created $CA_KEY (keep it on the signing machine only) and $CA_CRT"
    cmd_show "$CA_CRT"
}

cmd_server() {
    [[ $# -gt 0 ]] || die "usage: $0 server <DNS or IP>..."
    require_ca
    require_absent server.key server.crt

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

    new_key server.key
    local csr
    csr="$(mktemp)"
    trap 'rm -f "$csr"' RETURN
    openssl req -new -key server.key -subj "/CN=$1" -out "$csr"
    sign_with_ca "$csr" server.crt "basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = $san_list
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid"
    echo "created server.key and server.crt; copy both with $CA_CRT to the gateway's certs/ directory"
    cmd_show server.crt
}

cmd_csr() {
    [[ $# -eq 1 ]] || die "usage: $0 csr <CN>"
    local cn="$1"
    check_cn "$cn"
    require_absent "$cn.key" "$cn.csr"
    new_key "$cn.key"
    openssl req -new -key "$cn.key" -subj "/CN=$cn" -out "$cn.csr"
    echo "created $cn.key (keep it on this machine) and $cn.csr; send $cn.csr to the administrator"
}

cmd_sign() {
    [[ $# -eq 1 ]] || die "usage: $0 sign <file.csr>"
    local csr="$1"
    [[ -f "$csr" ]] || die "$csr not found"
    require_ca
    openssl req -in "$csr" -noout -verify 2>/dev/null || die "$csr signature verification failed"

    local subject cn
    subject="$(openssl req -in "$csr" -noout -subject -nameopt RFC2253)"
    cn="$(sed -n 's/^subject=.*CN=\([^,]*\).*$/\1/p' <<<"$subject")"
    [[ -n "$cn" ]] || die "$csr has no CN ($subject)"
    openssl req -in "$csr" -noout -text | grep -q 'prime256v1\|P-256' || die "$csr is not an ECDSA P-256 key"
    check_cn "$cn"
    require_absent "$cn.crt"

    echo "CSR subject: $subject"
    echo "The CN becomes the client name in maild.toml [clients]. Signing a CN that belongs to"
    echo "another service lets the requester impersonate that service."
    read -r -p "Sign a client certificate for CN=$cn? [y/N] " answer
    [[ "$answer" == "y" || "$answer" == "Y" ]] || die "aborted"

    sign_with_ca "$csr" "$cn.crt" "basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid"
    echo "created $cn.crt; send $cn.crt and $CA_CRT back to the client"
    echo "add [clients.$cn] to maild.toml (if new) and run: systemctl reload maild"
    cmd_show "$cn.crt"
}

cmd_show() {
    [[ $# -eq 1 ]] || die "usage: $0 show <file.crt>"
    openssl x509 -in "$1" -noout -subject -issuer -enddate \
        -ext subjectAltName,extendedKeyUsage,basicConstraints 2>/dev/null
}

[[ $# -ge 1 ]] || usage
command="$1"
shift
case "$command" in
    ca) cmd_ca "$@" ;;
    server) cmd_server "$@" ;;
    csr) cmd_csr "$@" ;;
    sign) cmd_sign "$@" ;;
    show) cmd_show "$@" ;;
    -h|--help|help) usage ;;
    *) echo "unknown command: $command" >&2; usage ;;
esac
