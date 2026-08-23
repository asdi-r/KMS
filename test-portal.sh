#!/bin/sh
# Global dashboard + customer portal tests. Usage: test-portal.sh <base-url> <apikey>
U=$1; K=$2; J="-H content-type:application/json"; A="-H X-API-Key:$K"
code() { curl -s -o /dev/null -w "%{http_code}" "$@"; }
f() { echo "$2" | sed -n "s/.*\"$1\":\"\{0,1\}\([^,}\"]*\).*/\1/p" | head -1; }
P=$(curl -s $J $A -X POST $U/purchase -d '{"customer_id":"PT-PORTAL","product":"AV","quantity":3}'); KEY=$(echo "$P" | sed -n 's/.*"key":"\([A-Z0-9-]*\)".*/\1/p')
for d in pc-1 pc-2; do curl -s -o /dev/null $J -X POST $U/activate -d "{\"key\":\"$KEY\",\"device_id\":\"$d\",\"hostname\":\"host-$d\"}"; done
echo "## global list & stats"
S=$(curl -s $A $U/stats); echo "   stats: contracts=$(f contracts "$S") customers=$(f customers "$S") seats=$(f seats "$S") used=$(f used_seats "$S") expiring=$(f expiring_soon "$S") activations_today=$(f activations_today "$S")"
L=$(curl -s $A "$U/purchases?limit=2"); echo "   list all limit=2 -> total=$(f total "$L") rows=$(echo "$L" | grep -o '"customer_id"' | wc -l | tr -d ' ')"
echo "   search q=PORTAL -> $(curl -s $A "$U/purchases?q=portal" | grep -o '"customer_id":"[^"]*"' | sort -u | tr '\n' ' ')"
echo "   search by key -> total=$(f total "$(curl -s $A "$U/purchases?q=$KEY")") (expect 1)"
echo "   status=expired -> $(code $A "$U/purchases?status=expired"); viewer-less (no auth) -> $(code "$U/stats") (401)"
echo "## customer portal"
echo "   login wrong customer -> $(code $J -X POST $U/portal/login -d "{\"customer_id\":\"PT-OTHER\",\"key\":\"$KEY\"}") (401)"
L=$(curl -s $J -X POST $U/portal/login -d "{\"customer_id\":\"pt-portal\",\"key\":\"$KEY\"}"); T=$(f token "$L"); echo "   login ok (case-insensitive customer) -> token len=${#T}"
M=$(curl -s -H "Authorization: Bearer $T" $U/portal/me); echo "   /portal/me -> customer=$(f customer_id "$M") used=$(f used_seats "$M") devices=$(echo "$M" | grep -o '"device_id"' | wc -l | tr -d ' ')"
echo "   customer token on admin route /purchases -> $(code -H "Authorization: Bearer $T" "$U/purchases") (403); /users -> $(code -H "Authorization: Bearer $T" $U/users) (403)"
echo "   release pc-1 -> $(code -H "Authorization: Bearer $T" -X DELETE $U/portal/activations/pc-1) (200); again -> $(code -H "Authorization: Bearer $T" -X DELETE $U/portal/activations/pc-1) (404)"
echo "   used now=$(f used_seats "$(curl -s -H "Authorization: Bearer $T" $U/portal/me)") (expect 1)"
# another customer's key must not be reachable: create second contract, try release via first token
P2=$(curl -s $J $A -X POST $U/purchase -d '{"customer_id":"PT-OTHER","product":"AV","quantity":1}'); KEY2=$(echo "$P2" | sed -n 's/.*"key":"\([A-Z0-9-]*\)".*/\1/p')
curl -s -o /dev/null $J -X POST $U/activate -d "{\"key\":\"$KEY2\",\"device_id\":\"x-1\"}"
echo "   release other customer's device x-1 via PT-PORTAL token -> $(code -H "Authorization: Bearer $T" -X DELETE $U/portal/activations/x-1) (404 — scoped to own key)"
echo "   events actor: $(curl -s -H "Authorization: Bearer $T" $U/portal/events | grep -o '"actor":"customer:[^"]*"' | sort -u)"
