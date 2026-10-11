package main

import "net/http"

func (s *ProxyServer) serveProxyUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(proxyUIDashboardHTML))
}

const proxyUIDashboardHTML = `<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>LLM Yönlendirici — Gömülü Proxy Yönlendirici</title>
<script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-slate-950 text-slate-100 antialiased font-sans">
<div class="max-w-6xl mx-auto px-4 sm:px-6 py-6 space-y-6">

  <!-- Unified Top Navigation Bar -->
  <nav class="flex items-center gap-2 border-b border-slate-800 bg-slate-900/60 p-2 rounded-xl">
    <a href="/ui" class="flex items-center gap-2 px-3.5 py-2 text-xs font-semibold text-slate-400 hover:text-white rounded-lg hover:bg-slate-800 transition-colors">
      <span>🎭 Zen Kostüm (Kullanım & Modeller)</span>
    </a>
    <a href="/ui/proxy" class="flex items-center gap-2 px-3.5 py-2 text-xs font-semibold text-emerald-400 bg-slate-800 rounded-lg border border-slate-700 shadow-sm">
      <span>🌐 Proxy Yönlendirici (Havuz & Rotasyon)</span>
    </a>
    <a href="/ui/evren" class="flex items-center gap-2 px-3.5 py-2 text-xs font-semibold text-amber-400 hover:text-amber-300 rounded-lg hover:bg-slate-800 transition-colors">
      <span>⚡ EVREN Token & Kota</span>
    </a>
  </nav>

  <!-- Header -->
  <header class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 border-b border-slate-800 pb-5">
    <div>
      <div class="flex items-center gap-3">
        <h1 class="text-xl font-bold tracking-tight text-white flex items-center gap-2">
          <span class="text-emerald-400">⚡</span> Proxy Yönlendirici
        </h1>
        <span id="badgeListen" class="text-xs px-2.5 py-0.5 rounded-full bg-slate-800 text-slate-300 font-mono border border-slate-700">127.0.0.1:8110</span>
        <span id="badgeActiveTunnels" class="hidden items-center gap-1.5 rounded-full bg-rose-950/80 border border-rose-800 px-2.5 py-0.5 text-xs text-rose-300 font-medium">
          <span class="inline-block h-2 w-2 rounded-full bg-rose-500 animate-pulse"></span>
          <span id="badgeActiveText">0 aktif</span>
        </span>
      </div>
      <p class="text-xs text-slate-400 mt-1">SOCKS5, SOCKS5h ve HTTP havuzlu yerel tünel ve rotasyon servisi (Zen çıkışları bu havuzdan çıkar)</p>
    </div>
    <div class="flex items-center gap-2">
      <button onclick="checkAll()" id="btnCheckAll" class="rounded-lg bg-emerald-600 hover:bg-emerald-500 text-white text-xs font-medium px-3.5 py-2 transition-colors flex items-center gap-1.5 shadow-sm">
        <span>⚡ Tümünü Test Et</span>
      </button>
      <button onclick="fetchStatus()" class="rounded-lg border border-slate-700 hover:border-slate-500 bg-slate-900 text-slate-300 text-xs font-medium px-3.5 py-2 transition-colors">
        Yenile
      </button>
    </div>
  </header>

  <!-- Quick Setup & Mode Switcher -->
  <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
    <!-- Mode Selection -->
    <div class="bg-slate-900 border border-slate-800 rounded-xl p-4 md:col-span-1 space-y-2.5">
      <div class="text-xs font-semibold uppercase tracking-wider text-slate-400">Yönlendirme Modu</div>
      <div class="flex flex-col gap-1.5 text-xs">
        <label class="flex items-center gap-2.5 p-2 rounded-lg border border-slate-800 hover:bg-slate-800/50 cursor-pointer transition-colors" id="lblModeFailover">
          <input type="radio" name="routingMode" value="failover" onchange="changeMode('failover')" class="text-emerald-500 focus:ring-0">
          <div>
            <div class="font-medium text-slate-200">🛡️ Yedekli (Failover)</div>
            <div class="text-[11px] text-slate-500">Seçilen proxy birincil kullanılır; hata anında sıradakine geçer</div>
          </div>
        </label>
        <label class="flex items-center gap-2.5 p-2 rounded-lg border border-slate-800 hover:bg-slate-800/50 cursor-pointer transition-colors" id="lblModeRoundRobin">
          <input type="radio" name="routingMode" value="round-robin" onchange="changeMode('round-robin')" class="text-emerald-500 focus:ring-0">
          <div>
            <div class="font-medium text-slate-200">🔄 Sırayla (Round-Robin)</div>
            <div class="text-[11px] text-slate-500">Her bağlantı canlı proxy'ler arasında döner</div>
          </div>
        </label>
        <label class="flex items-center gap-2.5 p-2 rounded-lg border border-slate-800 hover:bg-slate-800/50 cursor-pointer transition-colors" id="lblModeManual">
          <input type="radio" name="routingMode" value="manual" onchange="changeMode('manual')" class="text-emerald-500 focus:ring-0">
          <div>
            <div class="font-medium text-slate-200">📌 Sabit Seçim (Manual)</div>
            <div class="text-[11px] text-slate-500">Listeden seçtiğin tek bir proxy'ye kilitlenir</div>
          </div>
        </label>
      </div>
    </div>

    <!-- Integration Helper -->
    <div class="bg-slate-900 border border-slate-800 rounded-xl p-4 md:col-span-2 space-y-2">
      <div class="flex items-center justify-between">
        <div class="text-xs font-semibold uppercase tracking-wider text-slate-400">İstemci Entegrasyonu</div>
        <span class="text-[11px] text-slate-500">zen-kostum, curl veya ortam değişkeni</span>
      </div>
      <div class="bg-slate-950 rounded-lg p-3 border border-slate-800/80 font-mono text-xs space-y-2">
        <!-- CMD row -->
        <div class="space-y-1">
          <div class="text-amber-400 font-semibold font-sans flex items-center justify-between text-[11px]">
            <span>💻 CMD (Komut İstemi):</span>
            <span class="text-slate-500 font-mono text-[10px]">tıkla veya kopyala</span>
          </div>
          <div class="flex items-center gap-2">
            <code class="flex-1 bg-slate-900/90 hover:bg-slate-900 px-2.5 py-1.5 rounded border border-slate-800 text-emerald-300 font-bold select-all cursor-pointer transition-colors" onclick="copySnippet('set HTTPS_PROXY=http://127.0.0.1:8110', this)" title="Kopyalamak için tıkla">set HTTPS_PROXY=http://127.0.0.1:8110</code>
            <button onclick="copySnippet('set HTTPS_PROXY=http://127.0.0.1:8110', this)" class="px-2.5 py-1.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-300 text-[11px] font-sans shrink-0 transition-colors">Kopyala</button>
          </div>
        </div>
        
        <!-- PowerShell row -->
        <div class="space-y-1">
          <div class="text-sky-400 font-semibold font-sans text-[11px]">⚡ PowerShell:</div>
          <div class="flex items-center gap-2">
            <code class="flex-1 bg-slate-900/90 hover:bg-slate-900 px-2.5 py-1.5 rounded border border-slate-800 text-emerald-300 font-bold select-all cursor-pointer transition-colors" onclick="copySnippet('$env:HTTPS_PROXY = &quot;http://127.0.0.1:8110&quot;', this)" title="Kopyalamak için tıkla">$env:HTTPS_PROXY = "http://127.0.0.1:8110"</code>
            <button onclick="copySnippet('$env:HTTPS_PROXY = &quot;http://127.0.0.1:8110&quot;', this)" class="px-2.5 py-1.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-300 text-[11px] font-sans shrink-0 transition-colors">Kopyala</button>
          </div>
        </div>

        <!-- Quick test & Reset row -->
        <div class="pt-1.5 border-t border-slate-800/80 font-sans text-[11px] space-y-1.5">
          <div class="flex items-center justify-between gap-2">
            <span class="text-slate-400 shrink-0">Hızlı Test:</span>
            <code class="flex-1 font-mono text-slate-300 bg-slate-900/60 px-2 py-0.5 rounded border border-slate-800/60 select-all cursor-pointer truncate" onclick="copySnippet('curl.exe -x http://127.0.0.1:8110 https://api.ipify.org', this)">curl.exe -x http://127.0.0.1:8110 https://api.ipify.org</code>
            <button onclick="copySnippet('curl.exe -x http://127.0.0.1:8110 https://api.ipify.org', this)" class="px-2 py-0.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-300 text-[10px] shrink-0">Kopyala</button>
          </div>
          <div class="flex items-center justify-between gap-2">
            <span class="text-slate-400 shrink-0">Kaldır (CMD):</span>
            <code class="flex-1 font-mono text-slate-400 bg-slate-900/60 px-2 py-0.5 rounded border border-slate-800/60 select-all cursor-pointer truncate" onclick="copySnippet('set HTTPS_PROXY=', this)">set HTTPS_PROXY=</code>
            <button onclick="copySnippet('set HTTPS_PROXY=', this)" class="px-2 py-0.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-300 text-[10px] shrink-0">Kopyala</button>
          </div>
        </div>
      </div>
    </div>
  </div>

  <!-- Stat Cards -->
  <div class="grid grid-cols-2 sm:grid-cols-5 gap-3" id="statsGrid">
    <div class="bg-slate-900 border border-slate-800 rounded-xl p-3.5">
      <div class="text-[11px] uppercase tracking-wider text-slate-500 font-medium">Canlı Tüneller</div>
      <div id="statActiveTunnels" class="text-2xl font-bold text-slate-400 mt-1">0</div>
    </div>
    <div class="bg-slate-900 border border-slate-800 rounded-xl p-3.5">
      <div class="text-[11px] uppercase tracking-wider text-slate-500 font-medium">Toplam Tünel</div>
      <div id="statTotalTunnels" class="text-2xl font-bold text-sky-400 mt-1">0</div>
    </div>
    <div class="bg-slate-900 border border-slate-800 rounded-xl p-3.5">
      <div class="text-[11px] uppercase tracking-wider text-slate-500 font-medium">Hatalı Tünel</div>
      <div id="statFailedTunnels" class="text-2xl font-bold text-rose-400 mt-1">0</div>
    </div>
    <div class="bg-slate-900 border border-slate-800 rounded-xl p-3.5">
      <div class="text-[11px] uppercase tracking-wider text-slate-500 font-medium">Canlı / Toplam Proxy</div>
      <div class="text-2xl font-bold text-emerald-400 mt-1">
        <span id="statAlive">0</span> <span class="text-sm font-normal text-slate-500">/ <span id="statTotal">0</span></span>
      </div>
    </div>
    <div class="bg-slate-900 border border-slate-800 rounded-xl p-3.5">
      <div class="text-[11px] uppercase tracking-wider text-slate-500 font-medium">Aktarılan Veri</div>
      <div id="statBytes" class="text-sm font-bold font-mono text-slate-200 mt-2">↑ 0 B / ↓ 0 B</div>
    </div>
  </div>

  <!-- Canlı Çalışan Tüneller (In-Flight) -->
  <section id="secActiveTunnels" class="space-y-2 hidden">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-2">
        <span class="inline-block h-2.5 w-2.5 rounded-full bg-rose-500 animate-pulse"></span>
        <h2 class="text-xs font-bold uppercase tracking-widest text-rose-400">Canlı Çalışan Tüneller (Aktif İletim)</h2>
      </div>
      <button onclick="closeAllTunnels()" class="rounded-lg border border-rose-800 bg-rose-950/80 hover:bg-rose-900 text-rose-300 text-xs font-medium px-3 py-1 transition-colors flex items-center gap-1.5 shadow-sm">
        <span>🛑 Tüm Canlı Tünelleri Kapat</span>
      </button>
    </div>
    <div class="overflow-x-auto rounded-xl border border-rose-900/60 bg-slate-900">
      <table class="w-full text-left text-xs border-collapse">
        <thead>
          <tr class="border-b border-rose-900/40 text-[11px] uppercase tracking-wider text-rose-300/80 bg-rose-950/20">
            <th class="py-2.5 px-3">Geçen Süre</th>
            <th class="py-2.5 px-3">İstemci (PID / Port)</th>
            <th class="py-2.5 px-3">Hedef</th>
            <th class="py-2.5 px-3">Kullanılan Proxy</th>
            <th class="py-2.5 px-3 text-center">Durum</th>
            <th class="py-2.5 px-3 text-right">İşlem</th>
          </tr>
        </thead>
        <tbody id="activeTunnelsBody" class="divide-y divide-rose-900/30 font-mono">
        </tbody>
      </table>
    </div>
  </section>

  <!-- Proxy Pool Table -->
  <div class="space-y-2">
    <div class="flex items-center justify-between">
      <h2 class="text-sm font-semibold text-slate-200 flex items-center gap-2">
        <span>🌐 Proxy Havuzu</span>
        <span id="proxyCountBadge" class="text-xs font-normal text-slate-400">(0 proxy)</span>
      </h2>
      <button onclick="clearAll()" class="text-xs text-rose-400 hover:text-rose-300 hover:underline">
        Tüm Havuzu Temizle
      </button>
    </div>

    <div class="overflow-x-auto rounded-xl border border-slate-800 bg-slate-900">
      <table class="w-full text-left text-xs border-collapse">
        <thead>
          <tr class="border-b border-slate-800 text-[11px] uppercase tracking-wider text-slate-400 bg-slate-950/40">
            <th class="py-2.5 px-3">Seçim</th>
            <th class="py-2.5 px-3">Protokol</th>
            <th class="py-2.5 px-3">Adres</th>
            <th class="py-2.5 px-3">Kullanıcı</th>
            <th class="py-2.5 px-3">Durum</th>
            <th class="py-2.5 px-3">Gecikme</th>
            <th class="py-2.5 px-3">Çıkış IP</th>
            <th class="py-2.5 px-3 text-right">İstekler</th>
            <th class="py-2.5 px-3 text-right">İşlem</th>
          </tr>
        </thead>
        <tbody id="proxyTableBody" class="divide-y divide-slate-800/60 font-mono">
          <tr><td colspan="9" class="p-6 text-center text-slate-500 font-sans">Henüz proxy eklenmedi. Aşağıdan ekleyebilirsiniz.</td></tr>
        </tbody>
      </table>
    </div>
  </div>

  <!-- Son Tünellenen İşlemler (Recent History) -->
  <div class="space-y-2">
    <h2 class="text-sm font-semibold text-slate-200 flex items-center gap-2">
      <span>⏱️ Son Tünellenen İşlemler</span>
    </h2>
    <div class="overflow-x-auto rounded-xl border border-slate-800 bg-slate-900">
      <table class="w-full text-left text-xs border-collapse">
        <thead>
          <tr class="border-b border-slate-800 text-[11px] uppercase tracking-wider text-slate-400 bg-slate-950/40">
            <th class="py-2.5 px-3">Zaman</th>
            <th class="py-2.5 px-3">İstemci (PID / Port)</th>
            <th class="py-2.5 px-3">Hedef</th>
            <th class="py-2.5 px-3">Kullanılan Proxy</th>
            <th class="py-2.5 px-3 text-center">Durum</th>
            <th class="py-2.5 px-3 text-right">Süre</th>
            <th class="py-2.5 px-3 text-right">Veri (Yükleme / İndirme)</th>
          </tr>
        </thead>
        <tbody id="recentTunnelsBody" class="divide-y divide-slate-800/60 font-mono">
          <tr><td colspan="7" class="p-6 text-center text-slate-500 font-sans">Henüz tamamlanan tünel işlemi yok.</td></tr>
        </tbody>
      </table>
    </div>
  </div>

  <!-- Bulk Add Section -->
  <div class="bg-slate-900 border border-slate-800 rounded-xl p-4 space-y-3">
    <div class="flex items-center justify-between">
      <h3 class="text-xs font-semibold uppercase tracking-wider text-slate-300">Yeni Proxy Ekle (Toplu veya Tekil)</h3>
      <span class="text-[11px] text-slate-500">Her satıra bir adet yazın</span>
    </div>
    <textarea id="txtProxies" rows="4" placeholder="direct&#10;socks5://127.0.0.1:40000&#10;socks5://0DLDp9:NgGR54@185.239.137.65:8000" class="w-full bg-slate-950 border border-slate-800 rounded-lg p-3 text-xs font-mono text-slate-200 placeholder-slate-600 focus:outline-none focus:border-emerald-500"></textarea>
    <div class="flex items-center justify-between">
      <div id="addStatusMsg" class="text-xs text-slate-400"></div>
      <button onclick="addProxies()" class="rounded-lg bg-emerald-600 hover:bg-emerald-500 text-white text-xs font-medium px-4 py-2 transition-colors shadow-sm">
        Havuzuna Ekle
      </button>
    </div>
  </div>

  <!-- Toast Notification Container -->
  <div id="toastContainer" class="fixed bottom-5 right-5 z-50 flex flex-col gap-2 pointer-events-none"></div>

</div>

<script>
function ts(iso) { return iso ? new Date(iso).toLocaleTimeString('tr-TR') : '-'; }

function formatBytes(bytes) {
  if (!bytes || bytes === 0) return '0 B';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  return (bytes / (1024 * 1024)).toFixed(2) + ' MB';
}

function clientStr(proc, pid, port) {
  if (!port) return '-';
  var pName = proc || 'istemci';
  if (pid) {
    return pName + ' [' + pid + ']:' + port;
  }
  return pName + ':' + port;
}

function fetchStatus() {
  fetch('/api/status')
    .then(function(r) { return r.json(); })
    .then(function(data) {
      render(data);
    })
    .catch(function(err) {
      console.error(err);
    });
}

function render(data) {
  if (data.listen) {
    document.getElementById('badgeListen').innerText = data.listen;
  }

  // Update mode radios
  var radios = document.getElementsByName('routingMode');
  for (var i = 0; i < radios.length; i++) {
    radios[i].checked = (radios[i].value === data.mode);
  }

  // Stats
  var stats = data.stats || {};
  var activeList = data.active || [];
  var recentList = data.recent || [];
  var list = data.proxies || [];

  document.getElementById('statTotal').innerText = list.length;
  document.getElementById('proxyCountBadge').innerText = '(' + list.length + ' proxy)';

  var alive = 0;
  for (var j = 0; j < list.length; j++) {
    if (list[j].alive) alive++;
  }
  document.getElementById('statAlive').innerText = alive;
  document.getElementById('statTotalTunnels').innerText = stats.total_tunnels || 0;
  document.getElementById('statFailedTunnels').innerText = stats.failed_tunnels || 0;
  document.getElementById('statActiveTunnels').innerText = activeList.length;
  if (activeList.length > 0) {
    document.getElementById('statActiveTunnels').className = 'text-2xl font-bold text-rose-400 mt-1';
  } else {
    document.getElementById('statActiveTunnels').className = 'text-2xl font-bold text-slate-400 mt-1';
  }

  document.getElementById('statBytes').innerText = '↑ ' + formatBytes(stats.total_bytes_up) + ' / ↓ ' + formatBytes(stats.total_bytes_down);

  // Active in-flight tunnels
  var badgeActive = document.getElementById('badgeActiveTunnels');
  var secActive = document.getElementById('secActiveTunnels');
  if (activeList.length > 0) {
    badgeActive.classList.remove('hidden');
    badgeActive.classList.add('inline-flex');
    document.getElementById('badgeActiveText').innerText = activeList.length + ' aktif tünel';

    secActive.classList.remove('hidden');
    var tbActive = document.getElementById('activeTunnelsBody');
    var aHtml = '';
    for (var a = 0; a < activeList.length; a++) {
      var at = activeList[a];
      aHtml += '<tr class="hover:bg-rose-950/20 transition-colors">';
      aHtml += '<td class="py-2 px-3 text-amber-300 font-bold">' + (at.elapsed_ms / 1000).toFixed(1) + ' sn</td>';
      aHtml += '<td class="py-2 px-3 text-rose-300 font-semibold">' + clientStr(at.client_proc, at.client_pid, at.client_port) + '</td>';
      aHtml += '<td class="py-2 px-3 text-slate-200">' + at.target + '</td>';
      aHtml += '<td class="py-2 px-3 text-slate-400">' + at.proxy_host + ' (' + at.proxy_type + ')</td>';
      aHtml += '<td class="py-2 px-3 text-center text-emerald-400 font-sans text-[11px]">● Akıyor</td>';
      aHtml += '<td class="py-2 px-3 text-right"><button onclick="closeTunnel(\'' + at.id + '\')" class="px-2 py-0.5 rounded bg-rose-950 hover:bg-rose-800 text-rose-300 text-[10px] border border-rose-800 transition-colors">Kes</button></td>';
      aHtml += '</tr>';
    }
    tbActive.innerHTML = aHtml;
  } else {
    badgeActive.classList.add('hidden');
    badgeActive.classList.remove('inline-flex');
    secActive.classList.add('hidden');
  }

  // Proxy Pool Table
  var tbody = document.getElementById('proxyTableBody');
  if (list.length === 0) {
    tbody.innerHTML = '<tr><td colspan="9" class="p-6 text-center text-slate-500 font-sans">Henüz proxy eklenmedi. Aşağıdan ekleyebilirsiniz.</td></tr>';
  } else {
    var html = '';
    for (var k = 0; k < list.length; k++) {
      var p = list[k];
      var isSelected = (p.id === data.selected_id);
      var rowClass = isSelected ? 'bg-emerald-950/30 border-l-4 border-l-emerald-400 font-medium' : 'hover:bg-slate-800/40 cursor-pointer';

      var schemeColor = 'bg-blue-900/40 text-blue-300 border-blue-800';
      if (p.scheme === 'socks5h') schemeColor = 'bg-purple-900/40 text-purple-300 border-purple-800';
      if (p.scheme === 'http') schemeColor = 'bg-amber-900/40 text-amber-300 border-amber-800';
      if (p.scheme === 'direct') schemeColor = 'bg-emerald-900/60 text-emerald-300 border-emerald-600 font-extrabold';

      var displayHost = p.host;
      if (p.scheme === 'direct') {
        displayHost = '<span class="text-emerald-300 font-semibold flex items-center gap-1.5">⚡ Doğrudan Bağlantı <span class="text-[10px] text-slate-400 font-normal font-sans">(Proxy Yok / Kendi IP\'n)</span></span>';
      } else if (p.host && p.host.indexOf('40000') !== -1) {
        displayHost = '<span class="text-orange-300 font-medium">' + p.host + '</span> <span class="ml-1.5 px-1.5 py-0.5 rounded text-[10px] bg-orange-950/80 text-orange-400 border border-orange-800/80 font-bold font-sans">Cloudflare WARP</span>';
      }

      var statusBadge = p.alive 
        ? '<span class="text-emerald-400 flex items-center gap-1 font-sans text-[11px]">● Canlı</span>'
        : '<span class="text-rose-400 flex items-center gap-1 font-sans text-[11px]" title="' + (p.last_error || '') + '">✖ Hata</span>';

      var latency = p.latency_ms > 0 ? (p.latency_ms + ' ms') : '-';
      var egress = p.egress_ip ? ('<span class="text-emerald-300">' + p.egress_ip + '</span>') : '<span class="text-slate-500">-</span>';

      var selectRadio = '<input type="radio" name="selectedProxy" value="' + p.id + '" ' + (isSelected ? 'checked' : '') + ' onclick="event.stopPropagation(); selectProxy(\'' + p.id + '\')" class="text-emerald-500 cursor-pointer focus:ring-0">';

      var selectBtn = isSelected
        ? '<span class="inline-flex items-center gap-1 text-[11px] font-bold text-emerald-400 bg-emerald-950/80 border border-emerald-700/60 px-2.5 py-0.5 rounded-full shadow-sm"><span class="h-1.5 w-1.5 rounded-full bg-emerald-400 animate-pulse"></span>Aktif</span>'
        : '<button onclick="event.stopPropagation(); selectProxy(\'' + p.id + '\')" class="px-2.5 py-0.5 rounded bg-emerald-950/80 hover:bg-emerald-800 text-emerald-300 hover:text-white border border-emerald-700/80 text-[11px] font-medium transition-colors shadow-sm">Seç</button>';

      html += '<tr class="' + rowClass + ' transition-colors" onclick="selectProxy(\'' + p.id + '\')">';
      html += '<td class="py-2.5 px-3 text-center">' + selectRadio + '</td>';
      html += '<td class="py-2.5 px-3"><span class="px-1.5 py-0.5 rounded text-[10px] uppercase font-bold border ' + schemeColor + '">' + p.scheme + '</span></td>';
      html += '<td class="py-2.5 px-3 text-slate-200 font-medium">' + displayHost + '</td>';
      html += '<td class="py-2.5 px-3 text-slate-400 font-sans">' + (p.user || '<span class="text-slate-600">-</span>') + '</td>';
      html += '<td class="py-2.5 px-3">' + statusBadge + '</td>';
      html += '<td class="py-2.5 px-3 text-slate-300">' + latency + '</td>';
      html += '<td class="py-2.5 px-3 font-semibold">' + egress + '</td>';
      html += '<td class="py-2.5 px-3 text-right font-sans"><span class="text-sky-400">' + (p.total_reqs || 0) + '</span> / <span class="text-rose-400">' + (p.failed_reqs || 0) + '</span></td>';
      html += '<td class="py-2.5 px-3 text-right font-sans space-x-1.5 flex items-center justify-end">' +
              selectBtn +
              '<button onclick="event.stopPropagation(); checkProxy(\'' + p.id + '\')" class="px-2 py-0.5 rounded bg-slate-800 hover:bg-slate-700 text-slate-300 text-[11px]">Test</button>' +
              '<button onclick="event.stopPropagation(); deleteProxy(\'' + p.id + '\')" class="px-2 py-0.5 rounded bg-slate-800 hover:bg-rose-900/60 text-rose-400 text-[11px]">Sil</button>' +
              '</td>';
      html += '</tr>';
    }
    tbody.innerHTML = html;
  }

  // Recent Tunnels Table
  var rbody = document.getElementById('recentTunnelsBody');
  if (recentList.length === 0) {
    rbody.innerHTML = '<tr><td colspan="7" class="p-6 text-center text-slate-500 font-sans">Henüz tamamlanan tünel işlemi yok.</td></tr>';
  } else {
    var rHtml = '';
    for (var m = 0; m < recentList.length; m++) {
      var rc = recentList[m];
      var isOk = (rc.status === 'OK');
      var stClass = isOk ? 'text-emerald-400' : 'text-rose-400';
      var stText = isOk ? '● OK' : ('✖ ' + (rc.status || 'Hata'));

      rHtml += '<tr class="hover:bg-slate-800/30 transition-colors">';
      rHtml += '<td class="py-2 px-3 text-slate-400">' + ts(rc.time) + '</td>';
      rHtml += '<td class="py-2 px-3 text-slate-300 font-semibold">' + clientStr(rc.client_proc, rc.client_pid, rc.client_port) + '</td>';
      rHtml += '<td class="py-2 px-3 text-slate-200">' + rc.target + '</td>';
      rHtml += '<td class="py-2 px-3 text-slate-400">' + rc.proxy_host + '</td>';
      rHtml += '<td class="py-2 px-3 text-center ' + stClass + ' font-sans font-medium text-[11px]">' + stText + '</td>';
      rHtml += '<td class="py-2 px-3 text-right text-slate-300">' + rc.duration_ms + ' ms</td>';
      rHtml += '<td class="py-2 px-3 text-right text-slate-400 text-[11px]">↑ ' + formatBytes(rc.bytes_up) + ' / ↓ ' + formatBytes(rc.bytes_down) + '</td>';
      rHtml += '</tr>';
    }
    rbody.innerHTML = rHtml;
  }
}

function changeMode(mode) {
  fetch('/api/mode', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mode: mode, reset_tunnels: true })
  })
  .then(function(r) { return r.json(); })
  .then(function(res) {
    showToast('Yönlendirme modu güncellendi: ' + mode, 'info');
    fetchStatus();
  })
  .catch(function(err) {
    showToast('Mod değiştirilemedi: ' + err, 'error');
  });
}

function selectProxy(id) {
  var currentMode = 'failover';
  var radios = document.getElementsByName('routingMode');
  for (var i = 0; i < radios.length; i++) {
    if (radios[i].checked) {
      currentMode = radios[i].value;
      break;
    }
  }
  var targetMode = (currentMode === 'round-robin') ? 'failover' : currentMode;

  fetch('/api/mode', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mode: targetMode, selected_id: id, reset_tunnels: true })
  })
  .then(function(r) { return r.json(); })
  .then(function(res) {
    var msg = 'Seçilen proxy aktif edildi!';
    if (res.tunnels_closed > 0) {
      msg += ' (' + res.tunnels_closed + ' aktif tünel sıfırlandı, yeni istekler bu proxye geçecek)';
    } else {
      msg += ' (Sonraki tüm istekler anında bu proxye yönlenecek)';
    }
    showToast(msg, 'success');
    fetchStatus();
  })
  .catch(function(err) {
    showToast('Hata: ' + err, 'error');
  });
}

function selectManual(id) {
  selectProxy(id);
}

function closeTunnel(id) {
  fetch('/api/tunnels/close', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id: id })
  })
  .then(function() {
    showToast('Tünel kapatıldı', 'info');
    fetchStatus();
  });
}

function closeAllTunnels() {
  fetch('/api/tunnels/close', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: '{}'
  })
  .then(function(r) { return r.json(); })
  .then(function(res) {
    showToast((res.tunnels_closed || 0) + ' canlı tünel kapatıldı', 'info');
    fetchStatus();
  });
}

function showToast(msg, type) {
  var container = document.getElementById('toastContainer');
  if (!container) return;
  var el = document.createElement('div');
  var bg = 'bg-slate-900 border-slate-700 text-slate-200';
  if (type === 'success') bg = 'bg-emerald-950 border-emerald-500 text-emerald-200';
  if (type === 'error') bg = 'bg-rose-950 border-rose-600 text-rose-200';
  if (type === 'info') bg = 'bg-sky-950 border-sky-600 text-sky-200';

  el.className = 'px-4 py-2.5 rounded-lg border shadow-2xl text-xs font-medium transition-all duration-300 transform translate-y-2 opacity-0 pointer-events-auto flex items-center gap-2 ' + bg;
  el.innerText = msg;
  container.appendChild(el);

  setTimeout(function() {
    el.classList.remove('translate-y-2', 'opacity-0');
  }, 10);

  setTimeout(function() {
    el.classList.add('translate-y-2', 'opacity-0');
    setTimeout(function() {
      if (el.parentNode) el.parentNode.removeChild(el);
    }, 300);
  }, 3500);
}

function checkAll() {
  var btn = document.getElementById('btnCheckAll');
  btn.disabled = true;
  btn.innerText = 'Test Başlatıldı...';
  fetch('/api/check', { method: 'POST', body: '{}' })
    .then(function() {
      setTimeout(function() {
        btn.disabled = false;
        btn.innerText = '⚡ Tümünü Test Et';
        fetchStatus();
      }, 2500);
    });
}

function checkProxy(id) {
  fetch('/api/check', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id: id })
  }).then(function() {
    setTimeout(fetchStatus, 1500);
  });
}

function deleteProxy(id) {
  if (!confirm('Bu proxy kaldırılsın mı?')) return;
  fetch('/api/proxies/delete', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id: id })
  }).then(fetchStatus);
}

function clearAll() {
  if (!confirm('Tüm proxy havuzu temizlensin mi?')) return;
  fetch('/api/proxies/clear', { method: 'POST' }).then(fetchStatus);
}

function addProxies() {
  var txt = document.getElementById('txtProxies').value;
  if (!txt.trim()) return;

  var statusMsg = document.getElementById('addStatusMsg');
  statusMsg.innerText = 'Ekleniyor...';

  fetch('/api/proxies', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text: txt })
  })
  .then(function(r) { return r.json(); })
  .then(function(res) {
    if (res.added > 0) {
      statusMsg.innerHTML = '<span class="text-emerald-400">' + res.added + ' proxy başarıyla eklendi!</span>';
      document.getElementById('txtProxies').value = '';
      fetchStatus();
      checkAll();
    } else if (res.errors && res.errors.length > 0) {
      statusMsg.innerHTML = '<span class="text-rose-400">Hata: ' + res.errors.join(', ') + '</span>';
    }
  })
  .catch(function(err) {
    statusMsg.innerText = 'İstek hatası: ' + err;
  });
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

// Initial fetch & fast auto-refresh every 1.5s
fetchStatus();
setInterval(fetchStatus, 1500);
</script>
</body>
</html>`
