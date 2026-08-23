#!/bin/sh
U=$1; K=$2; J="-H content-type:application/json"; A="-H X-API-Key:$K"
code() { curl -s -o /dev/null -w "%{http_code}" "$@"; }
f() { echo "$2" | sed -n "s/.*\"$1\":\"\{0,1\}\([^,}\"]*\).*/\1/p" | head -1; }
echo "## term flexibility"
for body in '"term_months":1' '"term_months":7' '"term_months":12' '"term_years":1' '"term_years":3' '"term_years":5' '"term_years":20' ''; do
  P=$(curl -s $J $A -X POST $U/purchase -d "{\"customer_id\":\"PT-TERM\",\"product\":\"AV\",\"quantity\":1${body:+,$body}}")
  echo "   {$body} -> term_months=$(f term_months "$P") term_years=$(f term_years "$P") expires=$(f expires_at "$P" | cut -c1-10)"
done
echo "   term_months=0 & term_years=0 -> $(code $J $A -X POST $U/purchase -d '{"customer_id":"PT-TERM","product":"AV","term_months":-1}') (400)"
echo "   term_months=601 -> $(code $J $A -X POST $U/purchase -d '{"customer_id":"PT-TERM","product":"AV","term_months":601}') (400 cap)"
echo "## customers autocomplete"
for c in PT-MUF PT-MAKMUR PT-MANDIRI CV-ABC; do curl -s -o /dev/null $J $A -X POST $U/purchase -d "{\"customer_id\":\"$c\",\"product\":\"AV\"}"; done
echo "   q=pt-m -> $(curl -s $A "$U/customers?q=pt-m")"
echo "   q=abc  -> $(curl -s $A "$U/customers?q=abc")"
echo "   q=     -> $(curl -s $A "$U/customers?q=")"
echo "   no auth -> $(code "$U/customers?q=pt") (401)"
