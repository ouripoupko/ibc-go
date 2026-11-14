#!/usr/bin/env bash
# A small command-line client for an ibc node, built on curl, jq and the
# ibc program (which signs with your key files; apps use passkeys instead).
#
#   export IBC_URL=http://localhost:2611   # the node (this is the default)
#   export IBC_KEY=~/.ibc/alice.key        # your root key file (required): your identity
#   export IBC_BIN=./ibc                   # the ibc program (default: ibc on the PATH)
#
#   ibc.sh newkey                                create the root key IBC_KEY
#   ibc.sh whoami                                your agent id (the key's address)
#   ibc.sh register                              register yourself on the node
#   ibc.sh exists                                are you registered?
#   ibc.sh deploy NAME [CONSTRUCTOR_JSON]        deploy contracts/NAME.sol, compiled by the node; prints the contract id
#   ibc.sh write CONTRACT METHOD [VALUES_JSON]   a transaction; waits for and prints its result
#   ibc.sh read CONTRACT METHOD [VALUES_JSON]    a read of a view function; prints its result
#   ibc.sh join CONTRACT MEMBER_URL MEMBER_ID SOURCE    ask a member to let you join a contract of the
#                                                source file SOURCE (the contract's .sol); waits for the outcome
#   ibc.sh guest CONTRACT MEMBER_URL MEMBER_ID SOURCE   join as a guest: no copy here; calls and reads go to the member
#   ibc.sh leave CONTRACT [MEMBER_ID]            leave a contract (or ask to remove a member)
#   ibc.sh public CONTRACT                       let anyone read your copy, unsigned
#   ibc.sh private CONTRACT                      stop it
#   ibc.sh anyone AGENT CONTRACT METHOD [VALUES_JSON]   read a public contract, unsigned, as anyone
#   ibc.sh contracts                             list your contracts
#   ibc.sh contract CONTRACT                     one contract's summary
#   ibc.sh stream [CONTRACT]                     follow your event stream (Ctrl-C to stop)
#
# Example:
#   ibc.sh newkey && ibc.sh register
#   C=$(ibc.sh deploy Counter '{"start": 10}')
#   ibc.sh write "$C" increment '{"amount": 5}'     # -> 15
#   ibc.sh read "$C" count                          # -> 15
set -euo pipefail

