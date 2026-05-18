import {
  open,
  type Variable,
  type WMT,
} from "wmtiles";
import type {
  HeatmapRendererOptions,
  HeatmapRendererState,
  ParticlesRendererOptions,
  ParticlesRendererState,
  ArrowsRendererOptions,
  ArrowsRendererState,
  IsobarRendererOptions,
  IsobarRendererState,
  HatchRendererOptions,
  HatchRendererState,
  HatchPattern,
  HatchBand,
} from "wmtiles";
// side-effect: registers Leaflet + MapLibre backends for wmt.create*Layer.
import "wmtiles/leaflet";
import "wmtiles/maplibre";
import { installDebugHud } from "./debug";
import L from "leaflet";
import maplibregl from "maplibre-gl";

const $ = (id: string): HTMLElement => {
  const el = document.getElementById(id);
  if (!el) throw new Error(`missing #${id}`);
  return el;
};


// SVG sprites for hatch icon-fill, keyed by the "icon:<name>" dropdown values
const svgIcon = (body: string): string =>
  "data:image/svg+xml;utf8," +
  encodeURIComponent(
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">${body}</svg>`,
  );
const HATCH_ICONS: Record<string, string> = {
  warning: svgIcon(
    '<path d="M12 2 L22 21 H2 Z" fill="#ff5a3c" stroke="#7a1a08" ' +
      'stroke-width="1.5" stroke-linejoin="round"/>' +
      '<rect x="11" y="9" width="2" height="6" fill="#fff"/>' +
      '<rect x="11" y="17" width="2" height="2" fill="#fff"/>',
  ),
  dot: svgIcon('<circle cx="12" cy="12" r="7" fill="#ff5a5a"/>'),
};

type RendererName = "leaflet" | "maplibre";

function resolveRenderer(): RendererName {
  const fromUrl = new URLSearchParams(location.search).get("renderer");
  if (fromUrl === "maplibre" || fromUrl === "leaflet") return fromUrl;
  return "leaflet";
}

interface LayerWrap<S> {
  readonly state: S;
  setState(patch: Partial<S>): void;
  remove(): void;
}

interface IsobarWrap extends LayerWrap<IsobarRendererState> {
  setSpacing(s: number): void;
  setSmoothness(s: number): void;
  setFillEnabled(e: boolean): void;
  effectiveSpacing(): number;
}

type HeatmapOpts = HeatmapRendererOptions;
type ParticlesOpts = ParticlesRendererOptions & { uVar: string; vVar: string };
type ArrowsOpts = ArrowsRendererOptions & { uVar: string; vVar: string };
type IsobarOpts = IsobarRendererOptions & {
  variable: string;
  spacing: number;
  smoothness: number;
  fillEnabled: boolean;
};
type HatchOpts = HatchRendererOptions & { variable: string };

interface MarkerHandle {
  setLatLng(lat: number, lng: number): void;
  remove(): void;
}

interface Backend {
  readonly name: RendererName;
  getZoom(): number;
  onZoomEnd(cb: () => void): void;
  onClick(cb: (lat: number, lng: number) => void): void;
  openPopup(lat: number, lng: number, html: string): void;
  addMarker(lat: number, lng: number): MarkerHandle;
  addHeatmap(opts?: HeatmapOpts): LayerWrap<HeatmapRendererState>;
  addParticles(opts: ParticlesOpts): LayerWrap<ParticlesRendererState>;
  addArrows(opts: ArrowsOpts): LayerWrap<ArrowsRendererState>;
  addIsobar(opts: IsobarOpts): IsobarWrap;
  addHatch(opts: HatchOpts): LayerWrap<HatchRendererState>;
}

