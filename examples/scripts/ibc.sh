#!/usr/bin/env bash
# A small command-line client for an ibc node, built on curl and jq.
#
#   export IBC_URL=http://localhost:2611   # the node (this is the default)
#   export IBC_AGENT=alice                 # who you are (required)
#
#   ibc.sh register                              register IBC_AGENT on the node
#   ibc.sh exists                                is IBC_AGENT registered?
#   ibc.sh deploy NAME [CONSTRUCTOR_JSON]        deploy build/NAME.bin; prints the contract id
#   ibc.sh write CONTRACT METHOD [VALUES_JSON]   a transaction; waits for and prints its result
#   ibc.sh read CONTRACT METHOD [VALUES_JSON]    a read-only call; prints its result
#   ibc.sh contracts                             list IBC_AGENT's contracts
#   ibc.sh contract CONTRACT                     one contract's summary
#   ibc.sh join CONTRACT PARTNER_URL PARTNER_AGENT   ask a partner to let IBC_AGENT join
#   ibc.sh ledger CONTRACT [FROM_INDEX]          the contract's ledger records
#   ibc.sh stream [CONTRACT]                     follow the event stream (Ctrl-C to stop)
#
# Example:
#   ibc.sh register
#   C=$(ibc.sh deploy Counter '{"start": 10}')
#   ibc.sh write "$C" increment '{"amount": 5}'     # -> 15
#   ibc.sh read "$C" count                          # -> 15
set -euo pipefail

IBC_URL=${IBC_URL:-http://localhost:2611}
IBC_URL=${IBC_URL%/}
BUILD=${IBC_BUILD:-$(cd "$(dirname "$0")/.." && pwd)/build}
TIMEOUT=${IBC_TIMEOUT:-30}

die() { echo "ibc.sh: $*" >&2; exit 1; }
need_agent() { [ -n "${IBC_AGENT:-}" ] || die "set IBC_AGENT to your agent id"; }

# request VERB PATH ACTION [BODY]: one call to /ibc/app.
request() {
    local verb=$1 path=$2 action=$3 body=${4:-}
    if [ -n "$body" ]; then
        curl -sS -X "$verb" "$IBC_URL/ibc/app/$path?action=$action" \
            -H 'Content-Type: application/json' --data-binary "$body"
    else
        curl -sS -X "$verb" "$IBC_URL/ibc/app/$path?action=$action"
    fi
}

# fail_on_message: an error reply looks like {"message": "..."}.
fail_on_message() {
    local reply=$1
    if jq -e 'type == "object" and has("message")' >/dev/null 2>&1 <<<"$reply"; then
        die "$(jq -r .message <<<"$reply")"
    fi
}

# submit_and_wait SCOPE VERB PATH ACTION BODY: sends a request whose outcome
# arrives on the event stream, and prints that event.
submit_and_wait() {
    local scope=$1 verb=$2 path=$3 action=$4 body=$5
    local events
    events=$(mktemp)
    curl -sN "$IBC_URL/stream?agent=$IBC_AGENT&contract=$scope" >"$events" &
    local stream=$!
    trap 'kill $stream 2>/dev/null; rm -f "$events"' RETURN
    # Wait for the stream's first (empty) message: we are subscribed.
    for _ in $(seq 100); do [ -s "$events" ] && break; sleep 0.05; done
    [ -s "$events" ] || die "could not open the event stream at $IBC_URL"

    local ack
    ack=$(request "$verb" "$path" "$action" "$body")
    fail_on_message "$ack"
    ack=$(jq -r . <<<"$ack")
    for _ in $(seq $((TIMEOUT * 20))); do
        local event
        event=$(sed -n 's/^data: //p' "$events" | jq -c --arg r "$ack" 'select(.request == $r)' 2>/dev/null | head -n 1)
        if [ -n "$event" ]; then
            echo "$event"
            return
        fi
        sleep 0.05
    done
    die "no result for request $ack after ${TIMEOUT}s"
}

cmd=${1:-}
[ -n "$cmd" ] || { sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
shift

case "$cmd" in
register)
    need_agent
    request PUT "$IBC_AGENT" register_agent "$(jq -n --arg a "$IBC_URL" '{address: $a}')" >/dev/null
    echo "registered $IBC_AGENT at $IBC_URL"
    ;;
exists)
    need_agent
    request GET "$IBC_AGENT" is_exist_agent
    ;;