IBC_URL=${IBC_URL:-http://localhost:2611}
IBC_URL=${IBC_URL%/}
IBC_BIN=${IBC_BIN:-ibc}
CONTRACTS=${IBC_CONTRACTS:-$(cd "$(dirname "$0")/.." && pwd)/contracts}
TIMEOUT=${IBC_TIMEOUT:-60}

die() { echo "ibc.sh: $*" >&2; exit 1; }
need_key() {
    [ -n "${IBC_KEY:-}" ] || die "set IBC_KEY to your key file (create it with: ibc.sh newkey)"
    [ -f "$IBC_KEY" ] || die "no key file $IBC_KEY (create it with: ibc.sh newkey)"
    AGENT=$("$IBC_BIN" key address "$IBC_KEY")
}
node_key() { request GET "" node_info | jq -r .nodeKey; }

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

# sign KIND CONTRACT BODY_JSON: a transaction signed with the root key.
sign() {
    "$IBC_BIN" sign tx -key "$IBC_KEY" -kind "$1" -contract "$2" -body "$3"
}

# query ACTION CONTRACT [BODY_JSON] [TARGET]: a query signed with the root key.
query() {
    "$IBC_BIN" sign query -key "$IBC_KEY" -action "$1" -contract "$2" -body "${3:-}" -target "${4:-}"
}

# signed_get PATH ACTION CONTRACT: a GET whose signed query goes in the URL.
signed_get() {
    curl -sS -G "$IBC_URL/ibc/app/$1" --data-urlencode "action=$2" --data-urlencode "auth=$(query "$2" "$3")"
}

# stream_url SCOPE: the URL of your event stream, for one contract or all.
stream_url() {
    echo "$IBC_URL/stream?auth=$(query stream "$1" | jq -sRr @uri)"
}

# submit_and_wait SCOPE VERB PATH ACTION BODY: sends a request whose outcome
# arrives on the event stream, and prints that event.
submit_and_wait() {
    local scope=$1 verb=$2 path=$3 action=$4 body=$5
    local events
    events=$(mktemp)
    curl -sN "$(stream_url "$scope")" >"$events" &
    local stream=$!
    trap 'kill $stream 2>/dev/null; rm -f "$events"' RETURN
    for _ in $(seq 100); do [ -s "$events" ] && break; sleep 0.05; done
    [ -s "$events" ] || die "could not open the event stream at $IBC_URL"

    local ack
    ack=$(request "$verb" "$path" "$action" "$body")
    fail_on_message "$ack"
    ack=$(jq -r . <<<"$ack")
    [ "$ack" = "null" ] && die "nothing to do (already done?)"
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
[ -n "$cmd" ] || { sed -n '2,31p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
shift

case "$cmd" in
newkey)
    [ -n "${IBC_KEY:-}" ] || die "set IBC_KEY to the key file to create"
    mkdir -p "$(dirname "$IBC_KEY")"
    "$IBC_BIN" key new "$IBC_KEY"
    ;;
whoami)
    need_key
    echo "$AGENT"
    ;;
register)
    need_key
    body=$("$IBC_BIN" sign registration -key "$IBC_KEY" -node "$IBC_URL" -node-key "$(node_key)")
    submit_and_wait "" PUT "$AGENT" register_agent "$body" >/dev/null
    echo "registered $AGENT at $IBC_URL"
    ;;
exists)
    need_key
    request GET "$AGENT" is_exist_agent
    ;;
deploy)
    need_key
    name=${1:?usage: ibc.sh deploy NAME [CONSTRUCTOR_JSON]}
    ctor=${2:-{\}}
    [ -f "$CONTRACTS/$name.sol" ] || die "no $CONTRACTS/$name.sol"
    # The node compiles the source; the deploy carries both, so joiners can compile it again.
    compiled=$(request POST "" compile "$(jq -cn --rawfile src "$CONTRACTS/$name.sol" '{source: $src, contract: ""}')")
    fail_on_message "$compiled"
    body=$(jq -cn --arg name "$name" --arg node "$IBC_URL" --arg nodeKey "$(node_key)" \
        --rawfile src "$CONTRACTS/$name.sol" --argjson c "$compiled" --argjson ctor "$ctor" \
        '{name: $name, contract: ($name + ".sol"), default_app: "", profile: "",
          code: $c.code, abi: $c.abi, constructor: $ctor, node: $node, nodeKey: $nodeKey,
          source: $src, compiler: $c.compiler}')
    event=$(submit_and_wait "" PUT "$AGENT" deploy_contract "$(sign deploy "" "$body")")
    [ "$(jq -r .reply <<<"$event")" = "true" ] || die "deploy failed: $(jq -r .reply <<<"$event")"
    jq -r .contract <<<"$event"
    ;;
write)
    need_key
    contract=${1:?usage: ibc.sh write CONTRACT METHOD [VALUES_JSON]}
    method=${2:?usage: ibc.sh write CONTRACT METHOD [VALUES_JSON]}
    values=${3:-{\}}
    body=$(jq -cn --arg m "$method" --argjson v "$values" '{method: $m, values: $v}')
    event=$(submit_and_wait "$contract" POST "$AGENT/$contract/$method" contract_write "$(sign call "$contract" "$body")")
    jq -c .reply <<<"$event"
    ;;
