package main

import (
	"net/http"
)

// uiHTML is the whole /ui page: one file, Tailwind from the Play CDN, no
// other external assets. The script polls /kullanim and renders the report.
// No template literals — this lives in a Go raw string.
const uiHTML = `<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>zen-kostum — kullanım</title>
<script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-slate-950 text-slate-200 antialiased font-sans">
<!-- Unified Top Navigation Bar -->
<nav class="flex items-center gap-2 border-b border-slate-800 bg-slate-900/60 px-6 py-2.5">
  <a href="/ui" class="flex items-center gap-2 px-3.5 py-1.5 text-xs font-semibold text-sky-400 bg-slate-800 rounded-lg border border-slate-700 shadow-sm">
    <span>🎭 Zen Kostüm (Kullanım & Modeller)</span>
  </a>
  <a href="/ui/proxy" class="flex items-center gap-2 px-3.5 py-1.5 text-xs font-semibold text-slate-400 hover:text-white rounded-lg hover:bg-slate-800 transition-colors">
    <span>🌐 Proxy Yönlendirici (Havuz & Rotasyon)</span>
  </a>
  <a href="/ui/evren" class="flex items-center gap-2 px-3.5 py-1.5 text-xs font-semibold text-amber-400 hover:text-amber-300 rounded-lg hover:bg-slate-800 transition-colors">
    <span>⚡ EVREN Token & Kota</span>
  </a>
</nav>

<header class="flex flex-wrap items-baseline gap-x-4 gap-y-1 px-6 pt-4">
  <div class="flex items-center gap-2">
    <h1 class="text-lg font-semibold text-slate-100">zen-kostum — kullanım</h1>
    <span id="badgeActive" class="hidden items-center gap-1.5 rounded-full bg-rose-950/80 border border-rose-800 px-2.5 py-0.5 text-xs text-rose-300 font-medium">
      <span class="inline-block h-2 w-2 rounded-full bg-rose-500 animate-pulse"></span>
      <span id="badgeActiveText">0 aktif</span>
    </span>
  </div>
  <button onclick="yenile()" class="rounded-lg border border-slate-700 bg-slate-900 px-3 py-1 text-xs text-slate-300 transition-colors hover:border-slate-500">Yenile</button>
  <span id="bilgi" class="text-xs text-slate-500">yükleniyor…</span>
</header>

<main class="space-y-6 px-6 pb-12 pt-4">
  <!-- Base URL & Gömülü Proxy Havuz Bilgilendirme Kutusu -->
  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
    <!-- Base URL Kartı -->
    <div class="rounded-xl border border-sky-900/60 bg-slate-900/90 p-4 space-y-2 text-xs">
      <div class="flex items-center justify-between">
        <div class="font-bold text-sky-400 flex items-center gap-1.5">
          <span>🎯</span>
          <span>ZEN OPENAI BASE URL:</span>
        </div>
        <span class="rounded bg-sky-950 px-2 py-0.5 text-[11px] font-semibold text-sky-300 border border-sky-800">FREE TIER</span>
      </div>
      <div class="flex items-center gap-2">
        <code id="zenBaseUrl" class="flex-1 rounded-lg bg-slate-950 px-3 py-2 font-mono text-xs text-sky-300 border border-slate-800 select-all">http://127.0.0.1:8110/v1</code>
        <button onclick="navigator.clipboard.writeText('http://127.0.0.1:8110/v1'); this.innerText='Kopyalandı!'; setTimeout(() => this.innerText='Kopyala', 1500);" class="rounded-lg bg-sky-600 hover:bg-sky-500 text-white font-semibold px-3 py-2 text-xs transition-colors shadow-sm">Kopyala</button>
      </div>
      <p class="text-[11px] text-slate-400">Modeller: <code class="text-slate-300 font-mono">mimo-v2.6-flash-free</code>, <code class="text-slate-300 font-mono">space-bunny-free</code> (API Key: <code class="text-slate-300 font-mono">Bearer public</code>)</p>
    </div>

    <!-- Proxy Bilgilendirme Kartı -->
    <div class="rounded-xl border border-emerald-900/60 bg-slate-900/90 p-4 space-y-2 text-xs">
      <div class="flex items-center justify-between">
        <div class="font-bold text-emerald-400 flex items-center gap-1.5">
          <span>🛡️</span>
          <span>GÖMÜLÜ PROXY HAVUZU AKTİF:</span>
        </div>
        <a href="/ui/proxy" class="rounded bg-emerald-950 px-2 py-0.5 text-[11px] font-semibold text-emerald-300 border border-emerald-800 hover:bg-emerald-900 transition-colors">YÖNET →</a>
      </div>
      <p class="text-[11px] text-slate-400">Zen upstream çıkış istekleri (opencode.ai/zen/v1) doğrudan gömülü proxy havuzu üzerinden tünellenmektedir (IP ban / 429 koruması).</p>
      <div class="pt-1">
        <a href="/ui/proxy" class="text-xs text-emerald-400 hover:underline font-semibold flex items-center gap-1">
          <span>🌐 6 Proxy Havuzunu ve Tünelleri Görüntüle →</span>
        </a>
      </div>
    </div>
  </div>

  <!-- Stat Cards -->
  <div id="kartlar" class="grid grid-cols-2 gap-3 sm:grid-cols-4 lg:grid-cols-8"></div>

  <!-- Canlı Çalışan İstekler (In-Flight) -->
  <section id="secActive" class="space-y-2 hidden">
    <div class="flex items-center gap-2">
      <span class="inline-block h-2.5 w-2.5 rounded-full bg-rose-500 animate-pulse"></span>
      <h2 class="text-xs font-bold uppercase tracking-widest text-rose-400">Canlı Çalışan İstekler (Zen Bekleniyor)</h2>
    </div>
    <div class="overflow-hidden rounded-xl border border-rose-900/60 bg-slate-900">
      <table class="w-full table-fixed text-[13px]">
        <thead>
          <tr class="bg-rose-950/30 text-[11px] uppercase tracking-wider text-rose-300/80 border-b border-rose-900/50">
            <th class="w-24 px-3 py-2 text-left font-medium">Oturum</th>
            <th class="px-3 py-2 text-left font-medium">Açılış mesajı</th>
            <th class="w-[20%] px-3 py-2 text-left font-medium">Model</th>
            <th class="w-48 px-3 py-2 text-left font-medium">İstemci (PID / Port)</th>
            <th class="w-24 px-3 py-2 text-right font-medium">Geçen Süre</th>
            <th class="w-20 px-3 py-2 text-left font-medium">Tür</th>
          </tr>
        </thead>
        <tbody id="aktifler" class="divide-y divide-rose-900/30 font-mono"></tbody>
      </table>
    </div>
  </section>

  <!-- Oturumlar Tablosu -->
  <section class="space-y-2">
    <h2 class="text-xs font-semibold uppercase tracking-widest text-slate-400">Oturumlar</h2>
    <div class="overflow-hidden rounded-xl border border-slate-800">
      <table class="w-full table-fixed bg-slate-900 text-[13px]">
        <thead>
          <tr class="text-[11px] uppercase tracking-wider text-slate-500 bg-slate-950/40 border-b border-slate-800">
            <th class="w-24 px-3 py-2 text-left font-medium">Oturum</th>
            <th class="px-3 py-2 text-left font-medium">Açılış mesajı</th>
            <th class="w-14 px-3 py-2 text-right font-medium">İstek</th>
            <th class="w-20 px-3 py-2 text-right font-medium">Girdi</th>
            <th class="w-20 px-3 py-2 text-right font-medium">Önbellek</th>
            <th class="w-20 px-3 py-2 text-right font-medium">İşlenen</th>
            <th class="w-20 px-3 py-2 text-right font-medium">Çıktı</th>
            <th class="w-24 px-3 py-2 text-right font-medium">Reasoning</th>
            <th class="w-12 px-3 py-2 text-right font-medium">Hata</th>
            <th class="w-[22%] px-3 py-2 text-left font-medium">Modeller</th>
            <th class="w-32 px-3 py-2 text-left font-medium">Son etkinlik</th>
          </tr>
        </thead>
        <tbody id="oturumlar" class="divide-y divide-slate-800/60 font-mono"></tbody>
      </table>
    </div>
  </section>

  <!-- Son İşlemler Tablosu -->
  <section class="space-y-2">
    <h2 class="text-xs font-semibold uppercase tracking-widest text-slate-400">Son işlemler</h2>
    <div class="overflow-hidden rounded-xl border border-slate-800">
      <table class="w-full table-fixed bg-slate-900 text-[13px]">
        <thead>
          <tr class="text-[11px] uppercase tracking-wider text-slate-500 bg-slate-950/40 border-b border-slate-800">
            <th class="w-32 px-3 py-2 text-left font-medium">Zaman</th>
            <th class="px-3 py-2 text-left font-medium">Oturum</th>
            <th class="w-[18%] px-3 py-2 text-left font-medium">Model</th>
            <th class="w-44 px-3 py-2 text-left font-medium">İstemci (PID / Port)</th>
            <th class="w-14 px-3 py-2 text-right font-medium">Durum</th>
            <th class="w-20 px-3 py-2 text-right font-medium">Süre</th>
            <th class="w-20 px-3 py-2 text-right font-medium">Girdi</th>
            <th class="w-20 px-3 py-2 text-right font-medium">Önbellek</th>
            <th class="w-20 px-3 py-2 text-right font-medium">İşlenen</th>
            <th class="w-20 px-3 py-2 text-right font-medium">Çıktı</th>
            <th class="w-24 px-3 py-2 text-right font-medium">Reasoning</th>
            <th class="w-20 px-3 py-2 text-left font-medium">Tür</th>
          </tr>
        </thead>
        <tbody id="son" class="divide-y divide-slate-800/60 font-mono"></tbody>
      </table>
    </div>
  </section>
</main>

<script>
function tr(n) { return (n || 0).toLocaleString('tr-TR'); }
function ts(iso) { return iso ? new Date(iso).toLocaleString('tr-TR') : '-'; }
var TD = 'px-3 py-2';
var TD_SAYI = TD + ' text-right tabular-nums';
var TD_KES = TD + ' truncate';

function clientStr(proc, pid, port) {
  if (!port) return '-';
  var pName = proc || 'bilinmeyen';
  if (pid) {
    return pName + ' [' + pid + ']:' + port;
  }
  return pName + ':' + port;
}

function satir(govde, hucreler) {
  var r = document.createElement('tr');
  r.className = 'hover:bg-slate-800/30 transition-colors';
  for (var i = 0; i < hucreler.length; i++) {
    var c = document.createElement('td');
    c.className = hucreler[i][0];
    c.textContent = hucreler[i][1];
    r.appendChild(c);
  }
  govde.appendChild(r);
}

function temizle(govde, colSpan, mesaj) {
  govde.innerHTML = '';
  var r = document.createElement('tr');
  var c = document.createElement('td');
  c.colSpan = colSpan;
  c.className = TD + ' py-6 text-center text-slate-500 font-sans';
  c.textContent = mesaj;
  r.appendChild(c);
  govde.appendChild(r);
}

function islenen(girdi, onbellek) { return tr((girdi || 0) - (onbellek || 0)); }

function modelListesi(modeller) {
  var adlar = Object.keys(modeller || {});
  for (var i = 0; i < adlar.length; i++) {
    adlar[i] = adlar[i] + ' ×' + modeller[adlar[i]];
  }
  return adlar.join(', ') || '-';
}

async function yenile() {
  try {
    var r = await fetch('kullanim');
    var d = await r.json();
    document.getElementById('bilgi').textContent =
      'süreç başlangıcı ' + ts(d.started) +
      ' · ilk kayıt ' + ts(d.first_log) +
      ' · toplam ' + tr(d.totals.requests) + ' istek';

    // Active in-flight badge & section
    var activeList = d.active || [];
    var badge = document.getElementById('badgeActive');
    var secActive = document.getElementById('secActive');
    if (activeList.length > 0) {
      badge.classList.remove('hidden');
      badge.classList.add('inline-flex');
      document.getElementById('badgeActiveText').textContent = activeList.length + ' canlı istek';

      secActive.classList.remove('hidden');
      var tbActive = document.getElementById('aktifler');
      tbActive.innerHTML = '';
      for (var a = 0; a < activeList.length; a++) {
        var act = activeList[a];
        satir(tbActive, [
          [TD_KES, act.session],
          [TD_KES, act.preview || '-'],
          [TD_KES, act.model || '-'],
          [TD_KES + ' text-rose-300 font-semibold', clientStr(act.client_proc, act.client_pid, act.client_port)],
          [TD_SAYI + ' text-amber-300 font-bold', (act.elapsed_ms / 1000).toFixed(1) + ' sn'],
          [TD, act.streamed ? 'akış' : 'toplanmış']
        ]);
      }
    } else {
      badge.classList.add('hidden');
      badge.classList.remove('inline-flex');
      secActive.classList.add('hidden');
    }

    // Stat Cards (8 cards)
    var kartlar = document.getElementById('kartlar');
    kartlar.innerHTML = '';
    var oran = d.totals.prompt > 0 ? Math.round(100 * d.totals.cached / d.totals.prompt) : 0;
    var veriler = [
      ['Aktif İstek', String(activeList.length), activeList.length > 0 ? 'text-rose-400 font-bold' : 'text-slate-400', activeList.length > 0 ? 'Zen bekleniyor' : 'boşta'],
      ['İstek (Biten)', tr(d.totals.requests), 'text-slate-100', ''],
      ['Girdi token', tr(d.totals.prompt), 'text-sky-300', ''],
      ['Önbellek', tr(d.totals.cached), 'text-violet-300', 'isabet %' + oran],
      ['İşlenen girdi', islenen(d.totals.prompt, d.totals.cached), 'text-cyan-300', 'girdi − önbellek'],
      ['Çıktı token', tr(d.totals.completion), 'text-emerald-300', ''],
      ['Reasoning', tr(d.totals.reasoning), 'text-amber-300', ''],
      ['Hatalı istek', tr(d.totals.errors), 'text-red-400', '']
    ];
    for (var i = 0; i < veriler.length; i++) {
      var kart = document.createElement('div');
      kart.className = 'rounded-xl border border-slate-800 bg-slate-900 px-4 py-3';
      var e = document.createElement('div');
      e.className = 'text-[11px] font-medium uppercase tracking-widest text-slate-500';
      e.textContent = veriler[i][0];
      var v = document.createElement('div');
      v.className = 'mt-1 text-2xl font-semibold tabular-nums ' + veriler[i][2];
      v.textContent = veriler[i][1];
      kart.appendChild(e);
      kart.appendChild(v);
      if (veriler[i][3]) {
        var sub = document.createElement('div');
        sub.className = 'mt-0.5 text-[11px] text-slate-500';
        sub.textContent = veriler[i][3];
        kart.appendChild(sub);
      }
      kartlar.appendChild(kart);
    }

    // Sessions Table
    var oturumlar = document.getElementById('oturumlar');
    if (!d.sessions.length) {
      temizle(oturumlar, 11, 'Henüz kayıt yok — bir sohbet başlat, sayfa canlı yenilenir.');
    } else {
      oturumlar.innerHTML = '';
      for (var s = 0; s < d.sessions.length; s++) {
        var o = d.sessions[s];
        satir(oturumlar, [
          [TD_KES, o.session],
          [TD_KES, o.preview || '-'],
          [TD_SAYI, tr(o.requests)],
          [TD_SAYI, tr(o.prompt)],
          [TD_SAYI, tr(o.cached)],
          [TD_SAYI, islenen(o.prompt, o.cached)],
          [TD_SAYI, tr(o.completion)],
          [TD_SAYI, tr(o.reasoning)],
          [TD_SAYI, o.errors ? tr(o.errors) : '-'],
          [TD_KES, modelListesi(o.models)],
          [TD, ts(o.last_seen)]
        ]);
      }
    }

    // Recent Table
    var son = document.getElementById('son');
    if (!d.recent.length) {
      temizle(son, 12, 'Henüz kayıt yok.');
    } else {
      son.innerHTML = '';
      for (var k = 0; k < d.recent.length; k++) {
        var m = d.recent[k];
        satir(son, [
          [TD, ts(m.time)],
          [TD_KES, m.preview || m.session || '-'],
          [TD_KES, m.model || '-'],
          [TD_KES + ' text-slate-300', clientStr(m.client_proc, m.client_pid, m.client_port)],
          [TD_SAYI + (m.status === 200 ? ' font-semibold text-emerald-400' : ' font-semibold text-red-400'), String(m.status)],
          [TD_SAYI, tr(m.duration_ms) + ' ms'],
          [TD_SAYI, tr(m.prompt_tokens)],
          [TD_SAYI, tr(m.cached_tokens)],
          [TD_SAYI, islenen(m.prompt_tokens, m.cached_tokens)],
          [TD_SAYI, tr(m.completion_tokens)],
          [TD_SAYI, tr(m.reasoning_tokens)],
          [TD, m.streamed ? 'akış' : 'toplanmış']
        ]);
      }
    }
  } catch (hata) {
    document.getElementById('bilgi').textContent = 'okunamadı: ' + hata;
  }
}

function copySnippet(text, el) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(function() {
      var orig = el.innerText;
      el.innerText = 'Kopyalandı!';
      setTimeout(function() { el.innerText = orig; }, 1500);
    });
  }
}

// Initial fetch and 1.5s live polling
yenile();
setInterval(yenile, 1500);
</script>
</body>
</html>`

// serveUI answers with the usage page.
func (g *gateway) serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(uiHTML))
}

// serveStats answers with the aggregated usage report.
func (g *gateway) serveStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, g.usage.report(g.started))
}

// serveRoot redirects a bare / to the usage page and 404s everything else
// the mux would otherwise swallow.
func (g *gateway) serveRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/ui", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}
