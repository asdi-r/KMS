# Skenario Testing KMS (Postman) — model kuota seat

**Base URL:** `https://kms.116.212.74.168.sslip.io`
**Auth endpoint admin:** `Authorization: Bearer <JWT>` (dari `POST /auth/login` `{"username","password"}`) **atau** `X-API-Key: <SERVICE_PASSWORD_KMSAPIKEY>` untuk integrasi mesin. Collection ini memakai `X-API-Key` (variabel `apikey`). Role `viewer` hanya GET; `admin` semua.
**Deactivate publik** kini wajib `activation_token` yang dikembalikan `POST /activate`; admin melepas seat via `DELETE /keys/{key}/activations/{device_id}`.
**Endpoint publik (dipanggil central antivirus, tanpa apikey, rate-limit 600/menit):** `POST /activate`, `POST /deactivate`, `POST /validate`, `GET /health`

## Model lisensi
- 1 kontrak (purchase) = **1 license key** dengan kuota `seats` = `quantity` (jumlah endpoint).
- Central antivirus memanggil `POST /activate` dengan `device_id` unik per endpoint → seat terpakai.
  Aktivasi melebihi kuota → **409 `seat_limit_reached`**. Aktivasi ulang device yang sama tidak memakan seat baru.
- `POST /deactivate` melepas seat (PC diganti/dihapus). `POST /validate` + `device_id` = check-in rutin endpoint.
- Perpanjangan memperpanjang key & menjaga aktivasi; tambah kuantitas menaikkan `seats`; re-issue mengganti string key dan **memindahkan aktivasi** ke key baru.

## Cara pakai
1. Postman → Import `KMS.postman_collection.json`.
2. Collection → **Variables** → isi `apikey`.
3. **Run collection** berurutan (folder 00 → 05). `purchaseId`, `licenseKey`, `oldKey` terisi otomatis. TC-22 berulang otomatis 99× (loop `setNextRequest`).
4. Langkah bertanda **[SIMULASI]** perlu SQL dulu (tidak mungkin menunggu 1 tahun):

```bash
ssh nevacloud "docker exec -i postgres-kcwlic3chficcr1q52jxy83o psql -U kms -d kms -c \"update purchases set expires_at=now()+interval '<INTERVAL>' where id=<purchaseId>; update license_keys set expires_at=now()+interval '<INTERVAL>' where purchase_id=<purchaseId>;\""
```
`<INTERVAL>`: `59 days` (TC-41) · `-5 days` (TC-42/43) · `-1 day` (TC-45) · `10 days` (TC-48, 49, 4A — ulangi sebelum tiap request).
Catatan: setelah SQL simulasi, jalankan dulu `POST /validate` dengan key agar cache Redis (TTL 10 menit) tersegarkan, atau tunggu ≤10 menit — alur nyata (renew/revoke) meng-invalidate cache otomatis.

---

## 00 — Health & Auth
| TC | Request | Ekspektasi |
|---|---|---|
| 00 | `GET /health` | 200 |
| 01 | `POST /purchase` tanpa apikey | **401** |
| 02 | `POST /purchase` apikey salah | **401** |

## 01 — Skenario 1: Generate key PT MUF, 1 tahun, kuota 100 endpoint antivirus
| TC | Request | Payload | Ekspektasi |
|---|---|---|---|
| 10 | `POST /purchase` | `{"customer_id":"PT-MUF","product":"ANTIVIRUS","quantity":100,"term_years":1}` | **201**; **1 key**; `key.seats=100`, `used_seats=0`; `purchase.quantity=100`, `term_months=12`, `expires_at` ≈ +1 tahun |
| 11 | `POST /purchase` | `term_months: 7` atau `term_years: 5` | 201 (term fleksibel: bulan 1–600 atau tahun ≥1) |
| 12 | `POST /purchase` | tanpa `customer_id` | 400 |
| 13 | `POST /purchase` | `quantity: 1001` | 400 (maks 1000) |

Response TC-10:
```json
{ "purchase": { "id": 9, "customer_id": "PT-MUF", "product": "ANTIVIRUS", "quantity": 100, "term_years": 1, "term_months": 12, "expires_at": "2027-08-23T..." },
  "key": { "key": "UP9GF-B4FSM-KMMM8-TGVAZ-Y6BZV", "status": "active", "seats": 100, "used_seats": 0, "expires_at": "2027-08-23T..." } }
```