deploy)
    need_agent
    name=${1:?usage: ibc.sh deploy NAME [CONSTRUCTOR_JSON]}
    ctor=${2:-{\}}
    [ -f "$BUILD/$name.bin" ] || die "no $BUILD/$name.bin (run examples/scripts/compile.sh)"
    body=$(jq -n --arg pid "$IBC_AGENT" --arg address "$IBC_URL" --arg name "$name" \
        --arg code "$(tr -d '[:space:]' <"$BUILD/$name.bin")" \
        --rawfile abi "$BUILD/$name.abi" --argjson ctor "$ctor" \
        '{name: $name, contract: ($name + ".sol"), protocol: "BFT", default_app: "",
          pid: $pid, address: $address, profile: "",
          code: $code, abi: $abi, constructor: $ctor}')
    event=$(submit_and_wait "" PUT "$IBC_AGENT" deploy_contract "$body")
    [ "$(jq -r .reply <<<"$event")" = "true" ] || die "deploy failed: $(jq -r .reply <<<"$event")"
    jq -r .contract <<<"$event"
    ;;
write)
    need_agent
    contract=${1:?usage: ibc.sh write CONTRACT METHOD [VALUES_JSON]}
    method=${2:?usage: ibc.sh write CONTRACT METHOD [VALUES_JSON]}
    values=${3:-{\}}
    event=$(submit_and_wait "$contract" POST "$IBC_AGENT/$contract/$method" contract_write \
        "$(jq -n --argjson v "$values" '{values: $v}')")
    jq -c .reply <<<"$event"
    ;;
read)
    need_agent
    contract=${1:?usage: ibc.sh read CONTRACT METHOD [VALUES_JSON]}
    method=${2:?usage: ibc.sh read CONTRACT METHOD [VALUES_JSON]}
    values=${3:-{\}}
    reply=$(request POST "$IBC_AGENT/$contract/$method" contract_read "$(jq -n --argjson v "$values" '{values: $v}')")
    fail_on_message "$reply"
    jq -c . <<<"$reply"
    ;;
contracts)
    need_agent
    request GET "$IBC_AGENT" get_contracts | jq '[.[] | {id, name, contract, evm_address, block_number}]'
    ;;
contract)
    need_agent
    request GET "$IBC_AGENT/${1:?usage: ibc.sh contract CONTRACT}" get_contract | jq 'del(.code, .abi)'
    ;;
join)
    need_agent
    contract=${1:?usage: ibc.sh join CONTRACT PARTNER_URL PARTNER_AGENT}
    partner_url=${2:?usage: ibc.sh join CONTRACT PARTNER_URL PARTNER_AGENT}
    partner=${3:?usage: ibc.sh join CONTRACT PARTNER_URL PARTNER_AGENT}
    reply=$(request PUT "$IBC_AGENT" join_contract "$(jq -n --arg c "$contract" --arg a "$partner_url" \
        --arg p "$partner" '{contract: $c, address: $a, agent: $p, profile: ""}')")
    fail_on_message "$reply"
    [ "$reply" = "null" ] && die "$IBC_AGENT already has contract $contract"
    echo "join request sent; the answer arrives on the event stream as a2a_reply_join"
    ;;
ledger)
    need_agent
    contract=${1:?usage: ibc.sh ledger CONTRACT [FROM_INDEX]}
    request POST "$IBC_AGENT/$contract" a2a_get_ledger "$(jq -n --argjson i "${2:-0}" '{msg: {index: $i}}')" |
        jq 'to_entries | map({index: (.key | tonumber), action: .value.action, agent: .value.agent,
                              method: .value.method, timestamp: .value.timestamp})'
    ;;
stream)
    need_agent
    curl -sN "$IBC_URL/stream?agent=$IBC_AGENT&contract=${1:-}" | sed -un 's/^data: \(..*\)/\1/p'
    ;;
*)
    die "unknown command $cmd (run without arguments for help)"
    ;;
esac
