# 🚀 LLM Yönlendirici (Unified AI Gateway & Quota Router)

**Zen Kostüm**, **Gömülü Proxy Yönlendirici** ve **EVREN Token & Kota Router** sistemlerinin tek bir hafif ve yüksek performanslı Go sürecinde birleştirilmiş paketidir. Harici hiçbir bağımlılık (zero third-party dependency) gerektirmez; tek bir `baslat.bat` veya `llm-yonlendirici.exe` çalıştırmasıyla tüm katmanlar ayağa kalkar.

---

## 🌟 Birleşik Bileşenler & Mimari

| Bileşen | Dinleme Portu | Görev & Özellikler | Web Arayüzü |
| :--- | :--- | :--- | :--- |
| **🎭 Zen Kostüm** | `127.0.0.1:8110` | OpenCode Zen free-tier kıyafet protokolü, oturum maskeleme, stream filtreleme, model kataloğu | [`/ui`](http://127.0.0.1:8110/ui) |
| **🌐 Proxy Havuzu** | `127.0.0.1:8110` | Gömülü SOCKS5/HTTP/Direct havuzu. Zen upstream istekleri bu havuzdan çıkar, IP ban ve rate-limitleri baypas eder. | [`/ui/proxy`](http://127.0.0.1:8110/ui/proxy) |
| **⚡ EVREN Token Router** | `127.0.0.1:8088` & `8110` | EVREN API anahtar rotasyonu, %3 cache indirim formülü, 10M günlük kota, 03:00 TSİ sıfırlama, embedding ayrımı | [`/ui/evren`](http://127.0.0.1:8110/ui/evren) <br> [`/dashboard`](http://127.0.0.1:8088/dashboard) |

---

## 🎯 Entegrasyon & Base URL Kullanımı

İstemcilerinizde (Cursor, DSH, Cline, Continue, Chatbox vb.) aşağıdaki adresleri doğrudan kullanabilirsiniz:

### 1. Zen Kostüm (Ücretsiz Zen Modelleri)
- **Base URL:** `http://127.0.0.1:8110/v1`
- **Modeller:** `mimo-v2.6-flash-free`, `space-bunny-free` vb. (Dinamik katalog: `/v1/models`)
- **API Key:** `Bearer public` veya herhangi bir metin.

### 2. EVREN LLM API (Sabit HTTP Protokolü)
- **Sabit HTTP Base URL:** `http://127.0.0.1:8088/v1`
- **Alternatif (8110 Portundan):** `http://127.0.0.1:8110/evren/v1`
- **Desteklenen Modeller:**
  - `mimo-v2.6-pro`
  - `qwen3.8-flash-next`
  - `deepseek-v4.1-flash`
  - `qwen3-embedding-8b` (`/v1/embeddings`)

---

## 🧮 EVREN Net Token Hesaplama Mantığı

EVREN sisteminin uyguladığı kesin matematiksel kural:
$$\text{Net Harcanan} = (\text{Girdi} - \text{Önbellek}) + \text{int64}(\text{round}(\text{Önbellek} \times 0.03)) + \text{Çıktı}$$

- **Önbellek İndirimi:** Önbellekten okunan tokenların sadece **%3**'ü kotadan düşer (%97 tasarruf).
- **Günlük Limit:** Anahtar başına 10.000.000 (10 Milyon) net token.
- **Sıfırlama Saati:** Her gece **03:00 TSİ** (Türkiye Saati / UTC+3).
- **Embedding Ayrımı:** Embedding tokenları 10M chat kotasından düşmez; bağımsız takip edilir.

---

## 🧠 Protokol Uyumlu İstek Loglama & Transcript (YENİ)

Her chat isteği **giden `messages` bağlamı + dönen model yanıtı + hata detaylarıyla** birlikte, her kayıt **mutlak zaman damgalı** olarak bellekteki ring tamponunda tutulur (son **50 oturum / 1000 istek**; RAM patlaması imkânsız). Kayıtlar OpenAI Chat Completions mesaj şeması + OpenTelemetry GenAI alan adlarıyla JSON dökülür; yapay zekâ okuyucular tek istekle tam sohbet transkriptini okur. Arayüzde **oturum pill'leri** ile tek tıkla oturum geçmişi, istek günlüğünde **📋 detay** ile o isteğin tam HTTP gövdesi + yanıt gövdesi açılır.

| Endpoint | Adres (8110) | Adres (8088) | Görev |
| :--- | :--- | :--- | :--- |
| **Transcript Dökümü** | `GET http://127.0.0.1:8110/api/evren/logs/transcript` | `GET http://127.0.0.1:8088/api/logs/transcript` | Tüm konuşmaları (messages + yanıt + usage + hata) JSON döker |
| **Oto Geçiş (GET/POST)** | `GET/POST http://127.0.0.1:8110/api/evren/auto-switch` | `.../api/auto-switch` | `{"enabled":true\|false}` — kapalıyken apiler arası geçiş **yasak** |
| **Kart Üyeliği** | `POST /api/evren/keys/<id>/auto-switch` | `POST /api/keys/<id>/auto-switch` | `{"enabled":bool}` — kartı oto geçiş havuzuna al/çıkar |

Aynı konuşma zinciri `messages` ön eki üzerinden otomatik bağlanır; `conversation_id`/`session_id` verilirse oturum id'si esas alınır. Arayüzdeki **"🧠 Konuşma Transkripti"** paneli toplam istek sayısını ve **son istekten beri geçen süreyi saniyede bir canlı** gösterir.

**Hata yakalama garantileri:** upstream `HTTP 200` dışı her yanıt konsola `[UPSTREAM HATA]`, `RequestLog.error_message`a HTTP kodu + **ham gövdeyle birebir** yazılır ve istemciye **aynen** iletilir. SSE'de `error` nesneleri `[STREAM HATA]` olur; akış `[DONE]`/`finish_reason` gelmeden kopsa istemci patlamaz — kontrollü `stream_interrupted` hata chunk'ı + `[DONE]` gönderilir. Upstream zaman aşımı **600 sn (10 dk)**.

## 🔁 Oto Geçiş (API Anahtarı Rotasyonu)

- **Tetik 1 — geçiş eşiği (marj) aşımı:** Bir kartın günlük kullanımı `Limit − Geçiş Eşiği (Marj)` sınırına ulaştığında geçiş olur; aday **yalnızca oto geçiş havuzundaki** sıradaki müsait karttır.
- **Tetik 2 — günlük kota 429:** Upstream `code=daily_token_limit_exceeded` taşıyan **yalnızca** 429 yanıtı yakalanınca sıradaki havuz kartına geçilir ve istek 1 kez yeni kartla yinelendi. Hedef kart `IsExhausted` işaretlenir (günlük sıfırmaya kadar havuz dışı). **Dakikalık `rate_limit` 429'lar geçiş yapmaz**, gövde birebir iletilir.
- **Başka hiçbir olay geçiş tetiklemez:** `GetActiveKey` salt-okunurdur; günlük 03:00 sıfırlaması, kart silme ve açılış aktif anahtarı asla kendiliğinden değiştirmez.
- **`🔁 OTO GEÇİŞ` düğmesi** (EVREN paneli): **KAPALıyken** her iki tetik de çalışır, aynı anahtarla devam edilir.
- **Açıkken** geçiş yalnızca **havuza seçili kartlar** arasında olur: havuz üyeliği kart üzerinde **OTO GEÇİŞ pill'i** + dinamik **`#N` sırası rozeti** ile görünür; **ince turuncu çevre şeridi** (`#f59e0b`, 2px — tüm çerçeveyi kaplar) artık **DOLU kartı** (üzerinde oturum bağlı, `bound_sessions > 0`) ifade eder; **aktif kart cırtlak yeşil** (`#39FF14`) "nefes" animasyonlu çerçeve taşır ve şerit göstermez (tek renk).
- Eski `keys.json` dosyaları yükseltmede otomatik olarak tüm anahtarları havuza katar (mevcut davranış korunur); arayüzden tek tek çıkarılabilir.
- **Kart metrikleri:** Günlük Input (Prompt) / Output / Reasoning ayrı ayrı gösterilir; toplam önbellek aynen kalır. Bu günlük değerler **03:00 TSİ'de sıfırlanır** (total sayaçlar sıfırlanmaz).

### 🎯 İstek Bazlı Oturum Anahtarı Dağıtımı (dinamik atama)

- **Dağıtım:** her chat isteği oturumun **köküne** göre (`Root(session)` — subagent'lar kök oturumla **aynı anahtarı** paylaşır) `KeyForSession` ile anahtar alır: bağlı anahtar kullanılabilirse **o** (kararlı); yeni oturum → **en az oturumlu → en çok kota kalan → kart id** sırasıyla havuza dağıtılır. Boş root'lar (models/gömülü) aktif anahtarı alır; **oto geçiş KAPALIYKEN dağıtım yoktur** (eski davranış birebir).
- **Limit bitimi → anında yeniden bağ:** marj aşımı (`DeductUsage`) veya günlük 429 (`RotateAfterDailyLimit`) o anahtarın **tüm oturumlarını** tek adımda havuzdaki sıradaki müsait karta taşır; ayrıca her istekte bağlı anahtar yeniden denetlenir, havuz dışıysa hemen bağlanır. Havuz tamamen biterse oturum bağlı anahtarında kalır — nihai hakem upstream'in günlük 429'u; yenileme **03:00 TSİ**'de olur (bağlar **korunur**, kartlar yeniden kullanılabilir hâle gelir).
- **Kalıcılık:** `keys.json → session_keys` (oturum kökü → kart id); yeniden başlatmada aynen sürer, silinmiş anahtara giden bağ atılır. Log kanıtı: her istekte `key_id`/`key_name`, yeni bağta `[OTURUM ANAHTARI]`, taşımada `[OTURUM YENİDEN BAĞ]`.
- **Testler:** `go test -run TestKeyPool` — 9 oturuma 9 farklı kart (kararlı), marj/günlük 429 yeniden bağlama, havuz boşken upstream-429, restart kalıcılığı, oto-geçiş-kapalı, boş root. Canlı kanarya (03.10.2026): iki farklı `X-Session-Id` → iki farklı kart (`zafer`, `kemal`), tekrar istekte aynı kart.

---

## 🔍 Oturum Tespiti (tek sohbet = tek kart)

Her chat isteği şu kademelerle çözülür (`evren_session.go` + `transcript_store.go`):

1. **Açık kimlik** — gövde `conversation_id`/`session_id` ya da `X-Conversation-Id`/`X-Session-Id` başlığı
2. **Parmak izi** — ilk kullanıcı mesajının `sha256[:12]` → kart kimliği `evs-<fp>` (sohbet her turda tüm geçmişi yeniden gönderdiği için sabit)
3. **Köprü** — parmak izi değiştiyse (compaction/checkpoint) ≥3 mesajlık bitişik örtüşme varsa **oturum bölünmez**, `tier=bridge`
4. **Benimseme** — panoda önceden bölünmüş kart varsa (eski LCP dönemi) yeni kart açılmadan o karta bağlanır
5. **LCP** — hiçbir anahtar yoksa eski içerik-prefix davranışı korunur

Özellikler:

- **Zen + evren ortak depo:** aynı sohbet hangi yoldan (port 8110 / 8088) gelirse gelsin aynı kartta; kimlik eşleşmesinde kaynak (source) farkı engel değildir.
- **Kalıcılık:** `sessions-state.json` (fp takma adları + son 12 digest) — yeniden başlatma sonrası kimlikler **değişmez**; 1sn geciktirilmiş yazım.
- **Log kanıtı:** her isteğe `session_id`, `session_tier` (body/header/fingerprint/bridge/adopt/lcp), `session_fp`, `session_preview`, `client_pid/proc`, `client_headers` yazılır (`usage_history.jsonl`).
- **Yeniden oynatma doğrulaması:** `go test -run TestReplay` — `testdata/replay/` altındaki canlı fixture'lar taze bir deposa oynatılır: altın eşleşme (replay = canlı atama), **bölünme=0, yanlış birleşme=0**.

### Limit gerçeği (canlı deney, 03.10.2026)

EVREN'in dakikalık limiti **API anahtarı bazlıdır** (~2.5M token / ~60 sn), içerik-oturumu bazlı **değildir**: aynı anahtarda farklı içerikli istekler 429 yedi; **taze bir anahtarda stream açıkken bile farklı oturum200** aldı. "Oturum bloğu" görüntüsünü yaratan, ağır bağlam enjeksiyonlardır (~720K prompt token/istek) — dakikada 2-3 atış pencereyi doldurup aynı anahtarın her şeyini429'lar. `Retry-After`/`X-RateLimit-*` başlıkları upstream'den alınır.

**Sınır:** model değişimiyle (flash↔pro) sistem promptu ve bağlam tamamen farklılaşan aynı mantıksal oturum içerikten **tanınamaz**; %100 için istemcinin `X-Session-Id` göndermesi yeterlidir (gateway'deki `header` kademesi hazır ve testli).

---

## 🚀 Başlatma & Çalıştırma

Klasör içindeki `baslat.bat` dosyasına çift tıklamanız yeterlidir.
1. `llm-yonlendirici.exe` tek bir process olarak başlar.
2. Port `8110` (Zen & Hub) ve Port `8088` (EVREN Dual HTTP/HTTPS) dinlemeye geçer.
3. Tarayıcınızda otomatik olarak birleşik kontrol paneli açılır: `http://127.0.0.1:8110/ui`.

Tüm sekmeler arasında üstteki gezinme çubuğu ile tek tıkla geçiş yapabilirsiniz:
- `[🎭 Zen Kostüm]`
- `[🌐 Proxy Yönlendirici]`
- `[⚡ EVREN Token & Kota]`
