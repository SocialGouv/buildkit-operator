#!/usr/bin/env bash
#
# create-certs.sh — mint the shared mTLS material for buildkit-operator's buildkit fleet.
#
# Adapted from the upstream BuildKit example
# (.repos/buildkit/examples/kubernetes/create-certs.sh) with one crucial change:
# buildkit-operator creates ONE buildkitd Service per (project, arch) DYNAMICALLY at
# runtime, so we cannot enumerate the daemon hostnames ahead of time. Instead we
# issue a single daemon server certificate with a WILDCARD SAN that covers every
# Service DNS name in the target namespace:
#
#     *.<NS>.svc            *.<NS>.svc.cluster.local            localhost   127.0.0.1
#
# One cert therefore validates `buildkitd-<key>-<arch>.<NS>.svc` for any project
# the controller spins up. The matching client certificate (SAN: client) is what
# buildd and `buildctl` present to the daemons.
#
# Output (under ./.certs, relative to this script):
#   ca.pem  cert.pem  key.pem              # raw PEM, daemon side
#   client/{ca.pem,cert.pem,key.pem}       # raw PEM, client side
#   buildkit-daemon-certs.yaml             # Secret: ca.pem / cert.pem / key.pem
#   buildkit-client-certs.yaml             # Secret: ca.pem / cert.pem / key.pem
#
# The script is idempotent: an existing CA is reused (so re-running does not
# invalidate already-deployed client certs), and leaf certs are only re-minted
# when missing. Delete ./.certs to start from a fresh CA.
#
# Backend: prefers `mkcert` if installed (quick local dev), otherwise falls back
# to `openssl` (portable, no extra tooling). Both produce the same file layout.
#
# Usage:
#   ./create-certs.sh [NAMESPACE]
#   NAMESPACE defaults to "buildkit-builds".

set -o errexit
set -o nounset
set -o pipefail
set -o errtrace

PRODUCT=buildkit
NS="${1:-buildkit-builds}"

# Resolve paths relative to this script so it works from any CWD.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIR="${SCRIPT_DIR}/.certs"
CLIENT_DIR="${DIR}/client"

# Validity and key strength for the openssl path.
DAYS=3650
KEY_BITS=4096

# Wildcard daemon SANs: cover every Service DNS name in the namespace, plus
# loopback for port-forwarded `buildctl` access.
SAN_DNS_1="*.${NS}.svc"
SAN_DNS_2="*.${NS}.svc.cluster.local"
SAN_DNS_3="localhost"
SAN_IP_1="127.0.0.1"
SAN_CLIENT="client"
# Off-cluster CI via the shared SNI gateway: set GATEWAY_HOST=builds.example.com to also cover
# *.${GATEWAY_HOST} so the client's SNI (<daemon>.${GATEWAY_HOST}) validates against the daemon cert.
# GATEWAY_HOST may be COMMA-SEPARATED for several client domains (e.g. a public + a CI-platform host);
# each adds a *.<host> wildcard SAN (matches the gateway's multi-domain --domain list).
GATEWAY_HOST="${GATEWAY_HOST:-}"
GATEWAY_SANS=""      # mkcert: space-separated "*.<host>" args
GATEWAY_DNS_LINES="" # openssl [alt_names]: "DNS.N = *.<host>" lines
if [ -n "${GATEWAY_HOST}" ]; then
  _n=4; _oifs="$IFS"; IFS=','
  for _h in ${GATEWAY_HOST}; do
    _h="$(printf '%s' "${_h}" | tr -d ' ')"; [ -n "${_h}" ] || continue
    GATEWAY_SANS="${GATEWAY_SANS} *.${_h}"
    GATEWAY_DNS_LINES="${GATEWAY_DNS_LINES}DNS.${_n} = *.${_h}
"
    _n=$((_n + 1))
  done
  IFS="$_oifs"
fi

mkdir -p "${DIR}" "${CLIENT_DIR}"

