import {
  setDebugSink,
  type DebugEvent,
  type TileEvent,
} from "wmtiles";

const CODEC_NAMES: Record<number, string> = {
  0x01: "const",
  0x02: "raw",
  0x03: "bitshuf",
  0x04: "delta",
  0x05: "lorenzo",
};

const DTYPE_NAMES: Record<number, string> = {
  0: "u8",
  1: "u16",
  3: "f32",
};

const MAX_RECENT = 20;
const HISTOGRAM_WINDOW = 200;

interface CodecAgg {
  count: number;
  bytes: number;
  network: number;
  decode: number;
  dequant: number;
}

interface Stats {
  openMs: number;
  reads: number;
  bytesRead: number;
  tiles: number;
  tilesBatches: number;
  tilesBatchHits: number;
  tilesBatchBytes: number;
  netSamples: number[];
  decSamples: number[];
  deqSamples: number[];
  byCodec: Map<number, CodecAgg>;
  recent: TileEvent[];
}

function makeStats(): Stats {
  return {
    openMs: 0,
    reads: 0,
    bytesRead: 0,
    tiles: 0,
    tilesBatches: 0,
    tilesBatchHits: 0,
    tilesBatchBytes: 0,
    netSamples: [],
    decSamples: [],
    deqSamples: [],
    byCodec: new Map(),
    recent: [],
  };
}

function pushSample(arr: number[], v: number): void {
  arr.push(v);
  if (arr.length > HISTOGRAM_WINDOW) arr.shift();
}

function quantile(samples: number[], q: number): number {
  if (samples.length === 0) return 0;
  const sorted = samples.slice().sort((a, b) => a - b);
  const i = Math.min(sorted.length - 1, Math.floor(q * sorted.length));
  return sorted[i];
}

function avg(samples: number[]): number {
  if (samples.length === 0) return 0;
  let s = 0;
  for (const v of samples) s += v;
  return s / samples.length;
}

function fmtMs(v: number): string {
  if (v === 0) return "—";
  if (v < 10) return v.toFixed(2);
  if (v < 100) return v.toFixed(1);
  return v.toFixed(0);
}

function fmtBytes(b: number): string {
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  return `${(b / (1024 * 1024)).toFixed(2)} MB`;
}

function row(cells: string[]): string {
  return `<tr>${cells.map((c) => `<td>${c}</td>`).join("")}</tr>`;
}

function ingest(stats: Stats, e: DebugEvent): void {
  switch (e.kind) {
    case "open":
      stats.openMs = e.totalMs;
      break;
    case "read":
      stats.reads++;
      stats.bytesRead += e.length;
      break;
    case "tile": {
      stats.tiles++;
      pushSample(stats.netSamples, e.networkMs);
      pushSample(stats.decSamples, e.decodeMs);
      pushSample(stats.deqSamples, e.dequantizeMs);
      let agg = stats.byCodec.get(e.codec);
      if (!agg) {
        agg = { count: 0, bytes: 0, network: 0, decode: 0, dequant: 0 };
        stats.byCodec.set(e.codec, agg);
      }
      agg.count++;
      agg.bytes += e.compressedBytes;
      agg.network += e.networkMs;
      agg.decode += e.decodeMs;
      agg.dequant += e.dequantizeMs;
      stats.recent.unshift(e);
      if (stats.recent.length > MAX_RECENT) stats.recent.pop();
      break;
    }
    case "tiles":
      stats.tilesBatches++;
      stats.tilesBatchHits += e.hitCount;
      stats.tilesBatchBytes += e.bytesFetched;
      break;
  }
}

function renderPanel(stats: Stats): string {
  const phaseRow = (label: string, samples: number[]) =>
    row([
      label,
      fmtMs(avg(samples)),
      fmtMs(quantile(samples, 0.5)),
      fmtMs(quantile(samples, 0.95)),
    ]);

  const codecRows: string[] = [];
  const codecs = [...stats.byCodec.entries()].sort(
    (a, b) => b[1].count - a[1].count,
  );
  for (const [c, a] of codecs) {
    codecRows.push(
      row([
        CODEC_NAMES[c] ?? `0x${c.toString(16)}`,
        String(a.count),
        fmtBytes(a.bytes / a.count),
        fmtMs(a.decode / a.count),
        fmtMs(a.dequant / a.count),
        fmtMs(a.network / a.count),
      ]),
    );
  }

  const recentRows: string[] = [];
  for (const e of stats.recent) {
    recentRows.push(
      row([
        String(e.z),
        `${e.x},${e.y}`,
        CODEC_NAMES[e.codec] ?? `0x${e.codec.toString(16)}`,
        DTYPE_NAMES[e.dtype] ?? `?${e.dtype}`,
        fmtBytes(e.compressedBytes),
        fmtMs(e.decodeMs),
        fmtMs(e.dequantizeMs),
        fmtMs(e.networkMs),
      ]),
    );
  }

  return `
    <div class="hudLine"><b>open:</b> ${fmtMs(stats.openMs)} ms · <b>reads:</b> ${stats.reads} · <b>data:</b> ${fmtBytes(stats.bytesRead)}</div>
    <div class="hudLine"><b>tiles:</b> ${stats.tiles} · <b>batches:</b> ${stats.tilesBatches} (${stats.tilesBatchHits} hits, ${fmtBytes(stats.tilesBatchBytes)})</div>
    <table class="hudTable">
      <thead><tr><th>phase</th><th>avg</th><th>p50</th><th>p95</th></tr></thead>
      <tbody>
        ${phaseRow("network", stats.netSamples)}
        ${phaseRow("decode", stats.decSamples)}
        ${phaseRow("dequant", stats.deqSamples)}
      </tbody>
    </table>
    <table class="hudTable">
      <thead><tr><th>codec</th><th>n</th><th>bytes/avg</th><th>dec</th><th>deq</th><th>net</th></tr></thead>
      <tbody>${codecRows.join("") || row(["—", "", "", "", "", ""])}</tbody>
    </table>
    <div class="hudLine"><b>recent:</b></div>
    <table class="hudTable">
      <thead><tr><th>z</th><th>x,y</th><th>codec</th><th>dtype</th><th>bytes</th><th>dec</th><th>deq</th><th>net</th></tr></thead>
      <tbody>${recentRows.join("") || row(["—", "", "", "", "", "", "", ""])}</tbody>
    </table>
  `;
}