function makeLeafletBackend(wmt: WMT): Backend {
  const map = L.map("map", { worldCopyJump: true }).fitBounds([
    [wmt.bbox.south, wmt.bbox.west],
    [wmt.bbox.north, wmt.bbox.east],
  ]);
  L.tileLayer(
    "https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png",
    {
      attribution: "© OpenStreetMap, © CARTO",
      subdomains: "abcd",
      maxZoom: 19,
    },
  ).addTo(map);

  return {
    name: "leaflet",
    getZoom: () => map.getZoom(),
    onZoomEnd: (cb) => map.on("zoomend", cb),
    onClick: (cb) =>
      map.on("click", (ev: L.LeafletMouseEvent) =>
        cb(ev.latlng.lat, ev.latlng.lng),
      ),
    openPopup: (lat, lng, html) =>
      L.popup().setLatLng([lat, lng]).setContent(html).openOn(map),
    addMarker: (lat, lng) => {
      const icon = L.divIcon({
        className: "wmt-pin",
        html:
          '<div style="width:14px;height:14px;border-radius:50%;' +
          "background:#ff5a5a;border:2px solid #fff;" +
          'box-shadow:0 0 0 1px rgba(0,0,0,.5),0 2px 6px rgba(0,0,0,.6);"></div>',
        iconSize: [14, 14],
        iconAnchor: [7, 7],
      });
      const m = L.marker([lat, lng], { icon, interactive: false }).addTo(map);
      return {
        setLatLng: (la, lo) => m.setLatLng([la, lo]),
        remove: () => m.remove(),
      };
    },
    addHeatmap: (opts) => {
      const layer = wmt.createHeatmapLayer(opts).addTo(map);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
    addParticles: (opts) => {
      const layer = wmt.createParticlesLayer(opts).addTo(map);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
    addArrows: (opts) => {
      const layer = wmt.createArrowsLayer(opts).addTo(map);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
    addIsobar: (opts) => {
      const layer = wmt.createIsobarLayer(opts).addTo(map);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
        setSpacing: (s) => layer.setSpacing(s),
        setSmoothness: (s) => layer.setSmoothness(s),
        setFillEnabled: (e) => layer.setFillEnabled(e),
        effectiveSpacing: () => layer.effectiveSpacing(),
      };
    },
    addHatch: (opts) => {
      const layer = wmt.createHatchLayer(opts).addTo(map);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
  };
}

function makeMapLibreBackend(wmt: WMT): Backend {
  const style: maplibregl.StyleSpecification = {
    version: 8,
    sources: {
      carto: {
        type: "raster",
        tiles: [
          "https://a.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png",
          "https://b.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png",
          "https://c.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png",
          "https://d.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png",
        ],
        tileSize: 256,
        attribution: "© OpenStreetMap, © CARTO",
      },
    },
    layers: [{ id: "carto", type: "raster", source: "carto" }],
  };

  const map = new maplibregl.Map({
    container: "map",
    style,
    bounds: [
      [wmt.bbox.west, wmt.bbox.south],
      [wmt.bbox.east, wmt.bbox.north],
    ],
    fitBoundsOptions: { padding: 0, animate: false },
    attributionControl: { compact: true },
  });

  const ready = new Promise<void>((resolve) => {
    if (map.loaded()) resolve();
    else map.once("load", () => resolve());
  });

  function addWhenReady<T extends { addTo: (m: maplibregl.Map) => unknown }>(
    layer: T,
  ): T {
    ready.then(() => {
      map.setProjection({ type: "globe" });
      layer.addTo(map);
    });
    return layer;
  }

  return {
    name: "maplibre",
    getZoom: () => map.getZoom(),
    onZoomEnd: (cb) => map.on("zoomend", cb),
    onClick: (cb) =>
      map.on("click", (ev) => cb(ev.lngLat.lat, ev.lngLat.lng)),
    openPopup: (lat, lng, html) => {
      new maplibregl.Popup({ closeButton: true })
        .setLngLat([lng, lat])
        .setHTML(html)
        .addTo(map);
    },
    addMarker: (lat, lng) => {
      const dot = document.createElement("div");
      dot.style.cssText =
        "width:14px;height:14px;border-radius:50%;" +
        "background:#ff5a5a;border:2px solid #fff;" +
        "box-shadow:0 0 0 1px rgba(0,0,0,.5),0 2px 6px rgba(0,0,0,.6);";
      const m = new maplibregl.Marker({ element: dot, anchor: "center" })
        .setLngLat([lng, lat])
        .addTo(map);
      return {
        setLatLng: (la, lo) => m.setLngLat([lo, la]),
        remove: () => m.remove(),
      };
    },
    addHeatmap: (opts) => {
      const layer = wmt.createHeatmapLayer(opts);
      addWhenReady(layer);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
    addParticles: (opts) => {
      const layer = wmt.createParticlesLayer(opts);
      addWhenReady(layer);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
    addArrows: (opts) => {
      const layer = wmt.createArrowsLayer(opts);
      addWhenReady(layer);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
    addIsobar: (opts) => {
      const layer = wmt.createIsobarLayer(opts);
      addWhenReady(layer);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
        setSpacing: (s) => layer.setSpacing(s),
        setSmoothness: (s) => layer.setSmoothness(s),
        setFillEnabled: (e) => layer.setFillEnabled(e),
        effectiveSpacing: () => layer.effectiveSpacing(),
      };
    },
    addHatch: (opts) => {
      const layer = wmt.createHatchLayer(opts);
      addWhenReady(layer);
      return {
        get state() { return layer.state; },
        setState: (p) => layer.setState(p),
        remove: () => layer.remove(),
      };
    },
  };
}

function splitVarName(name: string): { param: string; level: string } {
  const i = name.indexOf("_");
  if (i < 0) return { param: name, level: "" };
  return { param: name.substring(0, i), level: name.substring(i + 1) };
}

function levelSortKey(level: string): [number, number, string] {
  if (level === "") return [0, 0, ""];
  const m = level.match(/^(-?\d+)/);
  if (m) return [1, +m[1], level];
  return [2, 0, level];
}

interface VarGroup {
  level: string;
  variable: Variable;
}

function ensureLegendEl(): HTMLElement {
  let el = document.getElementById("legend-floating");
  if (el) return el;
  el = document.createElement("div");
  el.id = "legend-floating";
  el.innerHTML =
    '<div id="legTitle"></div><div class="bar"></div>' +
    '<div class="row"><span id="legMin"></span><span id="legMax"></span></div>';
  document.body.appendChild(el);
  return el;
}

function formatTimestamp(d: Date): string {
  return d.toISOString().replace("T", " ").slice(0, 16) + "Z";
}

function buildSparkline(
  series: Float32Array,
  cursorIdx: number,
  width: number,
  height: number,
): string {
  const W = width;
  const H = height;
  const pad = 2;
  const N = series.length;
  if (N === 0) return "";
  let lo = +Infinity;
  let hi = -Infinity;
  for (let i = 0; i < N; i++) {
    const v = series[i];
    if (!Number.isFinite(v)) continue;
    if (v < lo) lo = v;
    if (v > hi) hi = v;
  }
  if (!Number.isFinite(lo) || !Number.isFinite(hi)) {
    return `<svg class="fc-spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">` +
      `<text x="${W / 2}" y="${H / 2 + 3}" text-anchor="middle">no data</text></svg>`;
  }
  // Flat series: draw a single horizontal line through the middle.
  if (hi - lo < 1e-12) {
    hi = lo + 1;
    lo = lo - 0;
  }
  const span = hi - lo;
  const xOf = (i: number): number =>
    N === 1 ? W / 2 : pad + (i / (N - 1)) * (W - 2 * pad);
  const yOf = (v: number): number =>
    H - pad - ((v - lo) / span) * (H - 2 * pad);

  let line = "";
  let area = "";
  let first = -1;
  let last = -1;
  for (let i = 0; i < N; i++) {
    const v = series[i];
    if (!Number.isFinite(v)) continue;
    const x = xOf(i);
    const y = yOf(v);
    line += line === "" ? `M${x.toFixed(1)} ${y.toFixed(1)}` : ` L${x.toFixed(1)} ${y.toFixed(1)}`;
    if (first < 0) first = i;
    last = i;
  }
  if (first >= 0 && last >= 0) {
    area = `M${xOf(first).toFixed(1)} ${(H - pad).toFixed(1)} ` +
      line.replace(/^M/, "L") +
      ` L${xOf(last).toFixed(1)} ${(H - pad).toFixed(1)} Z`;
  }

  const ci = Math.max(0, Math.min(N - 1, Math.round(cursorIdx)));
  const cx = xOf(ci);
  const cv = series[ci];
  const cursorLine = `<line class="cursor" x1="${cx.toFixed(1)}" y1="0" x2="${cx.toFixed(1)}" y2="${H}"/>`;
  const cursorDot = Number.isFinite(cv)
    ? `<circle class="dot" cx="${cx.toFixed(1)}" cy="${yOf(cv).toFixed(1)}" r="2.2"/>`
    : "";

  return (
    `<svg class="fc-spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">` +
    `<path class="area" d="${area}"/>` +
    `<path class="line" d="${line}"/>` +
    cursorLine + cursorDot +
    `<text x="2" y="${H - 1}">${lo.toPrecision(3)}</text>` +
    `<text x="${W - 2}" y="${H - 1}" text-anchor="end">${hi.toPrecision(3)}</text>` +
    `</svg>`
  );
}

interface ForecastViewer {
  show(lat: number, lng: number): void;
  onTimeChange(): void;
}

function installForecastViewer(
  wmt: WMT,
  backend: Backend,
  currentStep: () => number,
): ForecastViewer {
  const panel = $("forecast");
  const body = $("fcBody");
  const coordEl = $("fcCoord");
  const whenEl = $("fcWhen");
  const statusEl = $("fcStatus");
  const closeBtn = $("fcClose") as HTMLButtonElement;

  let marker: MarkerHandle | null = null;
  // Times + per-variable series for the active location. Cleared on new click.
  let times: readonly Date[] | null = null;
  let valuesByVar: Record<string, Float32Array> | null = null;
  // Generation token so a stale fetch never paints over a newer one.
  let gen = 0;

  function close(): void {
    panel.classList.add("hidden");
    if (marker) {
      marker.remove();
      marker = null;
    }
    times = null;
    valuesByVar = null;
  }

  closeBtn.onclick = close;

  function renderRows(): void {
    if (!times || !valuesByVar) return;
    const step = currentStep();
    const N = times.length;
    const stepF = Math.max(0, Math.min(N - 1, Math.floor(step)));
    const stepC = Math.min(N - 1, stepF + 1);
    const frac = step - stepF;

    // Group by parameter (e.g. "t_2m" → param "t", level "2m") for compact display.
    const groups = new Map<string, VarGroup[]>();
    for (const v of wmt.variables) {
      const { param, level } = splitVarName(v.name);
      if (!groups.has(param)) groups.set(param, []);
      groups.get(param)!.push({ level, variable: v });
    }
    for (const list of groups.values()) {
      list.sort((a, b) => {
        const ka = levelSortKey(a.level);
        const kb = levelSortKey(b.level);
        return ka[0] - kb[0] || ka[1] - kb[1] || ka[2].localeCompare(kb[2]);
      });
    }
    const paramNames = [...groups.keys()].sort((a, b) => a.localeCompare(b));

    let html = "";
    for (const p of paramNames) {
      const list = groups.get(p)!;
      const unit = list[0].variable.unit;
      html += `<div class="fc-group">`;
      html += `<div class="fc-group-name">${escapeHtml(p)}${
        unit ? ` <span style="color:#666;font-weight:400">(${escapeHtml(unit)})</span>` : ""
      }</div>`;
      for (const { level, variable } of list) {
        const series = valuesByVar[variable.name];
        let valueText: string;
        let cls = "fc-value";
        if (!series) {
          valueText = "—";
          cls += " nan";
        } else {
          // Interpolate so scrubbing between integer steps still shows movement.
          const a = series[stepF];
          const b = series[stepC];
          let v: number;
          if (!Number.isFinite(a) && !Number.isFinite(b)) v = NaN;
          else if (!Number.isFinite(a)) v = b;
          else if (!Number.isFinite(b)) v = a;
          else v = a + (b - a) * frac;
          if (!Number.isFinite(v)) {
            valueText = "NaN";
            cls += " nan";
          } else {
            valueText = `${formatNumberCompact(v)}${
              variable.unit ? `<span class="fc-unit">${escapeHtml(variable.unit)}</span>` : ""
            }`;
          }
        }
        const spark = series
          ? buildSparkline(series, step, 320, 28)
          : "";
        html += `<div class="fc-row">`;
        html += `<div class="fc-level">${escapeHtml(level || "—")}</div>`;
        html += `<div class="${cls}">${valueText}</div>`;
        html += spark;
        html += `</div>`;
      }
      html += `</div>`;
    }
    body.innerHTML = html;
  }

  async function show(lat: number, lng: number): Promise<void> {
    if (!Number.isFinite(lat) || !Number.isFinite(lng)) return;
    const myGen = ++gen;

    panel.classList.remove("hidden");
    coordEl.textContent = `${lat.toFixed(4)}, ${lng.toFixed(4)}`;
    whenEl.textContent = "";
    body.innerHTML = `<div class="fc-empty">loading ${wmt.variables.length} variables × ${wmt.timeStepCount} steps…</div>`;
    statusEl.textContent = "fetching…";

    if (marker) marker.setLatLng(lat, lng);
    else marker = backend.addMarker(lat, lng);

    const names = wmt.variables.map((v) => v.name);
    const t0 = performance.now();
    // filter to /wmt so basemap tiles and stylesheets don't inflate the count.
    let fetchCount = 0;
    let observer: PerformanceObserver | null = null;
    const isWmtUrl = (n: string): boolean =>
      n.includes("/wmt") && !n.endsWith(".js");
    if (typeof PerformanceObserver !== "undefined") {
      observer = new PerformanceObserver((list) => {
        for (const e of list.getEntries()) {
          if (isWmtUrl(e.name)) fetchCount++;
        }
      });
      try {
        observer.observe({ type: "resource", buffered: false });
      } catch {
        observer = null;
      }
    }
    let res: { times: readonly Date[]; values: Readonly<Record<string, Float32Array>> };
    try {
      res = await wmt.forecast({ lat, lon: lng, variables: names });
    } catch (err) {
      if (observer) observer.disconnect();
      if (myGen !== gen) return;
      const msg = err instanceof Error ? err.message : String(err);
      body.innerHTML = `<div class="fc-empty">error: ${escapeHtml(msg)}</div>`;
      statusEl.textContent = "";
      return;
    }
    if (observer) {
      for (const e of observer.takeRecords()) {
        if (isWmtUrl(e.name)) fetchCount++;
      }
      observer.disconnect();
    }
    if (myGen !== gen) return;
    const dt = (performance.now() - t0) | 0;

    times = res.times;
    valuesByVar = res.values as Record<string, Float32Array>;
    const reqText = fetchCount > 0 ? ` · ${fetchCount} req` : "";
    statusEl.textContent =
      `${names.length} vars · ${res.times.length} steps · ${dt}ms${reqText}`;
    renderRows();
    updateWhenLabel();
  }

  function updateWhenLabel(): void {
    if (!times || times.length === 0) {
      whenEl.textContent = "";
      return;
    }
    const step = currentStep();
    const N = times.length;
    const stepF = Math.max(0, Math.min(N - 1, Math.floor(step)));
    const stepC = Math.min(N - 1, stepF + 1);
    const frac = step - stepF;
    const ms = times[stepF].getTime() +
      (times[stepC].getTime() - times[stepF].getTime()) * frac;
    whenEl.textContent = formatTimestamp(new Date(ms));
  }

  function onTimeChange(): void {
    if (!times || !valuesByVar) return;
    updateWhenLabel();
    renderRows();
  }

  return {
    show: (lat, lng) => {
      void show(lat, lng);
    },
    onTimeChange,
  };
}

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function formatNumberCompact(v: number): string {
  const abs = Math.abs(v);
  if (abs === 0) return "0";
  if (abs >= 1000 || abs < 0.01) return v.toExponential(2);
  if (abs >= 100) return v.toFixed(1);
  if (abs >= 10) return v.toFixed(2);
  return v.toFixed(3);
}

async function boot(): Promise<void> {
  const status = $("status");
  status.textContent = "fetching header…";

  const rendererName = resolveRenderer();
  const rendererSel = $("renderer") as HTMLSelectElement;
  rendererSel.value = rendererName;
  rendererSel.onchange = () => {
    const url = new URL(location.href);
    url.searchParams.set("renderer", rendererSel.value);
    location.assign(url.toString());
  };

  installDebugHud();

  const wmt = await open("/wmt");

  const backend: Backend =
    rendererName === "maplibre"
      ? makeMapLibreBackend(wmt)
      : makeLeafletBackend(wmt);

  const urlFmt = new URLSearchParams(location.search).get("fmt");
  const tileTextureFormat =
    urlFmt === "r32f" || urlFmt === "r16f" || urlFmt === "auto"
      ? (urlFmt as "r32f" | "r16f" | "auto")
      : undefined;
  let layer = backend.addHeatmap({ tileTextureFormat });
  let scalarOn = true;

  const groups = new Map<string, VarGroup[]>();
  for (const v of wmt.variables) {
    const { param, level } = splitVarName(v.name);
    if (!groups.has(param)) groups.set(param, []);
    groups.get(param)!.push({ level, variable: v });
  }
  for (const list of groups.values()) {
    list.sort((a, b) => {
      const ka = levelSortKey(a.level);
      const kb = levelSortKey(b.level);
      return ka[0] - kb[0] || ka[1] - kb[1] || ka[2].localeCompare(kb[2]);
    });
  }
  const paramNames = [...groups.keys()].sort((a, b) => a.localeCompare(b));

  const paramSel = $("param") as HTMLSelectElement;
  paramSel.innerHTML = "";
  for (const p of paramNames) {
    const unit = groups.get(p)![0].variable.unit || "?";
    const o = document.createElement("option");
    o.value = p;
    o.textContent = `${p} (${unit})`;
    paramSel.appendChild(o);
  }
  paramSel.disabled = false;

  const heightSel = $("height") as HTMLSelectElement;
  heightSel.disabled = false;

  const time = $("time") as HTMLInputElement;
  time.max = String(Math.max(0, wmt.timeStepCount - 1));
  time.disabled = false;

  const vminEl = $("vmin") as HTMLInputElement;
  const vmaxEl = $("vmax") as HTMLInputElement;
  vminEl.disabled = false;
  vmaxEl.disabled = false;

  const windUSel = $("windU") as HTMLSelectElement;
  const windVSel = $("windV") as HTMLSelectElement;
  const sortedVars = [...wmt.variables].sort((a, b) =>
    a.name.localeCompare(b.name),
  );
  for (const sel of [windUSel, windVSel]) {
    for (const v of sortedVars) {
      const o = document.createElement("option");
      o.value = v.name;
      o.textContent = v.name;
      sel.appendChild(o);
    }
    sel.disabled = false;
  }

  let windLayer: LayerWrap<ParticlesRendererState> | null = null;
  function rebuildWindLayer(): void {
    const uName = windUSel.value;
    const vName = windVSel.value;
    if (windLayer) {
      windLayer.remove();
      windLayer = null;
    }
    if (!uName || !vName) return;
    if (uName === vName) {
      console.warn("wmtiles: u and v must differ");
      return;
    }
    try {
      windLayer = backend.addParticles({
        uVar: uName,
        vVar: vName,
        colormap: "white",
        particleSize: 2.5,
        speedFactor: 0.0005,
        tileTextureFormat,
      });
      windLayer.setState({ t: +time.value });
    } catch (err) {
      console.error("wmtiles: wind overlay failed", err);
    }
  }
  windUSel.onchange = rebuildWindLayer;
  windVSel.onchange = rebuildWindLayer;

  const arrowUSel = $("arrowU") as HTMLSelectElement;
  const arrowVSel = $("arrowV") as HTMLSelectElement;
  const arrowStyleSel = $("arrowStyle") as HTMLSelectElement;
  for (const sel of [arrowUSel, arrowVSel]) {
    for (const v of sortedVars) {
      const o = document.createElement("option");
      o.value = v.name;
      o.textContent = v.name;
      sel.appendChild(o);
    }
    sel.disabled = false;
  }

  let arrowLayer: LayerWrap<ArrowsRendererState> | null = null;
  function rebuildArrowLayer(): void {
    const uName = arrowUSel.value;
    const vName = arrowVSel.value;
    if (arrowLayer) {
      arrowLayer.remove();
      arrowLayer = null;
    }
    if (!uName || !vName) return;
    if (uName === vName) {
      console.warn("wmtiles: arrow u and v must differ");
      return;
    }
    try {
      arrowLayer = backend.addArrows({
        uVar: uName,
        vVar: vName,
        // style is compiled into the renderer, so switching it rebuilds
        style: arrowStyleSel.value as "arrow" | "barb",
        colormap: "viridis",
        tileTextureFormat,
      });
      arrowLayer.setState({ t: +time.value });
    } catch (err) {
      console.error("wmtiles: arrow overlay failed", err);
    }
  }
  arrowUSel.onchange = rebuildArrowLayer;
  arrowVSel.onchange = rebuildArrowLayer;
  arrowStyleSel.onchange = rebuildArrowLayer;

  const isoVarSel = $("isoVar") as HTMLSelectElement;
  const isoSpacingEl = $("isoSpacing") as HTMLInputElement;
  const isoUnitEl = $("isoUnit");
  const isoSmoothEl = $("isoSmooth") as HTMLInputElement;
  const isoSmoothLabelEl = $("isoSmoothLabel");
  const isoFillEl = $("isoFill") as HTMLInputElement;
  for (const v of sortedVars) {
    const o = document.createElement("option");
    o.value = v.name;
    o.textContent = v.name;
    isoVarSel.appendChild(o);
  }
  isoVarSel.disabled = false;
  isoSpacingEl.disabled = false;
  isoSmoothEl.disabled = false;
  isoFillEl.disabled = false;

  function niceSpacing(raw: number): number {
    if (!(raw > 0) || !Number.isFinite(raw)) return 1;
    const exp = Math.floor(Math.log10(raw));
    const base = Math.pow(10, exp);
    const r = raw / base;
    let mult = 1;
    if (r >= 5) mult = 5;
    else if (r >= 2) mult = 2;
    return mult * base;
  }

  function suggestSpacing(v: Variable): number {
    const lo = Number.isFinite(v.range.min) ? v.range.min : 0;
    const hi = Number.isFinite(v.range.max) ? v.range.max : 1;
    const span = Math.max(hi - lo, 1e-6);
    return niceSpacing(span / 25);
  }

  let isoLayer: IsobarWrap | null = null;
  function updateIsoHint(): void {
    if (!isoLayer) {
      isoUnitEl.textContent = "";
      return;
    }
    const unit = isoLayer.state.variable.unit || "";
    const eff = isoLayer.effectiveSpacing();
    const base = +isoSpacingEl.value;
    isoUnitEl.textContent =
      eff !== base
        ? `${unit} (eff ${eff})`
        : unit;
  }
  function rebuildIsobarLayer(): void {
    const name = isoVarSel.value;
    if (isoLayer) {
      isoLayer.remove();
      isoLayer = null;
    }
    isoUnitEl.textContent = "";
    if (!name) return;
    const v = wmt.variable(name);
    isoUnitEl.textContent = v.unit ? v.unit : "";
    const spacing = +isoSpacingEl.value;
    if (!Number.isFinite(spacing) || spacing <= 0) {
      console.warn("wmtiles: invalid isobar spacing");
      return;
    }
    try {
      isoLayer = backend.addIsobar({
        variable: name,
        spacing,
        smoothness: +isoSmoothEl.value,
        fillEnabled: isoFillEl.checked,
        tileTextureFormat,
      });
      isoLayer.setState({ t: +time.value });
      updateIsoHint();
    } catch (err) {
      console.error("wmtiles: isobar overlay failed", err);
    }
  }
  backend.onZoomEnd(updateIsoHint);

  isoSmoothEl.oninput = () => {
    const s = +isoSmoothEl.value;
    isoSmoothLabelEl.textContent = String(s);
    if (isoLayer) isoLayer.setSmoothness(s);
  };
  isoFillEl.onchange = () => {
    if (isoLayer) isoLayer.setFillEnabled(isoFillEl.checked);
  };
  isoVarSel.onchange = () => {
    const name = isoVarSel.value;
    if (name) {
      const v = wmt.variable(name);
      isoSpacingEl.value = String(suggestSpacing(v));
    }
    rebuildIsobarLayer();
  };
  isoSpacingEl.onchange = () => {
    if (isoLayer) {
      const s = +isoSpacingEl.value;
      if (Number.isFinite(s) && s > 0) {
        isoLayer.setSpacing(s);
        updateIsoHint();
      }
    } else {
      rebuildIsobarLayer();
    }
  };

  const hatchVarSel = $("hatchVar") as HTMLSelectElement;
  const hatchModeSel = $("hatchMode") as HTMLSelectElement;
  const hatchThresholdEl = $("hatchThreshold") as HTMLInputElement;
  const hatchPatternSel = $("hatchPattern") as HTMLSelectElement;
  const hatchIconFileEl = $("hatchIconFile") as HTMLInputElement;
  const hatchLockToMapEl = $("hatchLockToMap") as HTMLInputElement;
  // backs the "icon:upload" pattern option
  let uploadedIconUrl: string | null = null;
  for (const v of sortedVars) {
    const o = document.createElement("option");
    o.value = v.name;
    o.textContent = v.name;
    hatchVarSel.appendChild(o);
  }
  hatchVarSel.disabled = false;
  hatchThresholdEl.disabled = false;

  let hatchLayer: LayerWrap<HatchRendererState> | null = null;
  function rebuildHatchLayer(): void {
    const name = hatchVarSel.value;
    if (hatchLayer) {
      hatchLayer.remove();
      hatchLayer = null;
    }
    if (!name) return;
    const threshold = +hatchThresholdEl.value;
    const t = Number.isFinite(threshold) ? threshold : 0;
    const range: [number, number] =
      hatchModeSel.value === "below" ? [-Infinity, t] : [t, Infinity];
    const pat = hatchPatternSel.value;
    let band: HatchBand;
    if (pat.startsWith("icon:")) {
      const key = pat.slice(5);
      const iconUrl = key === "upload" ? uploadedIconUrl : HATCH_ICONS[key];
      if (!iconUrl) return; // "upload" picked but no file chosen yet
      band = { range, icon: iconUrl, spacing: 30, iconSize: 22 };
    } else {
      band = { range, pattern: pat as HatchPattern, color: [255, 90, 90] };
    }
    try {
      // bands and lockToMap are baked into the shader, so any change rebuilds
      hatchLayer = backend.addHatch({
        variable: name,
        bands: [band],
        lockToMap: hatchLockToMapEl.checked,
        tileTextureFormat,
      });
      hatchLayer.setState({ t: +time.value });
    } catch (err) {
      console.error("wmtiles: hatch overlay failed", err);
    }
  }
  hatchVarSel.onchange = rebuildHatchLayer;
  hatchModeSel.onchange = rebuildHatchLayer;
  hatchThresholdEl.onchange = rebuildHatchLayer;
  hatchPatternSel.onchange = rebuildHatchLayer;
  hatchLockToMapEl.onchange = rebuildHatchLayer;
  hatchIconFileEl.onchange = () => {
    const file = hatchIconFileEl.files?.[0];
    if (!file) return;
    if (uploadedIconUrl) URL.revokeObjectURL(uploadedIconUrl);
    uploadedIconUrl = URL.createObjectURL(file);
    hatchPatternSel.value = "icon:upload";
    rebuildHatchLayer();
  };

  ensureLegendEl();

  const scalarEnabledEl = $("scalarEnabled") as HTMLInputElement;
  scalarEnabledEl.disabled = false;
  scalarEnabledEl.addEventListener("click", (e) => e.stopPropagation());
  scalarEnabledEl.onchange = () => {
    const wantOn = scalarEnabledEl.checked;
    const legendEl = document.getElementById("legend-floating");
    if (wantOn && !scalarOn) {
      layer = backend.addHeatmap({ tileTextureFormat });
      scalarOn = true;
      if (legendEl) legendEl.style.display = "";
      refresh();
    } else if (!wantOn && scalarOn) {
      layer.remove();
      if (legendEl) legendEl.style.display = "none";
      scalarOn = false;
    }
  };

  function currentVar(): Variable | null {
    const list = groups.get(paramSel.value);
    if (!list) return null;
    const entry =
      list.find((e) => e.level === heightSel.value) ?? list[0];
    return entry.variable;
  }

  function populateHeights(): void {
    const prev = heightSel.value;
    const list = groups.get(paramSel.value) ?? [];
    heightSel.innerHTML = "";
    for (const { level } of list) {
      const o = document.createElement("option");
      o.value = level;
      o.textContent = level === "" ? "n/a" : level;
      heightSel.appendChild(o);
    }
    const keep = list.some((e) => e.level === prev)
      ? prev
      : list[0]?.level ?? "";
    heightSel.value = keep;
  }

  function applyRangeForVar(): void {
    const v = currentVar();
    if (!v) return;
    const vmin = Number.isFinite(v.range.min) ? v.range.min : 0;
    const vmax = Number.isFinite(v.range.max) ? v.range.max : 1;
    vminEl.value = vmin.toPrecision(6);
    vmaxEl.value = vmax.toPrecision(6);
  }

  function refresh(): void {
    const v = currentVar();
    if (!v) return;
    const t = +time.value;
    const vmin = +vminEl.value;
    const vmax = +vmaxEl.value;
    if (scalarOn) layer.setState({ variable: v, t, vmin, vmax });
    if (windLayer) windLayer.setState({ t });
    if (isoLayer) isoLayer.setState({ t });
    if (arrowLayer) arrowLayer.setState({ t });
    if (hatchLayer) hatchLayer.setState({ t });
    const maxStep = wmt.timeStepCount - 1;
    const tF = Math.max(0, Math.min(maxStep, Math.floor(t)));
    const tC = Math.min(maxStep, tF + 1);
    const ms = wmt.timeAt(tF).getTime() +
      (wmt.timeAt(tC).getTime() - wmt.timeAt(tF).getTime()) * (t - tF);
    const ts = new Date(ms).toISOString().replace("T", " ").slice(0, 16) + "Z";
    $("timeLabel").textContent =
      `step ${t.toFixed(2)} / ${maxStep}  ·  ${ts}`;
    $("legTitle").textContent =
      v.name + (v.unit ? ` (${v.unit})` : "");
    $("legMin").textContent = (+vminEl.value).toPrecision(5);
    $("legMax").textContent = (+vmaxEl.value).toPrecision(5);
    status.textContent =
      `${backend.name} · ${wmt.variables.length} var · ${wmt.timeStepCount} steps · ` +
      `z${wmt.zoomRange.min}-${wmt.zoomRange.max} · gen ${wmt.snapshotGeneration}`;
  }

  paramSel.onchange = () => {
    populateHeights();
    applyRangeForVar();
    refresh();
  };
  heightSel.onchange = () => {
    applyRangeForVar();
    refresh();
  };
  time.oninput = refresh;
  vminEl.onchange = refresh;
  vmaxEl.onchange = refresh;
  populateHeights();
  applyRangeForVar();
  refresh();

  const forecastViewer = installForecastViewer(wmt, backend, () => +time.value);
  time.addEventListener("input", () => forecastViewer.onTimeChange());
  backend.onClick((lat, lng) => forecastViewer.show(lat, lng));

  console.log("wmtiles loaded", {
    renderer: backend.name,
    bbox: wmt.bbox,
    zoomRange: wmt.zoomRange,
    variables: wmt.variables,
    timeAxis: wmt.timeAxis,
  });
}

boot().catch((err: Error) => {
  console.error(err);
  $("status").textContent = "error: " + err.message;
});