read)
    need_key
    contract=${1:?usage: ibc.sh read CONTRACT METHOD [VALUES_JSON]}
    method=${2:?usage: ibc.sh read CONTRACT METHOD [VALUES_JSON]}
    values=${3:-{\}}
    body=$(jq -cn --arg m "$method" --argjson v "$values" '{method: $m, values: $v}')
    reply=$(request POST "$AGENT/$contract/$method" contract_read "$(query contract_read "$contract" "$body")")
    fail_on_message "$reply"
    jq -c . <<<"$reply"
    ;;
join | guest)
    need_key
    usage="usage: ibc.sh $cmd CONTRACT MEMBER_URL MEMBER_ID SOURCE"
    contract=${1:?$usage}
    member_url=${2:?$usage}
    member=${3:?$usage}
    source=${4:?$usage}
    [ -f "$source" ] || die "no source file $source"
    join_body=$(jq -cn --arg node "$IBC_URL" --arg nodeKey "$(node_key)" --arg hash "$("$IBC_BIN" source-hash "$source")" \
        '{node: $node, nodeKey: $nodeKey, profile: "", sourceHash: $hash}')
    if [ "$cmd" = guest ]; then
        join_body=$(jq -c --arg host "$member" '. + {guest: true, host: $host}' <<<"$join_body")
    fi
    tx=$(sign join "$contract" "$join_body")
    body=$(jq -c --arg url "$member_url" --arg agent "$member" '. + {partner: {address: $url, agent: $agent}}' <<<"$tx")
    event=$(submit_and_wait "" PUT "$AGENT" join_contract "$body")
    [ "$(jq -r .reply <<<"$event")" = "true" ] || die "not joined: $(jq -r '.reason // "refused"' <<<"$event")"
    case $(jq -r '.source_check // ""' <<<"$event") in
    verified) echo "joined $contract (its source compiles to its code)" ;;
    *) echo "joined $contract, but its source was not checked: $(jq -r '.reason // ""' <<<"$event")" ;;
    esac
    ;;
leave)
    need_key
    contract=${1:?usage: ibc.sh leave CONTRACT [MEMBER_ID]}
    member=${2:-$AGENT}
    tx=$(sign leave "$contract" "$(jq -cn --arg m "$member" '{member: $m}')")
    event=$(submit_and_wait "$contract" PUT "$AGENT" leave_contract "$tx")
    jq -c .reply <<<"$event"
    ;;
public | private)
    need_key
    contract=${1:?usage: ibc.sh $cmd CONTRACT}
    flag=$([ "$cmd" = public ] && echo true || echo false)
    reply=$(request PUT "$AGENT/$contract" set_public "$("$IBC_BIN" sign publish -key "$IBC_KEY" -contract "$contract" -public="$flag")")
    fail_on_message "$reply"
    jq -c . <<<"$reply"
    ;;
anyone)
    agent=${1:?usage: ibc.sh anyone AGENT CONTRACT METHOD [VALUES_JSON]}
    contract=${2:?usage: ibc.sh anyone AGENT CONTRACT METHOD [VALUES_JSON]}
    method=${3:?usage: ibc.sh anyone AGENT CONTRACT METHOD [VALUES_JSON]}
    values=${4:-{\}}
    reply=$(curl -sS -G "$IBC_URL/ibc/app/$agent/$contract/$method" --data-urlencode action=public_read \
        --data-urlencode "values=$values")
    fail_on_message "$reply"
    jq -c . <<<"$reply"
    ;;
contracts)
    need_key
    signed_get "$AGENT" get_contracts "" | jq '[.[] | {id, name, contract, role, public, evm_address, block_number}]'
    ;;
contract)
    need_key
    contract=${1:?usage: ibc.sh contract CONTRACT}
    signed_get "$AGENT/$contract" get_contract "$contract" | jq 'del(.code, .abi)'
    ;;
stream)
    need_key
    curl -sN "$(stream_url "${1:-}")" | sed -un 's/^data: \(..*\)/\1/p'
    ;;
*)
    die "unknown command $cmd (run without arguments for help)"
    ;;
esac