## 02 — Aktivasi endpoint & penegakan kuota (lisensi hanya untuk 100 client)
| TC | Request | Payload | Ekspektasi |
|---|---|---|---|
| 20 | `POST /activate` | `{"key":"{{licenseKey}}","device_id":"dev-001","hostname":"PC-FINANCE-01"}` | 200 `activated=true`, `used_seats=1`, `remaining_seats=99` |
| 21 | `POST /activate` dev-001 lagi | sama | 200, `used_seats` tetap 1 (idempotent) |
| 22 | `POST /activate` dev-002 … dev-100 (loop) | `device_id` berbeda | semua 200, `used_seats` naik sampai 100 |
| 23 | `POST /activate` endpoint ke-101 | `{"key":"…","device_id":"dev-101"}` | **409** `reason="seat_limit_reached"`, `used_seats=100`, `remaining_seats=0` |
| 24 | `POST /validate` | `{"key":"…","device_id":"dev-050"}` | `valid=true`, `device_active=true` |
| 25 | `POST /validate` | `{"key":"…","device_id":"dev-101"}` | `valid=false`, `reason="device_not_activated"` |
| 26 | `POST /deactivate` | `{"key":"…","device_id":"dev-001","activation_token":"{{token001}}"}` (token dari TC-20; tanpa token → 400, token salah → 403) | 200, `remaining_seats=1` |
| 27 | `POST /activate` dev-101 | – | 200 (seat kosong terpakai), `used_seats=100` |
| 28 | `POST /deactivate` dev-999 | – | 404 `device_not_activated` |
| 29 | `GET /keys/{{licenseKey}}/activations` (admin) | – | 200; 100 aktivasi aktif (`?include=all` untuk yang sudah dilepas) |

Uji concurrency (sudah dijalankan di server): 20 aktivasi paralel memperebutkan 1 seat → tepat 1 berhasil, 19 → 409. Tidak ada oversubscription (row lock pada key).

## 03 — Skenario re-issuance
| TC | Request | Ekspektasi |
|---|---|---|
| 30 | `GET /purchases/{{purchaseId}}` | 1 key aktif, seats 100 / used 100, `renewable=false` |
| 31 | `GET /purchases?customer_id=PT-MUF` | daftar kontrak |
| 32 | `GET /keys/{{licenseKey}}` | key tersimpan (ambil ulang) |
| 33 | `POST /keys/{{licenseKey}}/reissue` | **201**; key baru; `seats=100`, `used_seats=100` (aktivasi ikut pindah — endpoint tidak perlu aktivasi ulang) |
| 34 | `POST /validate` key lama | `reason="reissued"` |
| 35 | `POST /validate` key baru + dev-050 | `valid=true` |
| 36 | reissue key lama lagi | 409 |

## 04 — Skenario perpanjangan (2, 3, 4, 5)
| Skenario | TC | Request | Payload | Ekspektasi |
|---|---|---|---|---|
| **4** Perpanjang 2 bulan sebelum habis | 40 | renew saat sisa >60 hari | `{"term_years":1}` | **409** `too early`, `renewable_after` = expired − 60 hari |
| | 41 | [SIMULASI 59 days] `GET /purchases/{id}` | – | `renewable=true` |
| **2** Perpanjang setelah expired, kuota tetap 100 | 42 | [SIMULASI −5 days] `POST /activate` dev-expired | – | **403** `reason="expired"` |
| | 43 | `POST /purchases/{id}/renew` | `{"term_years":1}` | 200; `quantity=100`, `seats=100`, `used_seats=100` (aktivasi terjaga); expired = hari ini + 1 thn |
| | 44 | `POST /validate` dev-050 | – | `valid=true` |
| **3** Perpanjang setelah expired + 100 → 200 | 45 | [SIMULASI −1 day] renew | `{"term_years":1,"add_quantity":100}` | 200; `quantity=200`, `seats=200`, `used_seats=100`, `seats_added=100` |
| | 46 | `POST /activate` dev-150 | – | 200, `remaining_seats=99` |
| Tambah kuota tanpa renew | 47 | `POST /purchases/{id}/keys` | `{"quantity":50}` | 200; `seats=250` |
| **5** Perpanjang fleksibel ≥1 bulan, tanpa batas | 48 | [SIMULASI 10 days] renew | `{"term_months":1}` | 200; `term_months=1`; expired = lama + 1 bulan |
| | 49 | [SIMULASI 10 days] renew | `{"term_months":7}` | 200 |
| | 4A | [SIMULASI 10 days] renew | `{"term_months":36}` | 200; `term_months=36`, `term_years=3` |
| | 4B | renew | `{"term_months":-1}` | 400 |

Payload renew: `{"term_months": N}` (≥1, tanpa batas) · `{"term_years": N}` (= N×12) · `{}` (default 12) · tambah `"add_quantity": N` untuk menaikkan kuota sekaligus.

## 05 — Validasi, revoke, audit
| TC | Request | Ekspektasi |
|---|---|---|
| 50 | `POST /validate` `{"key"}` tanpa device | `valid=true` + `seats`, `used_seats`, `remaining_seats` |
| 51 | `POST /validate` key palsu | `reason="not_found"` |
| 52 | `POST /activate` key palsu | 404 |
| 53 | `POST /activate` tanpa `device_id` | 400 |
| 54 | `GET /purchases/{id}/events` | ada `issued`, `activated`, `deactivated`, `reissued`, `renewed`, `seats_added` |
| 55 | `DELETE /keys/{{licenseKey}}` | 200 `revoked` |
| 56 | `POST /activate` setelah revoke | **403** `reason="revoked"` |
| 57 | `POST /validate` dev-050 setelah revoke | `reason="revoked"` |

## Kode status
200 OK · 201 Created · 400 payload salah · 401 apikey · 403 key tidak aktif/expired (activate) · 404 tidak ditemukan · 409 kuota penuh / terlalu awal renew / key tidak aktif · 429 rate limit
