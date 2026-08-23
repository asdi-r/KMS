#!/bin/sh
# Auth & activation-token tests. Usage: test-auth.sh <base-url> <apikey> <admin-pass>
U=$1; K=$2; AP=$3
J="-H content-type:application/json"
code() { curl -s -o /dev/null -w "%{http_code}" "$@"; }
f() { echo "$2" | sed -n "s/.*\"$1\":\"\{0,1\}\([^,}\"]*\).*/\1/p" | head -1; }
echo "## login"
echo "   wrong password -> $(code $J -X POST $U/auth/login -d '{"username":"admin","password":"nope"}') (expect 401)"
L=$(curl -s $J -X POST $U/auth/login -d "{\"username\":\"admin\",\"password\":\"$AP\"}"); T=$(f token "$L")
echo "   admin login -> token len=${#T} role=$(f role "$L")"
echo "   no auth: GET /purchases -> $(code "$U/purchases?customer_id=x") (401); X-API-Key -> $(code -H "X-API-Key: $K" "$U/purchases?customer_id=x") (200); bad key -> $(code -H "X-API-Key: bad" "$U/purchases?customer_id=x") (401)"
echo "## users & roles"
echo "   create viewer -> $(code $J -H "Authorization: Bearer $T" -X POST $U/users -d '{"username":"viewer1","password":"viewerpass123","role":"viewer"}') (201)"
echo "   duplicate     -> $(code $J -H "Authorization: Bearer $T" -X POST $U/users -d '{"username":"viewer1","password":"viewerpass123"}') (409)"
V=$(f token "$(curl -s $J -X POST $U/auth/login -d '{"username":"viewer1","password":"viewerpass123"}')")
P=$(curl -s $J -H "Authorization: Bearer $T" -X POST $U/purchase -d '{"customer_id":"PT-AUTH","product":"AV","quantity":2}'); PID=$(f id "$P"); KEY=$(echo "$P" | sed -n 's/.*"key":"\([A-Z0-9-]*\)".*/\1/p')
echo "   viewer GET /purchases/$PID -> $(code -H "Authorization: Bearer $V" $U/purchases/$PID) (200); viewer POST /purchase -> $(code $J -H "Authorization: Bearer $V" -X POST $U/purchase -d '{"customer_id":"x","product":"y"}') (403); viewer GET /users -> $(code -H "Authorization: Bearer $V" $U/users) (403)"
echo "   demote last admin -> $(code $J -H "Authorization: Bearer $T" -X PATCH $U/users/1 -d '{"role":"viewer"}') (409)"
echo "   disable viewer  -> $(code $J -H "Authorization: Bearer $T" -X PATCH $U/users/2 -d '{"status":"disabled"}') (200); viewer login now -> $(code $J -X POST $U/auth/login -d '{"username":"viewer1","password":"viewerpass123"}') (401)"
echo "   bad token -> $(code -H "Authorization: Bearer abc.def.ghi" $U/auth/me) (401)"
echo "## lockout: 5 bad logins then correct"
for i in 1 2 3 4 5; do curl -s -o /dev/null $J -X POST $U/auth/login -d '{"username":"locked","password":"x"}'; done
echo "   6th attempt -> $(code $J -X POST $U/auth/login -d '{"username":"locked","password":"x"}') (429)"
echo "## activation token"
A=$(curl -s $J -X POST $U/activate -d "{\"key\":\"$KEY\",\"device_id\":\"d1\"}"); TOK=$(f activation_token "$A")
echo "   activate d1 -> token len=${#TOK}"
echo "   deactivate without token -> $(code $J -X POST $U/deactivate -d "{\"key\":\"$KEY\",\"device_id\":\"d1\"}") (400)"
echo "   deactivate wrong token   -> $(code $J -X POST $U/deactivate -d "{\"key\":\"$KEY\",\"device_id\":\"d1\",\"activation_token\":\"bad\"}") (403)"
echo "   deactivate right token   -> $(code $J -X POST $U/deactivate -d "{\"key\":\"$KEY\",\"device_id\":\"d1\",\"activation_token\":\"$TOK\"}") (200)"
curl -s -o /dev/null $J -X POST $U/activate -d "{\"key\":\"$KEY\",\"device_id\":\"d2\"}"
echo "   admin release d2 (JWT) -> $(code -H "Authorization: Bearer $T" -X DELETE $U/keys/$KEY/activations/d2) (200); again -> $(code -H "Authorization: Bearer $T" -X DELETE $U/keys/$KEY/activations/d2) (404)"
echo "## audit actors"
curl -s -H "Authorization: Bearer $T" $U/purchases/$PID/events | grep -o '"action":"[a-z_]*","actor":"[^"]*"' | sort | uniq -c
echo "## change password"
echo "   wrong current -> $(code $J -H "Authorization: Bearer $T" -X POST $U/auth/password -d '{"current_password":"x","new_password":"newpassword123"}') (401)"