echo ">> buildkit-operator mTLS certs"
echo "   namespace : ${NS}"
echo "   output    : ${DIR}"
echo "   daemon SAN: DNS:${SAN_DNS_1}, DNS:${SAN_DNS_2}, DNS:${SAN_DNS_3}${GATEWAY_HOST:+, DNS:*.${GATEWAY_HOST}}, IP:${SAN_IP_1}"
echo "   client SAN: DNS:${SAN_CLIENT}"
echo

# ---------------------------------------------------------------------------
# Backend: mkcert (preferred if present)
# ---------------------------------------------------------------------------
gen_with_mkcert() {
  echo ">> backend: mkcert"
  (
    cd "${DIR}"
    # Use a CAROOT local to ./.certs so we never touch the user's system trust
    # store. mkcert writes rootCA.pem / rootCA-key.pem there.
    export CAROOT="${DIR}"

    # Daemon server cert with the wildcard SANs.
    # shellcheck disable=SC2086
    mkcert -cert-file cert.pem -key-file key.pem \
      "${SAN_DNS_1}" "${SAN_DNS_2}" "${SAN_DNS_3}" "${SAN_IP_1}" \
      ${GATEWAY_SANS} >/dev/null 2>&1

    # Client cert (clientAuth EKU via -client).
    mkcert -client -cert-file "client/cert.pem" -key-file "client/key.pem" \
      "${SAN_CLIENT}" >/dev/null 2>&1

    cp -f rootCA.pem ca.pem
    cp -f rootCA.pem "client/ca.pem"
  )
}

# ---------------------------------------------------------------------------
# Backend: openssl (portable fallback)
# ---------------------------------------------------------------------------
gen_with_openssl() {
  echo ">> backend: openssl"

  # --- CA (reused if it already exists: keeps deployed client certs valid) ---
  if [[ -f "${DIR}/ca.pem" && -f "${DIR}/ca-key.pem" ]]; then
    echo "   CA: reusing existing ${DIR}/ca.pem"
  else
    echo "   CA: generating new root CA"
    openssl genrsa -out "${DIR}/ca-key.pem" "${KEY_BITS}" 2>/dev/null
    openssl req -x509 -new -nodes -sha256 -days "${DAYS}" \
      -key "${DIR}/ca-key.pem" \
      -subj "/CN=buildkit-operator-ca/O=buildkit-operator" \
      -out "${DIR}/ca.pem" 2>/dev/null
  fi
  # Both bundles trust the same CA.
  cp -f "${DIR}/ca.pem" "${CLIENT_DIR}/ca.pem"

  # --- Daemon server cert (wildcard SAN, serverAuth EKU) ---
  if [[ -f "${DIR}/cert.pem" && -f "${DIR}/key.pem" ]]; then
    echo "   daemon: reusing existing ${DIR}/cert.pem"
  else
    echo "   daemon: minting server cert with wildcard SAN"
    local daemon_cnf="${DIR}/daemon-openssl.cnf"
    cat >"${daemon_cnf}" <<EOF
[req]
distinguished_name = dn
req_extensions     = v3_req
prompt             = no

[dn]
CN = buildkitd.${NS}
O  = buildkit-operator

[v3_req]
basicConstraints = CA:FALSE
keyUsage         = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName   = @alt_names

[alt_names]
DNS.1 = ${SAN_DNS_1}
DNS.2 = ${SAN_DNS_2}
DNS.3 = ${SAN_DNS_3}
${GATEWAY_DNS_LINES}IP.1  = ${SAN_IP_1}
EOF
    openssl genrsa -out "${DIR}/key.pem" "${KEY_BITS}" 2>/dev/null
    openssl req -new -key "${DIR}/key.pem" \
      -config "${daemon_cnf}" \
      -out "${DIR}/daemon.csr" 2>/dev/null
    openssl x509 -req -sha256 -days "${DAYS}" \
      -in "${DIR}/daemon.csr" \
      -CA "${DIR}/ca.pem" -CAkey "${DIR}/ca-key.pem" -CAcreateserial \
      -extensions v3_req -extfile "${daemon_cnf}" \
      -out "${DIR}/cert.pem" 2>/dev/null
    rm -f "${DIR}/daemon.csr"
  fi

  # --- Client cert (SAN: client, clientAuth EKU) ---
  if [[ -f "${CLIENT_DIR}/cert.pem" && -f "${CLIENT_DIR}/key.pem" ]]; then
    echo "   client: reusing existing ${CLIENT_DIR}/cert.pem"
  else
    echo "   client: minting client cert"
    local client_cnf="${DIR}/client-openssl.cnf"
    cat >"${client_cnf}" <<EOF
[req]
distinguished_name = dn
req_extensions     = v3_req
prompt             = no

[dn]
CN = ${SAN_CLIENT}
O  = buildkit-operator

[v3_req]
basicConstraints = CA:FALSE
keyUsage         = critical, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth
subjectAltName   = @alt_names

[alt_names]
DNS.1 = ${SAN_CLIENT}
EOF
    openssl genrsa -out "${CLIENT_DIR}/key.pem" "${KEY_BITS}" 2>/dev/null
    openssl req -new -key "${CLIENT_DIR}/key.pem" \
      -config "${client_cnf}" \
      -out "${DIR}/client.csr" 2>/dev/null
    openssl x509 -req -sha256 -days "${DAYS}" \
      -in "${DIR}/client.csr" \
      -CA "${DIR}/ca.pem" -CAkey "${DIR}/ca-key.pem" -CAcreateserial \
      -extensions v3_req -extfile "${client_cnf}" \
      -out "${CLIENT_DIR}/cert.pem" 2>/dev/null
    rm -f "${DIR}/client.csr"
  fi
}

