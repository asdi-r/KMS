#!/bin/sh
# PT MUF scenario (seat model). Usage: test-muf.sh <base-url> <apikey> "<psql cmd>"
U=$1; K=$2; PSQL=$3
H="-H content-type:application/json -H X-API-Key:$K"
j() { curl -s $H "$@"; }
code() { curl -s -o /dev/null -w "%{http_code}" $H "$@"; }
f() { echo "$2" | sed -n "s/.*\"$1\":\([^,}]*\).*/\1/p" | head -1; }
pub() { curl -s -H content-type:application/json -X POST "$@"; }
simulate() { $PSQL -c "update purchases set expires_at=now()+interval '$1' where id=$PID; update license_keys set expires_at=now()+interval '$1' where purchase_id=$PID" >/dev/null; }
act() { curl -s -o /dev/null -w "%{http_code}" -H content-type:application/json -X POST $U/activate -d "{\"key\":\"$KEY\",\"device_id\":\"$1\",\"hostname\":\"pc-$1\"}"; }

echo "## S1. Generate PT MUF: 1 tahun, kuota 100 endpoint antivirus -> 1 key"
P=$(j -X POST $U/purchase -d '{"customer_id":"PT-MUF","product":"ANTIVIRUS","quantity":100,"term_years":1}')
PID=$(f id "$P"); KEY=$(echo "$P" | sed -n 's/.*"key":"\([A-Z0-9-]*\)".*/\1/p')
echo "   id=$PID key=$KEY seats=$(f seats "$P") used=$(f used_seats "$P") expires=$(f expires_at "$P" | head -c 11)"

echo "## Seat enforcement: aktivasi 100 endpoint"
ok=0; for i in $(seq 1 100); do [ "$(act dev-$i)" = 200 ] && ok=$((ok+1)); done
echo "   activated=$ok/100"
echo "   endpoint ke-101 -> $(act dev-101) (expect 409)  $(pub $U/activate -d "{\"key\":\"$KEY\",\"device_id\":\"dev-101\"}")"
echo "   re-activate dev-1 (idempotent) -> $(act dev-1) (expect 200)"
echo "   validate dev-50 -> valid=$(f valid "$(pub $U/validate -d "{\"key\":\"$KEY\",\"device_id\":\"dev-50\"}")")  dev-101 -> $(f reason "$(pub $U/validate -d "{\"key\":\"$KEY\",\"device_id\":\"dev-101\"}")")"
echo "   deactivate dev-1 -> $(code -X DELETE $U/keys/$KEY/activations/dev-1); lalu dev-101 -> $(act dev-101) (expect 200)"
echo "   key status: $(j $U/keys/$KEY | sed -n 's/.*"seats":\([0-9]*\),"used_seats":\([0-9]*\).*/seats=\1 used=\2/p')"
echo "   concurrency: 20 aktivasi paralel ke 1 seat tersisa (setelah deactivate dev-2)"
code -X DELETE $U/keys/$KEY/activations/dev-2 >/dev/null
for i in $(seq 1 20); do act par-$i & done | sort | uniq -c | tr '\n' ' '; wait; echo
echo "   used after race: $(f used_seats "$(j $U/keys/$KEY)") (expect 100)"

echo "## S4. Perpanjang 2 bulan sebelum habis"
echo "   sisa 90 hari -> $(code -X POST $U/purchases/$PID/renew -d '{"term_years":1}') (expect 409)"
simulate '59 days'; echo "   sisa 59 hari -> renewable=$(f renewable "$(j $U/purchases/$PID)")"

echo "## S2. Perpanjang SETELAH expired, kuota tetap 100"
simulate '-5 days'
echo "   activate saat expired -> $(act dev-new) (expect 403); validate -> $(f reason "$(pub $U/validate -d "{\"key\":\"$KEY\"}")")"
R=$(j -X POST $U/purchases/$PID/renew -d '{"term_years":1}')
echo "   renew -> quantity=$(f quantity "$R") seats=$(f seats "$R") used=$(f used_seats "$R") expires=$(f expires_at "$R" | head -c 11)"
echo "   validate dev-50 setelah renew -> valid=$(f valid "$(pub $U/validate -d "{\"key\":\"$KEY\",\"device_id\":\"dev-50\"}")")"

echo "## S3. Perpanjang SETELAH expired + tambah kuota 100 -> 200"
simulate '-1 day'
R=$(j -X POST $U/purchases/$PID/renew -d '{"term_years":1,"add_quantity":100}')
echo "   renew+add -> quantity=$(f quantity "$R") seats=$(f seats "$R") used=$(f used_seats "$R") (expect 200,200,100)"
echo "   activate dev-150 -> $(act dev-150) (expect 200)"

echo "## Tambah kuota tanpa renew (+50) & reissue"
R=$(j -X POST $U/purchases/$PID/keys -d '{"quantity":50}'); echo "   seats=$(f seats "$R") (expect 250)"
R=$(j -X POST $U/keys/$KEY/reissue); NEW=$(echo "$R" | sed -n 's/.*"key":{[^}]*"key":"\([A-Z0-9-]*\)".*/\1/p')
echo "   reissue -> new=$NEW used carried=$(f used_seats "$R") (expect 101); old key validate -> $(f reason "$(pub $U/validate -d "{\"key\":\"$KEY\"}")")"
KEY=$NEW
echo "   validate dev-50 dengan key baru -> valid=$(f valid "$(pub $U/validate -d "{\"key\":\"$KEY\",\"device_id\":\"dev-50\"}")")"

echo "## S5. Perpanjang fleksibel: 1, 7, 36 bulan; -1 ditolak"
for m in 1 7 36; do simulate '10 days'; R=$(j -X POST $U/purchases/$PID/renew -d "{\"term_months\":$m}"); echo "   +$m bulan -> term_months=$(f term_months "$R") expires=$(f expires_at "$R" | head -c 11)"; done
echo "   term_months=-1 -> $(code -X POST $U/purchases/$PID/renew -d '{"term_months":-1}') (expect 400)"
echo "## audit"; j $U/purchases/$PID/events | grep -o '"action":"[a-z_]*"' | sort | uniq -c | tr '\n' ' '; echo
