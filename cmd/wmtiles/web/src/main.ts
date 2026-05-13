import { open, latLonToTilePixel, type Variable, type WMT } from "wmtiles";
import type {
  WMTHeatmapLayer,
  WMTParticlesLayer,
  WMTIsobarLayer,
  WMTArrowsLayer,
} from "wmtiles/leaflet";
import "wmtiles/leaflet";
import { installDebugHud } from "./debug";
import L from "leaflet";

const $ = (id: string): HTMLElement => {
  const el = document.getElementById(id);
  if (!el) throw new Error(`missing #${id}`);
  return el;
};

interface ClickResult {
  value?: number;
  missing?: boolean;
  z: number;
  x: number;
  y: number;
  col: number;
  row: number;
}

async function valueAtClick(
  wmt: WMT,
  layer: WMTHeatmapLayer,
  latlng: L.LatLng,
  mapZoom: number,
): Promise<ClickResult | null> {
  const z = Math.min(
    Math.max(mapZoom | 0, wmt.zoomRange.min),
    wmt.zoomRange.max,
  );
  const px = latLonToTilePixel(
    z,
    latlng.lat,
    latlng.lng,
    wmt.tileSize,
  );
  if (!px) return null;
  const pixels = await layer.state.variable.tile({
    time: layer.state.t,
    z,
    x: px.x,
    y: px.y,
  });
  if (!pixels) return { missing: true, z, ...px };
  return {
    value: pixels[px.row * wmt.tileSize + px.col],
    z,
    ...px,
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

async function boot(): Promise<void> {
  const status = $("status");
  status.textContent = "fetching header…";

  // installs sink before open() so the open event is captured
  installDebugHud();

  const wmt = await open("/wmt");

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

  // ?fmt=r16f|r32f overrides the auto-probe, for diagnosing mobile uniform-colour bugs
  const urlFmt = new URLSearchParams(location.search).get("fmt");
  const tileTextureFormat =
    urlFmt === "r32f" || urlFmt === "r16f" || urlFmt === "auto"
      ? (urlFmt as "r32f" | "r16f" | "auto")
      : undefined;
  let layer: WMTHeatmapLayer = wmt.createHeatmapLayer({
    tileTextureFormat,
  }).addTo(map) as WMTHeatmapLayer;
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

  let windLayer: WMTParticlesLayer | null = null;
  function rebuildWindLayer(): void {
    const uName = windUSel.value;
    const vName = windVSel.value;
    if (windLayer) {
      map.removeLayer(windLayer);
      windLayer = null;
    }
    if (!uName || !vName) return;
    if (uName === vName) {
      console.warn("wmtiles: u and v must differ");
      return;
    }
    try {
      windLayer = wmt.createParticlesLayer({
        uVar: uName,
        vVar: vName,
        colormap: "white",
        particleSize: 2.5,
        speedFactor: 0.0005,
        tileTextureFormat,
      }).addTo(map) as WMTParticlesLayer;
      windLayer.setState({ t: +time.value });
    } catch (err) {
      console.error("wmtiles: wind overlay failed", err);
    }
  }
  windUSel.onchange = rebuildWindLayer;
  windVSel.onchange = rebuildWindLayer;

  const arrowUSel = $("arrowU") as HTMLSelectElement;
  const arrowVSel = $("arrowV") as HTMLSelectElement;
  for (const sel of [arrowUSel, arrowVSel]) {
    for (const v of sortedVars) {
      const o = document.createElement("option");
      o.value = v.name;
      o.textContent = v.name;
      sel.appendChild(o);
    }
    sel.disabled = false;
  }

  let arrowLayer: WMTArrowsLayer | null = null;
  function rebuildArrowLayer(): void {
    const uName = arrowUSel.value;
    const vName = arrowVSel.value;
    if (arrowLayer) {
      map.removeLayer(arrowLayer);
      arrowLayer = null;
    }
    if (!uName || !vName) return;
    if (uName === vName) {
      console.warn("wmtiles: arrow u and v must differ");
      return;
    }
    try {
      arrowLayer = wmt.createArrowsLayer({
        uVar: uName,
        vVar: vName,
        colormap: "viridis",
        tileTextureFormat,
      }).addTo(map) as WMTArrowsLayer;
      arrowLayer.setState({ t: +time.value });
    } catch (err) {
      console.error("wmtiles: arrow overlay failed", err);
    }
  }
  arrowUSel.onchange = rebuildArrowLayer;
  arrowVSel.onchange = rebuildArrowLayer;

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

  // Round a raw spacing target to a "nice" number: 1, 2, 5, 10, 20, 50, ...
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
    // Aim for ~25 contour lines across the data range.
    return niceSpacing(span / 25);
  }

  let isoLayer: WMTIsobarLayer | null = null;
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
      map.removeLayer(isoLayer);
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
      isoLayer = wmt.createIsobarLayer({
        variable: name,
        spacing,
        smoothness: +isoSmoothEl.value,
        fillEnabled: isoFillEl.checked,
        tileTextureFormat,
      }).addTo(map) as WMTIsobarLayer;
      isoLayer.setState({ t: +time.value });
      updateIsoHint();
    } catch (err) {
      console.error("wmtiles: isobar overlay failed", err);
    }
  }
  map.on("zoomend", updateIsoHint);

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

  const legend = new L.Control({ position: "bottomright" });
  legend.onAdd = () => {
    const div = L.DomUtil.create("div", "legend");
    div.id = "legend";
    div.innerHTML =
      '<div id="legTitle"></div><div class="bar"></div>' +
      '<div class="row"><span id="legMin"></span><span id="legMax"></span></div>';
    return div;
  };
  legend.addTo(map);

  const scalarEnabledEl = $("scalarEnabled") as HTMLInputElement;
  scalarEnabledEl.disabled = false;
  // Click on the checkbox must not toggle the surrounding <details>.
  scalarEnabledEl.addEventListener("click", (e) => e.stopPropagation());
  scalarEnabledEl.onchange = () => {
    const wantOn = scalarEnabledEl.checked;
    if (wantOn && !scalarOn) {
      layer = wmt.createHeatmapLayer({
        tileTextureFormat,
      }).addTo(map) as WMTHeatmapLayer;
      scalarOn = true;
      legend.addTo(map);
      refresh();
    } else if (!wantOn && scalarOn) {
      map.removeLayer(layer);
      map.removeControl(legend);
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
      `${wmt.variables.length} var · ${wmt.timeStepCount} steps · ` +
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

  map.on("click", async (ev: L.LeafletMouseEvent) => {
    const result = await valueAtClick(wmt, layer, ev.latlng, map.getZoom());
    const v = currentVar();
    let body: string;
    if (!result) body = "<i>out of range</i>";
    else if (result.missing) body = "<i>no tile</i>";
    else if (result.value === undefined || Number.isNaN(result.value)) {
      body = "<i>NaN</i>";
    } else {
      body = `<b>${result.value.toFixed(4)}</b> ${v?.unit ?? ""}`;
    }
    if (result && !(result.missing && result.value === undefined)) {
      body += `<br><small>z=${result.z} tile=(${result.x},${result.y}) px=(${result.col},${result.row})</small>`;
    }
    L.popup().setLatLng(ev.latlng).setContent(body).openOn(map);
  });

  console.log("wmtiles loaded", {
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