if command -v mkcert >/dev/null 2>&1; then
  gen_with_mkcert
elif command -v openssl >/dev/null 2>&1; then
  gen_with_openssl
else
  echo "ERROR: neither mkcert nor openssl found in PATH." >&2
  echo "Install one of: https://github.com/FiloSottile/mkcert  or  openssl." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Render the two Secret manifests.
#
# We do NOT shell out to kubectl (the script must run on a build box with no
# cluster access), so the Secrets are rendered directly with base64-encoded
# data. Key names match the upstream example: ca.pem / cert.pem / key.pem.
# ---------------------------------------------------------------------------
b64() {
  # -w0: single line (GNU coreutils). BSD base64 ignores -w but does not wrap.
  base64 -w0 "$1" 2>/dev/null || base64 "$1" | tr -d '\n'
}

render_secret() {
  local name="$1" ca="$2" cert="$3" key="$4" out="$5"
  cat >"${out}" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${name}
  namespace: ${NS}
  labels:
    app.kubernetes.io/name: buildkit-operator
    app.kubernetes.io/component: buildd
type: Opaque
data:
  ca.pem: $(b64 "${ca}")
  cert.pem: $(b64 "${cert}")
  key.pem: $(b64 "${key}")
EOF
}

DAEMON_SECRET="${DIR}/${PRODUCT}-daemon-certs.yaml"
CLIENT_SECRET="${DIR}/${PRODUCT}-client-certs.yaml"

render_secret "${PRODUCT}-daemon-certs" \
  "${DIR}/ca.pem" "${DIR}/cert.pem" "${DIR}/key.pem" "${DAEMON_SECRET}"
render_secret "${PRODUCT}-client-certs" \
  "${CLIENT_DIR}/ca.pem" "${CLIENT_DIR}/cert.pem" "${CLIENT_DIR}/key.pem" "${CLIENT_SECRET}"

echo
echo ">> done. Apply the Secrets into the builds namespace:"
echo
echo "   kubectl create namespace ${NS} --dry-run=client -o yaml | kubectl apply -f -"
echo "   kubectl apply -f ${DAEMON_SECRET}"
echo "   kubectl apply -f ${CLIENT_SECRET}"
echo
echo "   The daemon Secret (${PRODUCT}-daemon-certs) is mounted by every"
echo "   per-project buildkitd StatefulSet; distribute the client Secret"
echo "   (${PRODUCT}-client-certs) to CI clients. buildd does not mount it."
