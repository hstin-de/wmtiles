import { open, latLonToTilePixel, type Variable, type WMT } from "wmtiles";
import { makeWMTLayer, type WMTLayer } from "./layer";
import { installDebugHud } from "./debug";

declare const L: typeof import("leaflet");

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
  layer: WMTLayer,
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

  const layer = makeWMTLayer(wmt).addTo(map);

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
    layer.setState({ variable: v, t, vmin, vmax });
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
