// TOKEN USAGE // observatory  —  vanilla JS dashboard
// All dynamic rendering uses DOM APIs (createElement / textContent) — never innerHTML —
// so server-supplied strings can never be parsed as HTML.
(() => {
  "use strict";

  // ---- model color palette ------------------------------------------------
  const MODEL_PALETTE = [
    { test: /opus-4|opus-3/i,           color: "#ff5c1a" },
    { test: /sonnet-4/i,                color: "#7ec7c2" },
    { test: /haiku-4/i,                 color: "#e8c47e" },
    { test: /3-5-sonnet|3\.5-sonnet/i,  color: "#5b8d8a" },
    { test: /3-5-haiku|3\.5-haiku/i,    color: "#b89860" },
    { test: /3-opus/i,                  color: "#c64b1a" },
  ];
  const FALLBACK_PALETTE = ["#d96e9a", "#7a9ec6", "#94c67e", "#c67e7e", "#7a7466"];
  const fallbackUsed = new Map();

  function colorFor(model) {
    for (const m of MODEL_PALETTE) if (m.test.test(model)) return m.color;
    if (!fallbackUsed.has(model)) {
      fallbackUsed.set(model, FALLBACK_PALETTE[fallbackUsed.size % FALLBACK_PALETTE.length]);
    }
    return fallbackUsed.get(model);
  }

  // Tool palette — fixed swatches for known tools, fallback otherwise.
  const TOOL_COLORS = {
    "claude-code": "#ff5c1a",
    "codex":       "#74aa9c",  // OpenAI mint
    "opencode":    "#a1a5e8",
  };
  function colorForTool(t) { return TOOL_COLORS[t] || "#7a7466"; }

  // ---- formatters ---------------------------------------------------------
  const fmtUSD = (n) => {
    if (!isFinite(n)) return "—";
    if (n >= 1000) return n.toLocaleString("en-US", { maximumFractionDigits: 0 });
    return n.toFixed(2);
  };
  const fmtTokens = (n) => {
    if (!isFinite(n) || n === 0) return "0";
    if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
    if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
    if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
    return n.toString();
  };
  const fmtInt = (n) => n.toLocaleString("en-US");
  const shortenModel = (m) => m.replace(/^claude-/, "").replace(/-\d{8}$/, "");
  const NS_SVG = "http://www.w3.org/2000/svg";

  // tiny DOM helper: el("div", {class: ...}, [children]) or text
  function el(tag, attrs, children) {
    const node = document.createElement(tag);
    if (attrs) for (const k in attrs) {
      if (k === "class") node.className = attrs[k];
      else if (k === "style") node.style.cssText = attrs[k];
      else if (k.startsWith("data-")) node.setAttribute(k, attrs[k]);
      else if (k.startsWith("on") && typeof attrs[k] === "function") node.addEventListener(k.slice(2), attrs[k]);
      else node.setAttribute(k, attrs[k]);
    }
    if (children == null) return node;
    if (!Array.isArray(children)) children = [children];
    for (const c of children) {
      if (c == null) continue;
      node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    }
    return node;
  }
  function svg(tag, attrs) {
    const node = document.createElementNS(NS_SVG, tag);
    if (attrs) for (const k in attrs) node.setAttribute(k, attrs[k]);
    return node;
  }
  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  // ---- state --------------------------------------------------------------
  const state = {
    rangeDays: 30,
    user: "",
    rows: [],
    clock: [],          // /clock rows: per-(dow,hour) totals in the browser's tz
    users: [],          // registered users (from /users); may include zero-data users
    prices: null,       // /prices history, lazily loaded on first modal open
    sortKey: "day",
    sortDir: "desc",
    filter: "",
    metric: "cost",     // "cost" (USD) | "tokens" — Daily bar chart axis
  };

  // Trend overlays: trailing window for the moving-average line, and the
  // factor a day's metric must exceed (vs its trailing avg) to trip the alert.
  const MA_WINDOW = 7, SPIKE_FACTOR = 2;

  // ---- DOM refs -----------------------------------------------------------
  const $ = (id) => document.getElementById(id);
  const els = {
    rpButtons:    document.querySelectorAll(".rp-btn"),
    metricButtons:document.querySelectorAll(".mt-btn"),
    userSelect:   $("user-select"),
    clock:        $("clock"),
    heroRange:    $("hero-range"),
    totalCost:    $("total-cost"),
    totalIn:      $("total-in"),
    totalOut:     $("total-out"),
    totalCw:      $("total-cw"),
    totalCr:      $("total-cr"),
    totalTok:     $("total-tok"),
    totalMsgs:    $("total-msgs"),
    heroSavings:  $("hero-savings"),
    heroForecast: $("hero-forecast"),
    legend:       $("legend"),
    spikeBanner:  $("spike-alert"),
    svg:          $("bars"),
    yaxis:        $("yaxis"),
    tooltip:      $("tooltip"),
    chartWrap:    document.querySelector(".chart-wrap"),
    modelRank:    $("model-rank"),
    userRank:     $("user-rank"),
    toolRank:     $("tool-rank"),
    heatmap:      $("heatmap"),
    heatmapWrap:  document.querySelector(".heatmap-wrap"),
    heatmapTip:   $("heatmap-tooltip"),
    clockHeatmap: $("clock-heatmap"),
    clockWrap:    document.querySelector(".clock-wrap"),
    clockTip:     $("clock-tooltip"),
    priceModal:   $("price-modal"),
    priceTitle:   $("price-title"),
    priceLegend:  $("price-legend"),
    priceChart:   $("price-chart"),
    priceClose:   $("price-close"),
    statsBtn:     $("stats-btn"),
    statsModal:   $("stats-modal"),
    statsLegend:  $("stats-legend"),
    statsChart:   $("stats-chart"),
    statsClose:   $("stats-close"),
    ledgerBody:   document.querySelector("#ledger tbody"),
    ledgerHead:   document.querySelectorAll("#ledger thead th"),
    ledgerFilter: $("ledger-filter"),
    emptyState:   $("empty-state"),
    emptyEndpoint:$("empty-endpoint"),
    lastFetch:    $("lastfetch"),
  };

  // ---- clock --------------------------------------------------------------
  function tickClock() {
    const d = new Date();
    const pad = (n) => String(n).padStart(2, "0");
    const s = `${d.getFullYear()}·${pad(d.getMonth()+1)}·${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
    clear(els.clock);
    els.clock.appendChild(el("span", { class: "cur" }, "▍"));
    els.clock.appendChild(document.createTextNode(" " + s));
  }
  setInterval(tickClock, 1000); tickClock();

  // ---- fetch --------------------------------------------------------------
  async function load() {
    const params = new URLSearchParams();
    if (state.user) params.set("user", state.user);
    if (state.rangeDays > 0) {
      const from = new Date(Date.now() - state.rangeDays * 86400_000);
      params.set("from", from.toISOString());
    }
    const sumURL = "/summary" + (params.toString() ? "?" + params.toString() : "");
    try {
      const sumRes = await fetch(sumURL, { headers: { accept: "application/json" }});
      if (!sumRes.ok) throw new Error("/summary HTTP " + sumRes.status);
      state.rows = (await sumRes.json()) || [];
    } catch (e) {
      console.error("/summary fetch failed", e);
      els.lastFetch.textContent = "fetch failed @ " + new Date().toLocaleTimeString();
      return;
    }
    els.lastFetch.textContent = "synced @ " + new Date().toLocaleTimeString();
    render();
    // /clock reads usage_detail (sub-day grain), so fetch it in the background
    // and bucket by the browser's local timezone — "when do I code" only makes
    // sense in wall-clock time. Re-renders the Rhythm card when it lands.
    const clockParams = new URLSearchParams(params);
    clockParams.set("tz", Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC");
    fetch("/clock?" + clockParams.toString(), { headers: { accept: "application/json" }})
      .then(r => r.ok ? r.json() : [])
      .then(c => { state.clock = c || []; renderClock(); })
      .catch(e => console.warn("/clock fetch failed (non-fatal)", e));
    // Fire /users in the background — it can be 20s+ during heavy /ingest
    // (pool saturation), and the dashboard's main content doesn't need it.
    // When it arrives, just refresh the USER dropdown.
    fetch("/users", { headers: { accept: "application/json" }})
      .then(r => r.ok ? r.json() : [])
      .then(u => { state.users = u || []; renderUserOptions(); })
      .catch(e => console.warn("/users fetch failed (non-fatal)", e));
  }

  // ---- input handlers -----------------------------------------------------
  els.rpButtons.forEach(btn => btn.addEventListener("click", () => {
    els.rpButtons.forEach(b => b.classList.remove("is-active"));
    btn.classList.add("is-active");
    state.rangeDays = Number(btn.dataset.range);
    const label = state.rangeDays === 0
      ? "all-time"
      : state.rangeDays === 1 ? "last 24h" : `last ${state.rangeDays} days`;
    els.heroRange.textContent = label;
    load();
  }));

  els.userSelect.addEventListener("change", () => {
    state.user = els.userSelect.value;
    load();
  });

  els.metricButtons.forEach(btn => btn.addEventListener("click", () => {
    els.metricButtons.forEach(b => b.classList.remove("is-active"));
    btn.classList.add("is-active");
    state.metric = btn.dataset.metric; // "cost" | "tokens"
    // Pure re-render — no /summary refetch needed, the raw rows already
    // have all four token counters plus cost_usd.
    render();
  }));

  els.ledgerFilter.addEventListener("input", () => {
    state.filter = els.ledgerFilter.value.toLowerCase();
    renderLedger();
  });

  els.ledgerHead.forEach(th => th.addEventListener("click", () => {
    const k = th.dataset.key;
    if (!k) return;
    if (state.sortKey === k) state.sortDir = state.sortDir === "desc" ? "asc" : "desc";
    else { state.sortKey = k; state.sortDir = "desc"; }
    renderLedger();
  }));

  // ---- render -------------------------------------------------------------
  function render() {
    renderUserOptions();
    if (!state.rows.length) { renderEmpty(); return; }
    els.emptyState.hidden = true;

    const totals = aggregateTotals(state.rows);
    renderHero(totals);
    renderForecast(state.rows);

    const byDay = aggregateByDay(state.rows);
    const byModel = aggregateBy(state.rows, "model");
    const byUser = aggregateBy(state.rows, "user");
    const byTool = aggregateBy(state.rows, "tool");

    renderLegend(byModel);
    renderBars(byDay, byModel);
    renderSpike(densify(byDay, state.rangeDays));
    renderHeatmap(byDay);
    renderClock();
    renderRank(els.modelRank, byModel, "model");
    renderRank(els.userRank, byUser, "none");
    renderRank(els.toolRank, byTool, "tool");
    renderLedger();
  }

  function renderEmpty() {
    els.emptyState.hidden = false;
    clear(els.totalCost);
    els.totalCost.appendChild(document.createTextNode("0"));
    els.totalCost.appendChild(el("span", { class: "cents" }, ".00"));
    [els.totalIn, els.totalOut, els.totalCw, els.totalCr, els.totalTok, els.totalMsgs].forEach(e => e.textContent = "—");
    clear(els.legend); clear(els.svg); clear(els.yaxis);
    clear(els.modelRank); clear(els.userRank); clear(els.toolRank); clear(els.ledgerBody);
    renderHeatmap([]);
    clear(els.clockHeatmap);
    els.heroSavings.textContent = "";
    clear(els.heroForecast);
    els.spikeBanner.hidden = true;
    els.emptyEndpoint.textContent = `${location.protocol}//${location.host}/ingest`;
  }

  function renderUserOptions() {
    // Merge registered users (some may have no data yet) with users-with-data,
    // then sort. Annotate the no-data ones so the dropdown hints at it.
    const withData = new Set(state.rows.map(r => r.user));
    const registered = new Set(state.users.map(u => u.user_id));
    const all = Array.from(new Set([...withData, ...registered])).sort();
    const cur = els.userSelect.value;
    clear(els.userSelect);
    els.userSelect.appendChild(el("option", { value: "" }, `all (${all.length})`));
    for (const u of all) {
      const label = withData.has(u) ? u : u + " · no data";
      els.userSelect.appendChild(el("option", { value: u }, label));
    }
    if (all.includes(cur)) els.userSelect.value = cur;
  }

  // ---- aggregation --------------------------------------------------------
  function aggregateTotals(rows) {
    let cost = 0, input = 0, output = 0, cw = 0, cr = 0, total = 0, msgs = 0;
    const modelSet = new Set();
    for (const r of rows) {
      cost   += r.cost_usd || 0;
      input  += r.input_tokens || 0;
      output += r.output_tokens || 0;
      cw     += r.cache_creation_tokens || 0;
      cr     += r.cache_read_tokens || 0;
      total  += r.total_tokens || 0;     // server-computed
      msgs   += r.messages || 0;
      modelSet.add(r.model);
    }
    return { cost, input, output, cw, cr, total, msgs, models: modelSet.size };
  }

  function aggregateByDay(rows) {
    const m = new Map();
    for (const r of rows) {
      if (!m.has(r.day)) {
        m.set(r.day, { day: r.day, byModel: {}, totalCost: 0, totalTokens: 0 });
      }
      const slot = m.get(r.day);
      const cost = r.cost_usd || 0;
      const tokens = (r.input_tokens || 0)
                   + (r.output_tokens || 0)
                   + (r.cache_creation_tokens || 0)
                   + (r.cache_read_tokens || 0);
      if (!slot.byModel[r.model]) slot.byModel[r.model] = { cost: 0, tokens: 0 };
      slot.byModel[r.model].cost   += cost;
      slot.byModel[r.model].tokens += tokens;
      slot.totalCost   += cost;
      slot.totalTokens += tokens;
    }
    return Array.from(m.values()).sort((a, b) => a.day.localeCompare(b.day));
  }
  // Picks the current metric off a per-day or per-(day,model) slot.
  // Centralised so the chart, tooltip, y-axis, and densify share one
  // notion of "what number are we plotting today".
  function metricTotal(day) {
    return state.metric === "tokens" ? day.totalTokens : day.totalCost;
  }
  function metricSlot(modelSlot) {
    if (!modelSlot) return 0;
    return state.metric === "tokens" ? modelSlot.tokens : modelSlot.cost;
  }
  // Formatter for the y-axis tick labels and the tooltip per-row value.
  function fmtMetric(v) {
    if (state.metric === "tokens") return fmtTokens(v);
    return "$" + (v >= 100 ? v.toFixed(0) : v.toFixed(2));
  }

  function aggregateBy(rows, key) {
    const m = new Map();
    for (const r of rows) {
      const k = r[key];
      if (!m.has(k)) m.set(k, { name: k, cost: 0, tokens: 0 });
      const slot = m.get(k);
      slot.cost   += r.cost_usd || 0;
      slot.tokens += (r.input_tokens || 0) + (r.output_tokens || 0);
    }
    return Array.from(m.values()).sort((a, b) => b.cost - a.cost);
  }

  // ---- hero ---------------------------------------------------------------
  function renderHero(t) {
    const [whole, frac] = t.cost.toFixed(2).split(".");
    clear(els.totalCost);
    els.totalCost.appendChild(document.createTextNode(Number(whole).toLocaleString("en-US")));
    els.totalCost.appendChild(el("span", { class: "cents" }, "." + frac));

    els.totalIn.textContent   = fmtTokens(t.input);
    els.totalOut.textContent  = fmtTokens(t.output);
    els.totalCw.textContent   = fmtTokens(t.cw);
    els.totalCr.textContent   = fmtTokens(t.cr);
    els.totalTok.textContent  = fmtTokens(t.total);
    els.totalMsgs.textContent = fmtInt(t.msgs);

    clear(els.heroSavings);
    if (t.cr > 0 && t.input > 0) {
      const ratio = t.cr / (t.input + t.cr);
      els.heroSavings.appendChild(el("strong", null, `${(ratio * 100).toFixed(0)}%`));
      els.heroSavings.appendChild(document.createTextNode(
        ` of read tokens came from cache · ≈${fmtTokens(t.cr * 0.9)} input-equivalents avoided`));
    }
  }

  // ---- legend -------------------------------------------------------------
  function renderLegend(models) {
    // Pick the same metric the chart is plotting so the legend's
    // percentages match the visual stack heights.
    const pick = (m) => state.metric === "tokens" ? m.tokens : m.cost;
    const sorted = [...models].sort((a, b) => pick(b) - pick(a));
    const total = sorted.reduce((s, m) => s + pick(m), 0);
    const top = sorted.slice(0, 6);
    clear(els.legend);
    for (const m of top) {
      const pct = total > 0 ? (pick(m) / total * 100).toFixed(0) : "0";
      const li = el("li", null, [
        el("span", { class: "swatch", style: `background:${colorFor(m.name)}` }),
        el("span", null, shortenModel(m.name)),
        el("span", { class: "pct" }, `${pct}%`),
      ]);
      els.legend.appendChild(li);
    }
    if (sorted.length > 6) {
      els.legend.appendChild(el("li", { class: "dim" }, `+ ${sorted.length - 6} more`));
    }
    // Marker for the trend overlay drawn by renderBars().
    els.legend.appendChild(el("li", { class: "ma-key" }, [
      el("span", { class: "swatch ma" }),
      el("span", null, `${MA_WINDOW}-day avg`),
    ]));
  }

  // ---- bars (SVG stacked) -------------------------------------------------
  function renderBars(byDay, byModel) {
    clear(els.svg); clear(els.yaxis);
    const W = els.svg.clientWidth || 800;
    const H = els.svg.clientHeight || 260;
    const padL = 44, padR = 8, padT = 14, padB = 26;
    const innerW = W - padL - padR;
    const innerH = H - padT - padB;
    if (!byDay.length) return;

    const dense = densify(byDay, state.rangeDays);
    const maxTotal = Math.max(...dense.map(d => metricTotal(d)), state.metric === "tokens" ? 1 : 0.01);
    const niceMax = niceCeil(maxTotal);

    // y grid lines + labels — three ticks (0 / mid / niceMax) is enough
    // to anchor the eye without crowding the card-head and without
    // making the top label collide with the title text. Mid and max
    // change with the data via niceCeil(maxTotal).
    const steps = 2;
    for (let i = 0; i <= steps; i++) {
      const v = niceMax * (i / steps);
      const div = el("div", { class: "gridline", style: `bottom:${(i / steps * 100)}%` });
      div.appendChild(el("span", null, fmtMetric(v)));
      els.yaxis.appendChild(div);
    }

    const barGap = 2;
    const bw = Math.max(2, (innerW / dense.length) - barGap);
    const orderedModels = byModel.map(m => m.name); // hot models bottom

    els.svg.setAttribute("viewBox", `0 0 ${W} ${H}`);

    // baseline
    els.svg.appendChild(svg("line", {
      x1: padL, x2: W - padR,
      y1: padT + innerH + 0.5, y2: padT + innerH + 0.5,
      stroke: "#2a3140", "stroke-width": "1",
    }));

    dense.forEach((day, idx) => {
      const x = padL + idx * (innerW / dense.length) + barGap / 2;
      let yCursor = padT + innerH;
      for (const model of orderedModels) {
        const v = metricSlot(day.byModel[model]);
        if (v <= 0) continue;
        const h = (v / niceMax) * innerH;
        const rect = svg("rect", {
          x: x.toFixed(2),
          y: (yCursor - h).toFixed(2),
          width: bw.toFixed(2),
          height: h.toFixed(2),
          fill: colorFor(model),
          class: "bar-segment",
        });
        rect.addEventListener("mouseenter", (e) => showTooltip(e, day));
        rect.addEventListener("mousemove",  (e) => moveTooltip(e));
        rect.addEventListener("mouseleave", hideTooltip);
        els.svg.appendChild(rect);
        yCursor -= h;
      }
      const N = dense.length > 30 ? 7 : (dense.length > 14 ? 3 : 2);
      if (idx % N === 0 || idx === dense.length - 1) {
        const t = svg("text", {
          x: (x + bw / 2).toFixed(2),
          y: H - 8,
          "text-anchor": "middle",
          class: "bar-tick",
        });
        t.textContent = formatTickDate(day.day);
        els.svg.appendChild(t);
      }
    });

    // 7-day trailing moving-average overlay — a dashed reference line so the
    // eye can separate trend from day-to-day noise. Uses metricTotal so it
    // follows the cost/tokens toggle, and the same niceMax scale as the bars
    // (an average never exceeds the max daily total, so it always fits).
    let acc = 0;
    const maPts = [];
    dense.forEach((day, idx) => {
      acc += metricTotal(day);
      if (idx >= MA_WINDOW) acc -= metricTotal(dense[idx - MA_WINDOW]);
      const avg = acc / Math.min(idx + 1, MA_WINDOW);
      const cx = padL + idx * (innerW / dense.length) + (innerW / dense.length) / 2;
      const cy = padT + innerH - (avg / niceMax) * innerH;
      maPts.push(`${cx.toFixed(2)},${cy.toFixed(2)}`);
    });
    if (maPts.length > 1) {
      els.svg.appendChild(svg("polyline", { points: maPts.join(" "), class: "ma-line" }));
    }
  }

  function densify(byDay, rangeDays) {
    if (rangeDays <= 0) return byDay;
    const out = [];
    const map = new Map(byDay.map(d => [d.day, d]));
    const today = new Date();
    today.setUTCHours(0, 0, 0, 0);
    for (let i = rangeDays - 1; i >= 0; i--) {
      const d = new Date(today);
      d.setUTCDate(today.getUTCDate() - i);
      const key = d.toISOString().slice(0, 10);
      out.push(map.get(key) || { day: key, byModel: {}, totalCost: 0, totalTokens: 0 });
    }
    return out;
  }

  // ---- trend: month-to-date projection + spike alert ----------------------
  // Month-to-date spend plus a linear run-rate projection for the current UTC
  // month. The rate uses COMPLETE days only (excludes today's partial bucket)
  // so the forecast isn't dragged down by a half-finished day. Only meaningful
  // when the loaded window reaches back to the 1st — renderForecast() guards.
  function mtdProjection(rows) {
    const now = new Date();
    const y = now.getUTCFullYear(), mo = now.getUTCMonth(), dom = now.getUTCDate();
    const monthStart = Date.UTC(y, mo, 1);
    const todayStart = Date.UTC(y, mo, dom);
    const daysInMonth = new Date(Date.UTC(y, mo + 1, 0)).getUTCDate();
    let mtd = 0, complete = 0;
    for (const r of rows) {
      const t = Date.parse(r.day + "T00:00:00Z");
      if (isNaN(t) || t < monthStart) continue;
      const c = r.cost_usd || 0;
      mtd += c;
      if (t < todayStart) complete += c;
    }
    const completeDays = dom - 1;
    const rate = completeDays > 0 ? complete / completeDays : mtd;
    return { mtd, projected: rate * daysInMonth, completeDays, daysInMonth };
  }

  function renderForecast(rows) {
    clear(els.heroForecast);
    // The projection is month-scoped, so it only holds if the loaded window
    // covers the whole month-to-date; otherwise mtd would be a partial sum.
    const dom = new Date().getUTCDate();
    const coversMonth = state.rangeDays === 0 || state.rangeDays >= dom;
    if (!coversMonth) {
      els.heroForecast.appendChild(
        el("span", { class: "dim" }, `select ≥${dom}d range for this month's forecast`));
      return;
    }
    const f = mtdProjection(rows);
    els.heroForecast.appendChild(el("span", { class: "fc-k" }, "month-to-date"));
    els.heroForecast.appendChild(el("span", { class: "fc-v" }, "$" + f.mtd.toFixed(2)));
    els.heroForecast.appendChild(el("span", { class: "fc-arrow" }, "→"));
    els.heroForecast.appendChild(el("span", { class: "fc-k" }, "projected"));
    els.heroForecast.appendChild(el("strong", { class: "fc-proj" }, "$" + f.projected.toFixed(2)));
    els.heroForecast.appendChild(
      el("span", { class: "dim fc-note" }, `· ${f.completeDays}/${f.daysInMonth}d`));
  }

  // Most-recent COMPLETE day whose metric exceeds `factor`× its trailing 7-day
  // average. Walks newest→oldest and needs ≥3 prior days to judge (avoids a
  // 1-day-vs-1-day false alarm near a range's left edge). `dense` must be the
  // densify() output so gaps count as zero rather than being skipped.
  function findSpike(dense, factor) {
    for (let i = dense.length - 2; i >= 0; i--) {   // -2 => skip today (partial)
      let sum = 0, cnt = 0;
      for (let j = Math.max(0, i - 7); j < i; j++) { sum += metricTotal(dense[j]); cnt++; }
      if (cnt < 3) break;
      const base = sum / cnt, v = metricTotal(dense[i]);
      if (base > 0 && v / base >= factor) return { day: dense[i].day, value: v, base, ratio: v / base };
    }
    return null;
  }

  function renderSpike(dense) {
    clear(els.spikeBanner);
    const s = findSpike(dense, SPIKE_FACTOR);
    if (!s) { els.spikeBanner.hidden = true; return; }
    els.spikeBanner.hidden = false;
    els.spikeBanner.appendChild(el("span", { class: "spike-icon" }, "▲"));
    els.spikeBanner.appendChild(el("span", { class: "spike-text" }, [
      el("strong", null, s.day),
      ` ${state.metric === "tokens" ? "usage" : "spend"} `,
      el("strong", null, fmtMetric(s.value)),
      " is ",
      el("strong", null, `${s.ratio.toFixed(1)}×`),
      ` the prior ${MA_WINDOW}-day average (${fmtMetric(s.base)})`,
    ]));
  }

  // ---- heatmap (GitHub-style year activity) -------------------------------
  // Always renders a 53-week × 7-day grid ending today (UTC), regardless of
  // the range picker. Cells outside state.rows just stay at level 0.
  const HEATMAP_COLORS = ["#1a212c", "#3a2a1d", "#7a3a1d", "#c64b1a", "#ff5c1a"];
  const HEATMAP_MONTHS = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
  const HEATMAP_DAY_LABELS = ["", "Mon", "", "Wed", "", "Fri", ""]; // Sun..Sat

  function heatmapLevel(v, max) {
    if (v <= 0 || max <= 0) return 0;
    const t = v / max;
    if (t < 0.25) return 1;
    if (t < 0.50) return 2;
    if (t < 0.75) return 3;
    return 4;
  }

  function renderHeatmap(byDay) {
    const svgEl = els.heatmap;
    clear(svgEl);
    const weeks = 53, days = 7;
    const cell = 12, gap = 3;
    const step = cell + gap;
    const padL = 30, padT = 18, padR = 4, padB = 4;
    const W = padL + weeks * step + padR;
    const H = padT + days * step + padB;
    svgEl.setAttribute("viewBox", `0 0 ${W} ${H}`);

    const byDayMap = new Map(byDay.map(d => [d.day, d.totalTokens]));

    // Color-scale anchor: use the 95th percentile of non-zero days so a
    // single outlier doesn't squash every other day into level 1.
    const vals = [...byDayMap.values()].filter(v => v > 0).sort((a, b) => a - b);
    const max = vals.length ? vals[Math.min(vals.length - 1, Math.floor(vals.length * 0.95))] : 1;

    const today = new Date();
    today.setUTCHours(0, 0, 0, 0);
    const todayDow = today.getUTCDay(); // 0 = Sun

    // Month labels: emit one when the top-of-column (Sunday) moves into a
    // new month. Only emit if there's room before the next label.
    let lastMonth = -1, lastLabelCol = -99;
    for (let col = 0; col < weeks; col++) {
      const daysBack = (weeks - 1 - col) * 7 + todayDow;
      const d = new Date(today);
      d.setUTCDate(today.getUTCDate() - daysBack);
      const m = d.getUTCMonth();
      if (m !== lastMonth && col - lastLabelCol >= 3) {
        lastMonth = m; lastLabelCol = col;
        const t = svg("text", {
          x: padL + col * step,
          y: padT - 6,
          class: "heatmap-month",
        });
        t.textContent = HEATMAP_MONTHS[m];
        svgEl.appendChild(t);
      } else if (m !== lastMonth) {
        lastMonth = m;
      }
    }

    // Day-of-week labels along the left.
    for (let row = 0; row < days; row++) {
      if (!HEATMAP_DAY_LABELS[row]) continue;
      const t = svg("text", {
        x: padL - 6,
        y: padT + row * step + cell - 2,
        "text-anchor": "end",
        class: "heatmap-day",
      });
      t.textContent = HEATMAP_DAY_LABELS[row];
      svgEl.appendChild(t);
    }

    // Cells.
    for (let col = 0; col < weeks; col++) {
      for (let row = 0; row < days; row++) {
        const daysBack = (weeks - 1 - col) * 7 + (todayDow - row);
        if (daysBack < 0) continue; // future days in current week
        const d = new Date(today);
        d.setUTCDate(today.getUTCDate() - daysBack);
        const key = d.toISOString().slice(0, 10);
        const v = byDayMap.get(key) || 0;
        const lvl = heatmapLevel(v, max);
        const rect = svg("rect", {
          x: padL + col * step,
          y: padT + row * step,
          width: cell, height: cell,
          rx: 2, ry: 2,
          fill: HEATMAP_COLORS[lvl],
          class: "heatmap-cell",
        });
        rect.addEventListener("mouseenter", (e) => showHeatmapTip(e, key, v));
        rect.addEventListener("mousemove", moveHeatmapTip);
        rect.addEventListener("mouseleave", hideHeatmapTip);
        svgEl.appendChild(rect);
      }
    }
  }

  function showHeatmapTip(e, iso, v) {
    const t = els.heatmapTip;
    clear(t);
    t.appendChild(el("div", { class: "tt-day" }, `${iso} (${weekday(iso)})`));
    t.appendChild(el("div", { class: "tt-total" }, [
      el("span", null, "TOKENS"),
      el("span", null, v > 0 ? fmtTokens(v) : "—"),
    ]));
    t.hidden = false;
    moveHeatmapTip(e);
  }
  function moveHeatmapTip(e) {
    const t = els.heatmapTip;
    const tw = t.offsetWidth, th = t.offsetHeight;
    let left = e.clientX - tw / 2;
    let top  = e.clientY - th - 10;
    const pad = 8;
    left = Math.max(pad, Math.min(left, window.innerWidth - tw - pad));
    if (top < pad) {
      top = e.clientY + 14;
      if (top + th > window.innerHeight - pad) top = window.innerHeight - th - pad;
    }
    t.style.left = left + "px";
    t.style.top  = top + "px";
  }
  function hideHeatmapTip() { els.heatmapTip.hidden = true; }

  // ---- rhythm heatmap (hour × weekday, local time) ------------------------
  // 7 rows (Sun..Sat, matching EXTRACT(DOW)) × 24 hour columns. Reuses the
  // calendar heatmap's 5-level colour scale and tooltip positioning; only the
  // grid geometry and data source (/clock, already in the browser's tz) differ.
  const CLOCK_DAY_NAMES = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

  function renderClock() {
    const svgEl = els.clockHeatmap;
    if (!svgEl) return;
    clear(svgEl);

    const cell = 14, gap = 3, step = cell + gap;
    const padL = 34, padT = 18, padR = 6, padB = 6;
    const W = padL + 24 * step + padR;
    const H = padT + 7 * step + padB;
    svgEl.setAttribute("viewBox", `0 0 ${W} ${H}`);

    const pick = (r) => state.metric === "tokens" ? (r.tokens || 0) : (r.cost_usd || 0);
    const grid = new Map();                 // dow*24 + hour -> row
    for (const r of state.clock) grid.set(r.dow * 24 + r.hour, r);

    // Same 95th-percentile anchor as the calendar heatmap so one busy hour
    // doesn't flatten everything else into level 1.
    const vals = state.clock.map(pick).filter(v => v > 0).sort((a, b) => a - b);
    const max = vals.length ? vals[Math.min(vals.length - 1, Math.floor(vals.length * 0.95))] : 1;

    // hour labels every 6h along the top
    for (let h = 0; h < 24; h += 6) {
      const t = svg("text", { x: padL + h * step, y: padT - 6, class: "heatmap-month" });
      t.textContent = String(h).padStart(2, "0");
      svgEl.appendChild(t);
    }
    // weekday labels down the left
    for (let row = 0; row < 7; row++) {
      const t = svg("text", {
        x: padL - 6, y: padT + row * step + cell - 3,
        "text-anchor": "end", class: "heatmap-day",
      });
      t.textContent = CLOCK_DAY_NAMES[row];
      svgEl.appendChild(t);
    }
    // cells
    for (let row = 0; row < 7; row++) {
      for (let h = 0; h < 24; h++) {
        const r = grid.get(row * 24 + h);
        const v = r ? pick(r) : 0;
        const rect = svg("rect", {
          x: padL + h * step, y: padT + row * step,
          width: cell, height: cell, rx: 2, ry: 2,
          fill: HEATMAP_COLORS[heatmapLevel(v, max)],
          class: "heatmap-cell",
        });
        const cap = r || { tokens: 0, cost_usd: 0, messages: 0 };
        rect.addEventListener("mouseenter", (e) => showClockTip(e, row, h, cap));
        rect.addEventListener("mousemove", moveClockTip);
        rect.addEventListener("mouseleave", hideClockTip);
        svgEl.appendChild(rect);
      }
    }
  }

  function showClockTip(e, dow, hour, r) {
    const t = els.clockTip;
    clear(t);
    t.appendChild(el("div", { class: "tt-day" }, `${CLOCK_DAY_NAMES[dow]} ${String(hour).padStart(2, "0")}:00`));
    t.appendChild(el("div", { class: "tt-total" }, [
      el("span", null, state.metric === "tokens" ? "TOKENS" : "USD"),
      el("span", null, state.metric === "tokens"
        ? (r.tokens > 0 ? fmtTokens(r.tokens) : "—")
        : (r.cost_usd > 0 ? "$" + r.cost_usd.toFixed(2) : "—")),
    ]));
    t.appendChild(el("div", { class: "tt-total" }, [
      el("span", null, "MSGS"),
      el("span", null, r.messages > 0 ? fmtInt(r.messages) : "—"),
    ]));
    t.hidden = false;
    moveClockTip(e);
  }
  function moveClockTip(e) {
    const t = els.clockTip;
    const tw = t.offsetWidth, th = t.offsetHeight;
    let left = e.clientX - tw / 2;
    let top  = e.clientY - th - 10;
    const pad = 8;
    left = Math.max(pad, Math.min(left, window.innerWidth - tw - pad));
    if (top < pad) {
      top = e.clientY + 14;
      if (top + th > window.innerHeight - pad) top = window.innerHeight - th - pad;
    }
    t.style.left = left + "px";
    t.style.top  = top + "px";
  }
  function hideClockTip() { els.clockTip.hidden = true; }

  function niceCeil(v) {
    if (v <= 0) return 1;
    const pow = Math.pow(10, Math.floor(Math.log10(v)));
    const n = v / pow;
    const nice = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
    return nice * pow;
  }

  function formatTickDate(iso) {
    const [, m, d] = iso.split("-");
    return `${m}·${d}`;
  }
  // Day-of-week label for the tooltip header. Forces UTC parsing
  // (`...T00:00:00Z`) so it lines up with the server's UTC day-bucket
  // — otherwise browsers near a DST boundary or in a positive-offset
  // timezone could shift one row's label.
  const WEEKDAYS = ["Sun","Mon","Tue","Wed","Thu","Fri","Sat"];
  function weekday(iso) {
    const d = new Date(iso + "T00:00:00Z");
    return WEEKDAYS[d.getUTCDay()];
  }

  // ---- tooltip ------------------------------------------------------------
  function showTooltip(e, day) {
    const t = els.tooltip;
    clear(t);
    t.appendChild(el("div", { class: "tt-day" }, `${day.day} (${weekday(day.day)})`));
    const entries = Object.entries(day.byModel)
      .map(([m, v]) => [m, metricSlot(v)])
      .filter(([, v]) => v > 0)
      .sort((a, b) => b[1] - a[1]);
    for (const [m, v] of entries) {
      t.appendChild(el("div", { class: "tt-row" }, [
        el("span", { class: "tt-sw", style: `background:${colorFor(m)}` }),
        el("span", { class: "tt-mdl" }, shortenModel(m)),
        el("span", { class: "tt-val" }, fmtMetric(v)),
      ]));
    }
    t.appendChild(el("div", { class: "tt-total" }, [
      el("span", null, "TOTAL"),
      el("span", null, fmtMetric(metricTotal(day))),
    ]));
    t.hidden = false;
    moveTooltip(e);
  }
  function moveTooltip(e) {
    const t = els.tooltip;
    const tw = t.offsetWidth;
    const th = t.offsetHeight;
    let left = e.clientX - tw / 2;
    let top  = e.clientY - th - 10;
    // Clamp to viewport so the tooltip is never clipped.
    const pad = 8;
    left = Math.max(pad, Math.min(left, window.innerWidth - tw - pad));
    if (top < pad) {
      top = e.clientY + 14;
      if (top + th > window.innerHeight - pad) top = window.innerHeight - th - pad;
    }
    t.style.left = left + "px";
    t.style.top  = top + "px";
  }
  function hideTooltip() { els.tooltip.hidden = true; }

  // ---- rankings -----------------------------------------------------------
  // mode: "model" | "tool" | "none" — controls swatch source and display text
  function renderRank(host, list, mode) {
    clear(host);
    const max = list.length ? list[0].cost : 1;
    list.slice(0, 8).forEach((item, i) => {
      const pct = max > 0 ? (item.cost / max * 100) : 0;
      const nameChildren = [];
      let display = item.name;
      let nameAttrs = { class: "rk-name" };
      if (mode === "model") {
        nameChildren.push(el("span", { class: "swatch", style: `background:${colorFor(item.name)}` }));
        display = shortenModel(item.name);
        nameAttrs["data-model"] = item.name;
        nameAttrs.title = "click for price history";
      } else if (mode === "tool") {
        nameChildren.push(el("span", { class: "swatch", style: `background:${colorForTool(item.name)}` }));
      }
      nameChildren.push(document.createTextNode(display));
      const li = el("li", null, [
        el("span", { class: "rk-num" }, String(i + 1).padStart(2, "0")),
        el("span", nameAttrs, nameChildren),
        el("span", { class: "rk-val" }, "$" + fmtUSD(item.cost)),
        el("span", { class: "rk-bar" }, el("i", { style: `width:${pct.toFixed(1)}%` })),
      ]);
      host.appendChild(li);
    });
  }

  // ---- ledger -------------------------------------------------------------
  function renderLedger() {
    let rows = state.rows.map(r => ({
      day: r.day, user: r.user, model: r.model,
      input:  r.input_tokens          || 0,
      output: r.output_tokens         || 0,
      cw:     r.cache_creation_tokens || 0,
      cr:     r.cache_read_tokens     || 0,
      total:  r.total_tokens          || 0,   // server-computed
      msgs:   r.messages              || 0,
      cost:   r.cost_usd              || 0,
    }));
    if (state.filter) {
      const q = state.filter;
      rows = rows.filter(r =>
        r.user.toLowerCase().includes(q) ||
        r.model.toLowerCase().includes(q) ||
        r.day.includes(q));
    }
    const dir = state.sortDir === "asc" ? 1 : -1;
    const k = state.sortKey;
    rows.sort((a, b) => {
      const av = a[k], bv = b[k];
      if (typeof av === "string") return av.localeCompare(bv) * dir;
      return (av - bv) * dir;
    });

    els.ledgerHead.forEach(th => {
      th.classList.toggle("is-sorted", th.dataset.key === k);
      th.classList.toggle("asc", th.dataset.key === k && state.sortDir === "asc");
    });

    clear(els.ledgerBody);
    const frag = document.createDocumentFragment();
    rows.slice(0, 500).forEach(r => {
      const tr = document.createElement("tr");
      tr.appendChild(el("td", null, r.day));
      tr.appendChild(el("td", null, r.user));
      const modelTd = el("td", { class: "model-cell", "data-model": r.model, title: "click for price history" }, [
        el("span", { class: "swatch", style: `background:${colorFor(r.model)}` }),
        document.createTextNode(shortenModel(r.model)),
      ]);
      tr.appendChild(modelTd);
      tr.appendChild(el("td", { class: "num" }, fmtTokens(r.input)));
      tr.appendChild(el("td", { class: "num" }, fmtTokens(r.output)));
      tr.appendChild(el("td", { class: "num" }, fmtTokens(r.cw)));
      tr.appendChild(el("td", { class: "num" }, fmtTokens(r.cr)));
      tr.appendChild(el("td", { class: "num cost-cell" }, fmtTokens(r.total)));
      tr.appendChild(el("td", { class: "num" }, fmtInt(r.msgs)));
      tr.appendChild(el("td", { class: "num cost-cell" }, "$" + (r.cost >= 100 ? fmtUSD(r.cost) : r.cost.toFixed(2))));
      frag.appendChild(tr);
    });
    els.ledgerBody.appendChild(frag);
  }

  // ---- price modal --------------------------------------------------------
  const PRICE_METRICS = [
    { key: "input_per_1m",          color: "#ff5c1a", label: "input"       },
    { key: "output_per_1m",         color: "#d96e9a", label: "output"      },
    { key: "cache_creation_per_1m", color: "#e8c47e", label: "cache write" },
    { key: "cache_read_per_1m",     color: "#76c7c0", label: "cache read"  },
  ];

  async function ensurePrices() {
    if (state.prices) return state.prices;
    const r = await fetch("/prices");
    if (!r.ok) throw new Error("HTTP " + r.status);
    state.prices = await r.json();
    return state.prices;
  }

  function bestPrefix(model, prices) {
    let best = null;
    const seen = new Set();
    for (const p of prices) {
      if (seen.has(p.model_prefix)) continue;
      seen.add(p.model_prefix);
      if (model.startsWith(p.model_prefix)) {
        if (!best || p.model_prefix.length > best.length) best = p.model_prefix;
      }
    }
    return best;
  }

  async function openPriceModal(model) {
    let prices;
    try {
      prices = await ensurePrices();
    } catch (e) {
      console.error("fetch /prices failed", e);
      return;
    }
    const prefix = bestPrefix(model, prices);
    if (!prefix) {
      // No known prefix — still show the modal with a "no data" hint.
      showModal(model, "(no pricing recorded)", []);
      return;
    }
    const history = prices
      .filter(p => p.model_prefix === prefix)
      .sort((a, b) => a.valid_from.localeCompare(b.valid_from));
    showModal(model, prefix, history);
  }

  function showModal(model, prefix, history) {
    els.priceModal.hidden = false;
    els.priceModal.setAttribute("aria-hidden", "false");
    clear(els.priceTitle);
    els.priceTitle.appendChild(document.createTextNode(model));
    const sub = el("span", { class: "dim", style: "font-size:14px; margin-left:10px" },
      "prefix: " + prefix);
    els.priceTitle.appendChild(sub);

    // legend doubles as the "current value" readout
    clear(els.priceLegend);
    const latest = history[history.length - 1];
    for (const m of PRICE_METRICS) {
      const children = [
        el("span", { class: "swatch", style: `background:${m.color}` }),
        el("span", null, m.label),
      ];
      if (latest) {
        children.push(el("span", { class: "pct" },
          "$" + latest[m.key].toFixed(2) + "/M"));
      }
      els.priceLegend.appendChild(el("li", null, children));
    }

    renderPriceChart(history);
  }

  function closePriceModal() {
    els.priceModal.hidden = true;
    els.priceModal.setAttribute("aria-hidden", "true");
  }

  function renderPriceChart(rows) {
    const svgEl = els.priceChart;
    clear(svgEl);
    if (rows.length === 0) return;

    const W = svgEl.clientWidth || 700;
    const H = svgEl.clientHeight || 240;
    const padL = 56, padR = 14, padT = 14, padB = 28;
    const innerW = W - padL - padR;
    const innerH = H - padT - padB;

    const t0 = new Date(rows[0].valid_from).getTime();
    let t1 = Date.now();
    for (const r of rows) {
      if (r.valid_to) t1 = Math.max(t1, new Date(r.valid_to).getTime());
    }
    if (t1 <= t0) t1 = t0 + 86400_000; // ensure non-zero span
    // Treat any valid_from earlier than 2020 as "since forever" — clamp so
    // the seed row (valid_from=1970-01-01) doesn't squash the whole chart.
    const clamped = t0 < new Date("2020-01-01").getTime();
    const tStart = clamped ? Math.max(t0, t1 - 365 * 86400_000) : t0;

    let maxV = 0;
    for (const r of rows) {
      for (const m of PRICE_METRICS) {
        if (r[m.key] > maxV) maxV = r[m.key];
      }
    }
    const niceMax = niceCeil(maxV);

    svgEl.setAttribute("viewBox", `0 0 ${W} ${H}`);
    const xOf = t => padL + (Math.max(t, tStart) - tStart) / (t1 - tStart) * innerW;
    const yOf = v => padT + innerH - (v / niceMax) * innerH;

    // y gridlines + labels — pick step count that keeps tick values "round".
    // niceMax is always 1/2/5 × 10^N, so 5 steps gives whole-number divisions
    // (e.g. 50 → 0/10/20/30/40/50, 20 → 0/4/8/…, 25 → 0/5/10/…).
    const steps = 5;
    for (let i = 0; i <= steps; i++) {
      const v = niceMax * (i / steps);
      const y = padT + innerH - (i / steps) * innerH;
      svgEl.appendChild(svg("line", {
        x1: padL, x2: W - padR, y1: y, y2: y,
        stroke: "#232a36", "stroke-dasharray": "3,3",
      }));
      const t = svg("text", {
        x: padL - 8, y: y + 3, "text-anchor": "end", class: "bar-tick",
      });
      t.textContent = formatPriceTick(v);
      svgEl.appendChild(t);
    }

    // Step lines per metric.
    for (const m of PRICE_METRICS) {
      let d = "";
      for (let i = 0; i < rows.length; i++) {
        const r = rows[i];
        const x1 = xOf(new Date(r.valid_from).getTime());
        const x2 = xOf(r.valid_to ? new Date(r.valid_to).getTime() : t1);
        const y = yOf(r[m.key]);
        if (i === 0) {
          d += `M ${x1} ${y} L ${x2} ${y}`;
        } else {
          const prevY = yOf(rows[i - 1][m.key]);
          d += ` L ${x1} ${prevY} L ${x1} ${y} L ${x2} ${y}`;
        }
      }
      svgEl.appendChild(svg("path", {
        d, stroke: m.color, fill: "none", "stroke-width": "2",
        "stroke-linecap": "square",
      }));
    }

    // x-axis labels: earliest visible date + "now" (or last valid_to).
    // If start and end fall on the same calendar day, only show one label
    // so we don't duplicate it.
    const startISO = new Date(tStart).toISOString().slice(0, 10);
    const endISO   = new Date(t1).toISOString().slice(0, 10);
    const sameDay  = startISO === endISO;
    const xLabels = sameDay
      ? [{ x: (padL + W - padR) / 2, t: startISO,                  anchor: "middle" }]
      : [
          { x: padL,     t: startISO, anchor: "start" },
          { x: W - padR, t: endISO,   anchor: "end"   },
        ];
    for (const lab of xLabels) {
      const t = svg("text", {
        x: lab.x, y: H - 8, "text-anchor": lab.anchor, class: "bar-tick",
      });
      t.textContent = lab.t;
      svgEl.appendChild(t);
    }
  }

  function formatPriceTick(v) {
    if (v === 0) return "$0";
    if (v >= 10) return "$" + v.toFixed(0);
    if (v >= 1)  return "$" + v.toFixed(1);
    return "$" + v.toFixed(2);
  }

  // close handlers — X, backdrop click, Escape
  els.priceClose.addEventListener("click", closePriceModal);
  els.priceModal.addEventListener("click", (e) => {
    if (e.target.classList.contains("modal-backdrop")) closePriceModal();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !els.priceModal.hidden) closePriceModal();
  });

  // Event delegation: any element carrying data-model triggers the modal.
  document.body.addEventListener("click", (e) => {
    const t = e.target.closest("[data-model]");
    if (!t) return;
    const m = t.getAttribute("data-model");
    if (m) openPriceModal(m);
  });

  // ---- stats modal (calendar-month bar chart) -----------------------------
  // Single accent for the monthly bars — reuse the "cost" signal colour.
  const STATS_COLOR = "#ff5c1a";
  const MONTH_ABBR = ["Jan","Feb","Mar","Apr","May","Jun",
                      "Jul","Aug","Sep","Oct","Nov","Dec"];
  function monthLabel(key) {                 // "2026-06" -> "Jun 2026"
    const [y, m] = key.split("-");
    return MONTH_ABBR[Number(m) - 1] + " " + y;
  }

  // Buckets the rows by natural (calendar) month. Independent of the range
  // picker — the modal always shows the full history for the current user.
  function aggregateByMonth(rows) {
    const m = new Map();
    for (const r of rows) {
      const key = (r.day || "").slice(0, 7); // YYYY-MM
      if (!key) continue;
      if (!m.has(key)) m.set(key, { month: key, cost: 0, tokens: 0, msgs: 0 });
      const s = m.get(key);
      s.cost   += r.cost_usd || 0;
      s.tokens += (r.input_tokens || 0) + (r.output_tokens || 0)
                + (r.cache_creation_tokens || 0) + (r.cache_read_tokens || 0);
      s.msgs   += r.messages || 0;
    }
    return Array.from(m.values()).sort((a, b) => a.month.localeCompare(b.month));
  }

  // All-time /summary for the current user filter (no `from` => full history).
  async function fetchAllTimeRows() {
    const params = new URLSearchParams();
    if (state.user) params.set("user", state.user);
    const url = "/summary" + (params.toString() ? "?" + params.toString() : "");
    const res = await fetch(url, { headers: { accept: "application/json" } });
    if (!res.ok) throw new Error("/summary HTTP " + res.status);
    return (await res.json()) || [];
  }

  async function openStatsModal() {
    els.statsModal.hidden = false;
    els.statsModal.setAttribute("aria-hidden", "false");
    let rows = [];
    try {
      rows = await fetchAllTimeRows();
    } catch (e) {
      console.error("stats fetch failed", e);
    }
    renderStats(aggregateByMonth(rows));
  }

  function closeStatsModal() {
    els.statsModal.hidden = true;
    els.statsModal.setAttribute("aria-hidden", "true");
  }

  function renderStats(months) {
    // legend doubles as a totals readout
    clear(els.statsLegend);
    const totalCost = months.reduce((s, m) => s + m.cost, 0);
    const totalTok  = months.reduce((s, m) => s + m.tokens, 0);
    els.statsLegend.appendChild(el("li", null, [
      el("span", { class: "swatch", style: `background:${STATS_COLOR}` }),
      el("span", null, "USD / calendar month"),
    ]));
    els.statsLegend.appendChild(el("li", { class: "dim" },
      `${months.length} mo · $${totalCost.toFixed(2)} · ${fmtTokens(totalTok)} tok`));

    const svgEl = els.statsChart;
    clear(svgEl);
    if (!months.length) return;

    const W = svgEl.clientWidth || 700;
    const H = svgEl.clientHeight || 240;
    const padL = 56, padR = 14, padT = 18, padB = 30;
    const innerW = W - padL - padR;
    const innerH = H - padT - padB;
    svgEl.setAttribute("viewBox", `0 0 ${W} ${H}`);

    const maxV = Math.max(...months.map(m => m.cost), 0.01);
    const niceMax = niceCeil(maxV);
    const yOf = v => padT + innerH - (v / niceMax) * innerH;

    // y gridlines + $ ticks
    const steps = 5;
    for (let i = 0; i <= steps; i++) {
      const v = niceMax * (i / steps);
      const y = padT + innerH - (i / steps) * innerH;
      svgEl.appendChild(svg("line", {
        x1: padL, x2: W - padR, y1: y, y2: y,
        stroke: "#232a36", "stroke-dasharray": "3,3",
      }));
      const t = svg("text", { x: padL - 8, y: y + 3, "text-anchor": "end", class: "bar-tick" });
      t.textContent = formatPriceTick(v);
      svgEl.appendChild(t);
    }

    // bars — one per calendar month
    const n = months.length;
    const slot = innerW / n;
    const bw = Math.min(slot * 0.62, 46);
    const labelEvery = Math.ceil(n / 12);   // thin x-labels when many months
    months.forEach((mo, i) => {
      const x = padL + slot * i + (slot - bw) / 2;
      const y = yOf(mo.cost);
      const h = Math.max(padT + innerH - y, 0);
      const rect = svg("rect", { x, y, width: bw, height: h, fill: STATS_COLOR, rx: 1 });
      const title = svg("title", {});
      title.textContent = `${mo.month}  $${mo.cost.toFixed(2)}  ${fmtTokens(mo.tokens)} tok`;
      rect.appendChild(title);
      svgEl.appendChild(rect);

      if (mo.cost > 0) {
        const vt = svg("text", {
          x: x + bw / 2, y: y - 5, "text-anchor": "middle", class: "bar-tick",
        });
        vt.textContent = "$" + (mo.cost >= 100 ? mo.cost.toFixed(0) : mo.cost.toFixed(1));
        svgEl.appendChild(vt);
      }
      if (i % labelEvery === 0 || i === n - 1) {
        const xt = svg("text", {
          x: x + bw / 2, y: H - 10, "text-anchor": "middle", class: "bar-tick",
        });
        xt.textContent = monthLabel(mo.month);
        svgEl.appendChild(xt);
      }
    });
  }

  els.statsBtn.addEventListener("click", openStatsModal);
  els.statsClose.addEventListener("click", closeStatsModal);
  els.statsModal.addEventListener("click", (e) => {
    if (e.target.classList.contains("modal-backdrop")) closeStatsModal();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !els.statsModal.hidden) closeStatsModal();
  });

  // ---- boot ---------------------------------------------------------------
  let resizeTimer = null;
  window.addEventListener("resize", () => {
    if (!state.rows.length) return;
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => {
      const byDay = aggregateByDay(state.rows);
      const byModel = aggregateBy(state.rows, "model");
      renderBars(byDay, byModel);
    }, 120);
  });

  setInterval(load, 30_000);
  load();
})();