const HUD_CSS = `
  #wmtHud {
    position: fixed;
    top: 60px;
    right: 12px;
    width: 360px;
    max-height: calc(100vh - 80px);
    background: rgba(20, 20, 20, 0.95);
    color: #ddd;
    border: 1px solid #444;
    border-radius: 4px;
    font: 11px/1.35 ui-monospace, "SF Mono", Menlo, monospace;
    z-index: 1000;
    display: none;
    overflow: auto;
    box-shadow: 0 4px 20px rgba(0,0,0,0.5);
  }
  #wmtHud.open { display: block; }
  #wmtHudHdr {
    position: sticky; top: 0; background: #2a2a2a;
    padding: 6px 10px; display: flex; gap: 8px; align-items: center;
    border-bottom: 1px solid #444;
  }
  #wmtHudHdr .title { font-weight: bold; }
  #wmtHudHdr .spacer { flex: 1; }
  #wmtHudHdr button {
    background: #1a1a1a; color: #ddd; border: 1px solid #444;
    border-radius: 3px; padding: 2px 8px; font-size: 11px; cursor: pointer;
  }
  #wmtHudHdr button:hover { background: #333; }
  #wmtHudBody { padding: 8px 10px; }
  .hudLine { margin: 4px 0; color: #bbb; }
  .hudLine b { color: #eee; font-weight: normal; }
  .hudTable {
    width: 100%; border-collapse: collapse; margin: 6px 0 10px;
    font-size: 10.5px;
  }
  .hudTable th, .hudTable td {
    text-align: right; padding: 2px 4px; border-bottom: 1px solid #2a2a2a;
  }
  .hudTable th:first-child, .hudTable td:first-child { text-align: left; }
  .hudTable th { color: #888; font-weight: normal; }
  .hudTable td { color: #ccc; }
  #wmtHudToggle {
    position: fixed; top: 60px; right: 12px;
    background: rgba(34,34,34,0.92); color: #ddd;
    border: 1px solid #444; border-radius: 3px;
    padding: 4px 8px; font: 11px monospace; cursor: pointer;
    z-index: 999;
  }
  #wmtHudToggle:hover { background: rgba(60,60,60,0.95); }
  #wmtHud.open ~ #wmtHudToggle { display: none; }
`;

export function installDebugHud(): void {
  const style = document.createElement("style");
  style.textContent = HUD_CSS;
  document.head.appendChild(style);

  const hud = document.createElement("div");
  hud.id = "wmtHud";
  hud.innerHTML = `
    <div id="wmtHudHdr">
      <span class="title">wmt debug</span>
      <span class="spacer"></span>
      <button id="wmtHudClear">clear</button>
      <button id="wmtHudClose">×</button>
    </div>
    <div id="wmtHudBody"></div>
  `;
  document.body.appendChild(hud);

  const toggle = document.createElement("button");
  toggle.id = "wmtHudToggle";
  toggle.textContent = "debug (d)";
  document.body.appendChild(toggle);

  const body = hud.querySelector<HTMLElement>("#wmtHudBody")!;
  let stats = makeStats();
  let dirty = false;

  setDebugSink((e) => {
    ingest(stats, e);
    dirty = true;
  });

  const tick = () => {
    if (dirty && hud.classList.contains("open")) {
      body.innerHTML = renderPanel(stats);
      dirty = false;
    }
  };
  setInterval(tick, 250);

  const open = () => {
    hud.classList.add("open");
    body.innerHTML = renderPanel(stats);
  };
  const close = () => hud.classList.remove("open");

  toggle.onclick = open;
  hud.querySelector<HTMLButtonElement>("#wmtHudClose")!.onclick = close;
  hud.querySelector<HTMLButtonElement>("#wmtHudClear")!.onclick = () => {
    stats = makeStats();
    body.innerHTML = renderPanel(stats);
  };
  document.addEventListener("keydown", (ev) => {
    if (ev.key !== "d" && ev.key !== "D") return;
    const target = ev.target as HTMLElement | null;
    if (target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName)) return;
    if (hud.classList.contains("open")) close();
    else open();
  });
}
