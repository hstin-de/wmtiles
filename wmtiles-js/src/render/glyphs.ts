import type { TileDrawRect } from "./heatmap.js";
import {
  colormapToGLSL,
  resolveColormap,
  type BuiltinColormapName,
  type Colormap,
} from "./colormap.js";
import { MISSING_GLSL_PREAMBLE } from "./missing.js";
import { linkProgram } from "./gl.js";
import {
  getProjectionUniformLocs,
  setProjectionUniforms,
  type GlobeProjectionData,
  type GlobeShaderData,
  type ProjectionUniformLocs,
} from "./globe.js";
import {
  beginHostFrame,
  RedrawScheduler,
  VariantProgramCache,
} from "./backend.js";

export type { TileDrawRect } from "./heatmap.js";

export interface AtlasLayout {
  tileW: number;
  tileH: number;
  minX: number;
  minY: number;
}

// Per-frame data handoff. The consumer builds (or reuses) the data atlas and
// returns its texture; `ready` is false while tiles are still loading.
export interface GlyphAtlas {
  tex: WebGLTexture;
  ready: boolean;
}

export interface SpriteSheet {
  // a URL the renderer loads, or an already-decoded image
  image: TexImageSource | string;
  cols: number;
  rows: number;
}

export interface SpriteIcons {
  // one entry per icon index; strings are loaded as URLs
  icons: (string | HTMLImageElement | ImageBitmap)[];
  // px per packed cell; defaults to the largest icon dimension, capped at 256
  cellSize?: number;
}

export type GlyphSprite = SpriteSheet | SpriteIcons;

export interface GlyphRendererOptions {
  // glyphsPerTile² glyphs per visible tile, anchored to tile coords so they
  // stay locked to world position across pan/zoom
  glyphsPerTile?: number;
  glyphSize?: number; // px
  // 0 disables. Outline pass redraws slightly larger in a dark colour so
  // glyphs stay legible on bright backgrounds. Ignored in sprite mode.
  outlineWidth?: number;
  outlineColor?: [number, number, number];
  colormap?: Colormap | BuiltinColormapName;
  // normalises the rule's value output onto the colormap
  valueRange?: [number, number];
  alpha?: number;
  // When true: caller drives drawing via draw(), tile rects are mercator world
  // units (0..1), and no internal rAF loop runs.
  matrixMode?: boolean;
  // Matrix mode only. Fires when the next draw() would differ. Wire to
  // map.triggerRepaint().
  onRedraw?: () => void;
  onFrame?: (frameMs: number) => void; // cpu time per draw

  geometry: Float32Array;
  rule: string;
  // Pulls the data atlas for the current frame. Called once per draw after the
  // view is known non-empty; texture layout must match getAtlasLayout().
  prepareAtlas: () => GlyphAtlas | null;
  texelsPerTile: number;
  // Matrix mode only. When true the rule's rotation is geographic: glyphs turn
  // with the map's bearing instead of staying screen-locked. Implied by `flat`.
  rotateWithMap?: boolean;
  flat?: boolean;
  // When set, glyphs are textured quads sampling these icons instead of solid
  // colormap-shaded geometry.
  sprite?: GlyphSprite;
}

const DEFAULTS = {
  glyphsPerTile: 8,
  glyphSize: 16,
  outlineWidth: 1.5,
  outlineColor: [0, 0, 0] as [number, number, number],
  valueRange: [0, 30] as [number, number],
  alpha: 0.95,
} as const;

// Centered unit quad (2 triangles). Sprite mode samples cell UV as
// a_local + 0.5, so this must span exactly -0.5..0.5.
const SPRITE_QUAD = new Float32Array([
  -0.5, -0.5,
   0.5, -0.5,
  -0.5,  0.5,
  -0.5,  0.5,
   0.5, -0.5,
   0.5,  0.5,
]);

interface GlyphProgram {
  program: WebGLProgram;
  proj: ProjectionUniformLocs; // all-null in screen mode
}

function makeGlyphProgram(
  gl: WebGL2RenderingContext,
  vs: string,
  fs: string,
): GlyphProgram {
  const program = linkProgram(gl, vs, fs);
  return { program, proj: getProjectionUniformLocs(gl, program) };
}

// HTMLImageElement exposes the decoded size as natural*; ImageBitmap only has
// width/height.
function iconW(im: HTMLImageElement | ImageBitmap): number {
  return "naturalWidth" in im ? im.naturalWidth : im.width;
}
function iconH(im: HTMLImageElement | ImageBitmap): number {
  return "naturalHeight" in im ? im.naturalHeight : im.height;
}

// a_inst.zw: device-pixel center of the glyph.
function buildGlyphScreenVS(rule: string, sprite: boolean): string {
  return `#version 300 es
precision highp float;

layout(location=0) in vec2 a_local;
layout(location=1) in vec4 a_inst;  // dataU, dataV, sx, sy

uniform sampler2D u_dataTex;
uniform vec2  u_screen;       // canvas in device px
uniform float u_glyphSize;    // px
uniform float u_sizeBoost;    // 1 for fill, >1 for outline
uniform vec2  u_valueRange;
${sprite ? "uniform vec2 u_spriteGrid;  // cols, rows" : ""}

flat out int v_alive;
out float v_t;
${sprite ? "out vec2 v_spriteUV;" : ""}

${MISSING_GLSL_PREAMBLE}
// glyph latitude in radians, for rules that need geographic context; screen
// mode carries no geo info so it stays 0
float g_glyphLat;
${rule}

void main() {
  vec2 dataUV = a_inst.xy;
  vec2 screenCenter = a_inst.zw;

  g_glyphLat = 0.0;
  vec4 g = glyphRule(texture(u_dataTex, dataUV));
  if (g.z < 0.5) {
    v_alive = 0;
    gl_Position = vec4(2.0, 2.0, 2.0, 1.0);
    return;
  }
  v_alive = 1;
  v_t = clamp((g.y - u_valueRange.x) / max(u_valueRange.y - u_valueRange.x, 1e-30), 0.0, 1.0);
${sprite ? `
  // row-major sprite cell from the rule's icon index (g.w)
  float idx = floor(g.w + 0.5);
  float col = mod(idx, u_spriteGrid.x);
  float row = floor(idx / u_spriteGrid.x);
  v_spriteUV = (vec2(col, row) + a_local + 0.5) / u_spriteGrid;
` : ""}
  float c = cos(g.x), s = sin(g.x);
  vec2 local = a_local * u_glyphSize * u_sizeBoost;
  vec2 rotated = vec2(local.x * c - local.y * s, local.x * s + local.y * c);
  vec2 px = screenCenter + rotated;
  vec2 ndc = vec2(px.x / u_screen.x * 2.0 - 1.0, 1.0 - px.y / u_screen.y * 2.0);
  gl_Position = vec4(ndc, 0.0, 1.0);
}`;
}

function buildGlyphMatrixVS(
  rule: string,
  sprite: boolean,
  rotateWithMap: boolean,
  flat: boolean,
  shaderData: GlobeShaderData,
): string {
  return `#version 300 es
precision highp float;
${shaderData.vertexShaderPrelude}
${shaderData.define}

layout(location=0) in vec2 a_local;
layout(location=1) in vec4 a_inst;  // dataU, dataV, mercX, mercY

uniform sampler2D u_dataTex;
uniform vec2  u_screen;
uniform float u_glyphSize;    // device pixels
uniform float u_sizeBoost;
uniform vec2  u_valueRange;
${sprite ? "uniform vec2 u_spriteGrid;  // cols, rows" : ""}
${rotateWithMap && !flat ? "uniform float u_bearing;  // radians, map compass bearing" : ""}
${flat ? "uniform float u_glyphMercSize;  // glyph size in mercator units" : ""}

flat out int v_alive;
out float v_t;
${sprite ? "out vec2 v_spriteUV;" : ""}

${MISSING_GLSL_PREAMBLE}
// glyph latitude in radians, derived from the mercator center for rules that
// need geographic context (e.g. hemisphere-aware wind barbs)
float g_glyphLat;
${rule}

void main() {
  vec2 dataUV = a_inst.xy;
  vec2 center = a_inst.zw;

  g_glyphLat = atan(sinh(3.14159265 * (1.0 - 2.0 * center.y)));
  vec4 g = glyphRule(texture(u_dataTex, dataUV));
  if (g.z < 0.5) {
    v_alive = 0;
    gl_Position = vec4(2.0, 2.0, 2.0, 1.0);
    return;
  }
  v_alive = 1;
  v_t = clamp((g.y - u_valueRange.x) / max(u_valueRange.y - u_valueRange.x, 1e-30), 0.0, 1.0);
${sprite ? `
  // row-major sprite cell from the rule's icon index (g.w)
  float idx = floor(g.w + 0.5);
  float col = mod(idx, u_spriteGrid.x);
  float row = floor(idx / u_spriteGrid.x);
  v_spriteUV = (vec2(col, row) + a_local + 0.5) / u_spriteGrid;
` : ""}
  // billboard: rule rotation is screen-relative, subtract bearing so geographic
  // angles turn with the map. flat: projectTile() handles the map rotation.
  ${rotateWithMap && !flat ? "float ang = g.x - u_bearing;" : "float ang = g.x;"}
  float c = cos(ang), s = sin(ang);
${flat ? `
  // glyph lies in the tile plane: rotate in mercator units and project each
  // vertex, so it foreshortens with tilt and turns with the map
  vec2 lr = vec2(a_local.x * c - a_local.y * s, a_local.x * s + a_local.y * c);
  gl_Position = projectTile(center + lr * u_glyphMercSize * u_sizeBoost);
` : `
  // glyph billboards toward the camera at a fixed device-pixel size
  vec2 local = a_local * u_glyphSize * u_sizeBoost;
  vec2 rotated = vec2(local.x * c - local.y * s, local.x * s + local.y * c);
  vec4 centerClip = projectTile(center);
  // px -> NDC: multiply by 2/screen, flip y. Multiply by w to counter the
  // perspective divide so the offset stays a screen-pixel size.
  vec2 ndcOffset = vec2(rotated.x * 2.0 / u_screen.x, -rotated.y * 2.0 / u_screen.y);
  gl_Position = vec4(centerClip.xy + ndcOffset * centerClip.w, centerClip.z, centerClip.w);
`}
}`;
}

function buildOutlineFS(premultiply: boolean): string {
  return `#version 300 es
precision highp float;
flat in int v_alive;
in float v_t;
out vec4 outColor;
uniform vec3 u_color;
uniform float u_alpha;
void main() {
  if (v_alive == 0) discard;
  ${premultiply
    ? "outColor = vec4(u_color * u_alpha, u_alpha);"
    : "outColor = vec4(u_color, u_alpha);"}
}`;
}

function buildFillFS(colormap: Colormap, premultiply: boolean): string {
  return `#version 300 es
precision highp float;
flat in int v_alive;
in float v_t;
out vec4 outColor;

${colormapToGLSL(colormap)}

uniform float u_alpha;
void main() {
  if (v_alive == 0) discard;
  vec3 rgb = colormap(v_t);
  ${premultiply
    ? "outColor = vec4(rgb * u_alpha, u_alpha);"
    : "outColor = vec4(rgb, u_alpha);"}
}`;
}

function buildSpriteFS(premultiply: boolean): string {
  return `#version 300 es
precision highp float;
flat in int v_alive;
in vec2 v_spriteUV;
out vec4 outColor;
uniform sampler2D u_spriteSheet;
uniform float u_alpha;
void main() {
  if (v_alive == 0) discard;
  vec4 c = texture(u_spriteSheet, v_spriteUV);
  ${premultiply
    ? "outColor = vec4(c.rgb * c.a * u_alpha, c.a * u_alpha);"
    : "outColor = vec4(c.rgb, c.a * u_alpha);"}
}`;
}

export class GlyphRenderer {
  private readonly gl: WebGL2RenderingContext;
  private readonly matrixMode: boolean;
  private readonly onFrame?: (frameMs: number) => void;
  private readonly prepareAtlas: () => GlyphAtlas | null;
  private readonly vertsPerGlyph: number;
  private readonly texelsPerTile: number;
  private readonly rotateWithMap: boolean;
  private readonly flat: boolean;
  // matrix-mode map state, refreshed per draw(). worldSize is css px for the
  // whole mercator world; flat mode uses it to size glyphs in merc units.
  private bearingRad = 0;
  private worldSize = 0;

  private readonly opts: Required<
    Pick<
      GlyphRendererOptions,
      | "glyphsPerTile"
      | "glyphSize"
      | "outlineWidth"
      | "outlineColor"
      | "valueRange"
      | "alpha"
    >
  >;

  // sprite mode: textured quads instead of colormap-shaded geometry. spriteTex
  // stays null until an async URL-loaded sheet arrives.
  private readonly spriteMode: boolean;
  private spriteTex: WebGLTexture | null = null;
  private readonly spriteCols: number;
  private readonly spriteRows: number;

  // outline pass only exists in non-sprite mode
  private readonly outlinePrograms: VariantProgramCache<GlyphProgram> | null;
  private readonly fillPrograms: VariantProgramCache<GlyphProgram>;
  private readonly scheduler: RedrawScheduler;
  private readonly glyphVAO: WebGLVertexArrayObject;
  private readonly geometryVBO: WebGLBuffer;
  private readonly instanceVBO: WebGLBuffer;

  private canvasW = 1;
  private canvasH = 1;
  private view: TileDrawRect[] = [];
  private layout: AtlasLayout | null = null;
  private viewKey = "";
  // instance buffer depends on the visible tile set + atlas layout only
  private instanceBuf: Float32Array | null = null;
  private instancesDirty = true;
  private instanceCount = 0;
  private disposed = false;

  constructor(gl: WebGL2RenderingContext, options: GlyphRendererOptions) {
    this.gl = gl;
    this.matrixMode = options.matrixMode ?? false;
    this.onFrame = options.onFrame;
    this.prepareAtlas = options.prepareAtlas;
    this.texelsPerTile = Math.max(1, options.texelsPerTile | 0);
    this.rotateWithMap = options.rotateWithMap ?? false;
    this.flat = options.flat ?? false;

    this.opts = {
      glyphsPerTile: Math.max(
        1,
        options.glyphsPerTile ?? DEFAULTS.glyphsPerTile,
      ),
      glyphSize: options.glyphSize ?? DEFAULTS.glyphSize,
      outlineWidth: options.outlineWidth ?? DEFAULTS.outlineWidth,
      outlineColor: options.outlineColor ?? DEFAULTS.outlineColor,
      valueRange: options.valueRange ?? DEFAULTS.valueRange,
      alpha: options.alpha ?? DEFAULTS.alpha,
    };

    const sprite = options.sprite;
    this.spriteMode = !!sprite;
    if (sprite && "icons" in sprite) {
      // square-ish grid; the icon count drives cols/rows, cellSize only drives
      // the packed canvas resolution
      const n = sprite.icons.length;
      this.spriteCols = Math.max(1, Math.ceil(Math.sqrt(n)));
      this.spriteRows = Math.max(1, Math.ceil(n / this.spriteCols));
    } else {
      this.spriteCols = sprite?.cols ?? 0;
      this.spriteRows = sprite?.rows ?? 0;
    }
    const geometry = sprite ? SPRITE_QUAD : options.geometry;
    this.vertsPerGlyph = geometry.length / 2;

    const colormap = resolveColormap(options.colormap);
    const rule = options.rule;
    const spriteMode = this.spriteMode;
    const rotateWithMap = this.rotateWithMap;
    const flat = this.flat;

    this.scheduler = new RedrawScheduler(
      this.matrixMode,
      () => this.drawInternal(null, null),
      options.onRedraw,
    );

    // matrix mode composites over the host with ONE/ONE_MINUS_SRC_ALPHA, so
    // the FS must output premultiplied alpha; screen mode uses straight
    this.fillPrograms = new VariantProgramCache<GlyphProgram>(
      gl,
      this.matrixMode,
      {
        buildScreen: (g) =>
          makeGlyphProgram(
            g,
            buildGlyphScreenVS(rule, spriteMode),
            spriteMode ? buildSpriteFS(false) : buildFillFS(colormap, false),
          ),
        buildMatrix: (g, sd) =>
          makeGlyphProgram(
            g,
            buildGlyphMatrixVS(rule, spriteMode, rotateWithMap, flat, sd),
            spriteMode ? buildSpriteFS(true) : buildFillFS(colormap, true),
          ),
        destroy: (g, p) => g.deleteProgram(p.program),
      },
    );
    // sprite glyphs carry their own colours, so the dark outline pass (a
    // larger solid silhouette) makes no sense there
    this.outlinePrograms = spriteMode
      ? null
      : new VariantProgramCache<GlyphProgram>(gl, this.matrixMode, {
          buildScreen: (g) =>
            makeGlyphProgram(
              g,
              buildGlyphScreenVS(rule, false),
              buildOutlineFS(false),
            ),
          buildMatrix: (g, sd) =>
            makeGlyphProgram(
              g,
              buildGlyphMatrixVS(rule, false, rotateWithMap, flat, sd),
              buildOutlineFS(true),
            ),
          destroy: (g, p) => g.deleteProgram(p.program),
        });

    const gvbo = gl.createBuffer();
    if (!gvbo) throw new Error("createBuffer failed");
    this.geometryVBO = gvbo;
    gl.bindBuffer(gl.ARRAY_BUFFER, gvbo);
    gl.bufferData(gl.ARRAY_BUFFER, geometry, gl.STATIC_DRAW);
    const ivbo = gl.createBuffer();
    if (!ivbo) throw new Error("createBuffer failed");
    this.instanceVBO = ivbo;
    const vao = gl.createVertexArray();
    if (!vao) throw new Error("createVertexArray failed");
    gl.bindVertexArray(vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, gvbo);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, ivbo);
    gl.enableVertexAttribArray(1);
    gl.vertexAttribPointer(1, 4, gl.FLOAT, false, 0, 0);
    gl.vertexAttribDivisor(1, 1);
    gl.bindVertexArray(null);
    this.glyphVAO = vao;

    if (sprite && "icons" in sprite) {
      this.loadIconList(sprite.icons, sprite.cellSize);
    } else if (sprite) {
      const src = sprite.image;
      if (typeof src === "string") this.loadSprite(src);
      else this.spriteTex = this.uploadSprite(src);
    }
  }

  // Async URL load. Until it resolves the sprite pass is skipped; the redraw
  // is rescheduled once the sheet is uploaded.
  private loadSprite(url: string): void {
    this.resolveIcon(url).then((img) => {
      if (this.disposed || !img) return;
      this.spriteTex = this.uploadSprite(img);
      this.scheduler.schedule();
    });
  }

  private loadIconList(
    icons: (string | HTMLImageElement | ImageBitmap)[],
    cellSize: number | undefined,
  ): void {
    if (icons.length === 0) return;
    Promise.all(icons.map((src) => this.resolveIcon(src))).then((images) => {
      if (this.disposed) return;
      this.spriteTex = this.packIcons(images, cellSize);
      this.scheduler.schedule();
    });
  }

  // Resolves to the decoded image, or null on load failure (never rejects).
  private resolveIcon(
    src: string | HTMLImageElement | ImageBitmap,
  ): Promise<HTMLImageElement | ImageBitmap | null> {
    if (typeof src !== "string") return Promise.resolve(src);
    return new Promise((resolve) => {
      const img = new Image();
      // icons are often CDN-hosted; WebGL needs CORS-clean pixels
      img.crossOrigin = "anonymous";
      img.onload = (): void => resolve(img);
      img.onerror = (): void => {
        console.error(`wmtiles: failed to load sprite icon "${src}"`);
        resolve(null);
      };
      img.src = src;
    });
  }

  private packIcons(
    images: (HTMLImageElement | ImageBitmap | null)[],
    cellSize: number | undefined,
  ): WebGLTexture {
    const cols = this.spriteCols;
    const rows = this.spriteRows;
    let cs = cellSize ?? 0;
    if (cs <= 0) {
      for (const im of images) {
        if (im) cs = Math.max(cs, iconW(im), iconH(im));
      }
      cs = Math.min(cs || 64, 256);
    }
    const canvas = document.createElement("canvas");
    canvas.width = cols * cs;
    canvas.height = rows * cs;
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("2d canvas context unavailable");
    images.forEach((im, i) => {
      if (!im) return;
      const w = iconW(im);
      const h = iconH(im);
      const scale = Math.min(cs / w, cs / h);
      const dw = w * scale;
      const dh = h * scale;
      const dx = (i % cols) * cs + (cs - dw) / 2;
      const dy = Math.floor(i / cols) * cs + (cs - dh) / 2;
      ctx.drawImage(im, dx, dy, dw, dh);
    });
    return this.uploadSprite(canvas);
  }

  private uploadSprite(image: TexImageSource): WebGLTexture {
    const gl = this.gl;
    const tex = gl.createTexture();
    if (!tex) throw new Error("createTexture failed");
    gl.bindTexture(gl.TEXTURE_2D, tex);
    // a shared host context may have left FLIP_Y on; sprite UVs assume the
    // image's natural top-left origin
    gl.pixelStorei(gl.UNPACK_FLIP_Y_WEBGL, false);
    gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, image);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
    return tex;
  }

  // Returns true when the tile set actually changed, so a consumer can gate an
  // atlas rebuild on it.
  setView(tiles: TileDrawRect[]): boolean {
    if (this.disposed) return false;
    // canonical world only, no wrap copies
    const filtered = tiles.filter((t) => t.worldX === t.x);
    // fingerprint the tile set: matrix mode calls setView on every move, and a
    // pan that doesn't change the set shouldn't trigger a rebuild
    let key = "";
    for (const t of filtered) key += `${t.z},${t.x},${t.y};`;
    if (key === this.viewKey) return false;
    this.viewKey = key;
    this.view = filtered;
    this.computeLayout();
    this.instancesDirty = true;
    this.scheduler.schedule();
    return true;
  }

  setViewport(widthDevPx: number, heightDevPx: number): void {
    if (this.disposed) return;
    const w = Math.max(1, widthDevPx | 0);
    const h = Math.max(1, heightDevPx | 0);
    if (this.canvasW === w && this.canvasH === h) return;
    this.canvasW = w;
    this.canvasH = h;
    if (!this.matrixMode) {
      this.gl.viewport(0, 0, w, h);
      this.scheduler.schedule();
    }
  }

  // Matrix mode entry point. Call from the host's per-frame render hook.
  // worldSize is the host's mercator-world width in css px (flat-mode sizing).
  draw(
    projData: GlobeProjectionData,
    shaderData: GlobeShaderData,
    viewportWPx: number,
    viewportHPx: number,
    bearingRad = 0,
    worldSize = 0,
  ): void {
    if (this.disposed || !this.matrixMode) return;
    this.canvasW = Math.max(1, viewportWPx | 0);
    this.canvasH = Math.max(1, viewportHPx | 0);
    this.bearingRad = bearingRad;
    this.worldSize = worldSize;
    this.drawInternal(projData, shaderData);
  }

  // Trigger a redraw after the consumer's data changes (new variable/time).
  schedule(): void {
    this.scheduler.schedule();
  }

  // Atlas tile-grid layout for the current view, or null when the view is
  // empty. The consumer's prepareAtlas() must produce a matching texture.
  getAtlasLayout(): AtlasLayout | null {
    return this.layout;
  }

  // Visible tile set (canonical world only). The consumer iterates this to
  // blit its data atlas; slot order matches getAtlasLayout().
  getView(): readonly TileDrawRect[] {
    return this.view;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.scheduler.dispose();
    const gl = this.gl;
    gl.deleteVertexArray(this.glyphVAO);
    gl.deleteBuffer(this.geometryVBO);
    gl.deleteBuffer(this.instanceVBO);
    if (this.spriteTex) gl.deleteTexture(this.spriteTex);
    this.outlinePrograms?.dispose();
    this.fillPrograms.dispose();
  }

  private computeLayout(): void {
    if (this.view.length === 0) {
      this.layout = null;
      return;
    }
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const t of this.view) {
      if (t.x < minX) minX = t.x;
      if (t.y < minY) minY = t.y;
      if (t.x > maxX) maxX = t.x;
      if (t.y > maxY) maxY = t.y;
    }
    this.layout = {
      tileW: maxX - minX + 1,
      tileH: maxY - minY + 1,
      minX,
      minY,
    };
  }

  // per-instance buffer at fixed tile-fractional coords so glyphs stay world-
  // locked. Reuploaded only when the visible tile set changes
  private uploadInstances(): void {
    const layout = this.layout;
    const view = this.view;
    if (!layout || view.length === 0) {
      this.instanceCount = 0;
      return;
    }
    const N = this.opts.glyphsPerTile;
    const ts = this.texelsPerTile;
    const atlasW = layout.tileW * ts;
    const atlasH = layout.tileH * ts;
    const total = view.length * N * N;
    if (!this.instanceBuf || this.instanceBuf.length !== total * 4) {
      this.instanceBuf = new Float32Array(total * 4);
    }
    const buf = this.instanceBuf;
    let i = 0;
    for (const t of view) {
      const tx = t.x - layout.minX;
      const ty = t.y - layout.minY;
      const dx = t.sx1 - t.sx0;
      const dy = t.sy1 - t.sy0;
      for (let fy = 0; fy < N; fy++) {
        const fv = (fy + 0.5) / N;
        const sy = t.sy0 + fv * dy;
        // sample at the center of the atlas texel this cell falls in: keeps
        // NEAREST stable when float32 rounding of dataUV shifts on regrid
        const dataV = (Math.floor((ty + fv) * ts) + 0.5) / atlasH;
        for (let fx = 0; fx < N; fx++) {
          const fu = (fx + 0.5) / N;
          buf[i++] = (Math.floor((tx + fu) * ts) + 0.5) / atlasW;
          buf[i++] = dataV;
          buf[i++] = t.sx0 + fu * dx;
          buf[i++] = sy;
        }
      }
    }
    const gl = this.gl;
    gl.bindBuffer(gl.ARRAY_BUFFER, this.instanceVBO);
    gl.bufferData(gl.ARRAY_BUFFER, buf, gl.DYNAMIC_DRAW);
    this.instanceCount = total;
  }

  private setFramebufferState(): void {
    const gl = this.gl;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    if (this.matrixMode) {
      // host owns the framebuffer, don't clear
      beginHostFrame(gl, this.canvasW, this.canvasH, "premultiplied");
    } else {
      gl.viewport(0, 0, this.canvasW, this.canvasH);
      gl.clearColor(0, 0, 0, 0);
      gl.clear(gl.COLOR_BUFFER_BIT);
      gl.enable(gl.BLEND);
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
    }
  }

  private clearCanvas(): void {
    if (this.matrixMode) return; // host owns the framebuffer
    const gl = this.gl;
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    gl.viewport(0, 0, this.canvasW, this.canvasH);
    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT);
  }

  private drawGlyphs(
    dataTex: WebGLTexture,
    projData: GlobeProjectionData | null,
    shaderData: GlobeShaderData | null,
  ): void {
    const gl = this.gl;
    this.setFramebufferState();

    if (this.instancesDirty) {
      this.uploadInstances();
      this.instancesDirty = false;
    }
    if (this.instanceCount === 0) return;

    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    // u_glyphSize is device pixels (billboard mode)
    const sizeUniform = this.opts.glyphSize * dpr;
    let glyphMercSize = 0;
    if (this.flat) {
      if (this.worldSize > 0) {
        glyphMercSize = this.opts.glyphSize / this.worldSize;
      } else if (this.view.length) {
        const mercPerPx =
          (this.view[0].sx1 - this.view[0].sx0) / this.texelsPerTile;
        glyphMercSize = sizeUniform * mercPerPx;
      }
    }

    const setCommon = (prog: GlyphProgram, sizeBoost: number): void => {
      gl.useProgram(prog.program);
      gl.bindVertexArray(this.glyphVAO);
      gl.activeTexture(gl.TEXTURE0);
      gl.bindTexture(gl.TEXTURE_2D, dataTex);
      gl.uniform1i(gl.getUniformLocation(prog.program, "u_dataTex"), 0);
      gl.uniform2f(
        gl.getUniformLocation(prog.program, "u_screen"),
        this.canvasW,
        this.canvasH,
      );
      gl.uniform1f(
        gl.getUniformLocation(prog.program, "u_glyphSize"),
        sizeUniform,
      );
      gl.uniform1f(gl.getUniformLocation(prog.program, "u_sizeBoost"), sizeBoost);
      gl.uniform2f(
        gl.getUniformLocation(prog.program, "u_valueRange"),
        this.opts.valueRange[0],
        this.opts.valueRange[1],
      );
      gl.uniform1f(
        gl.getUniformLocation(prog.program, "u_alpha"),
        this.opts.alpha,
      );
      if (this.spriteTex) {
        gl.activeTexture(gl.TEXTURE1);
        gl.bindTexture(gl.TEXTURE_2D, this.spriteTex);
        gl.uniform1i(gl.getUniformLocation(prog.program, "u_spriteSheet"), 1);
        gl.uniform2f(
          gl.getUniformLocation(prog.program, "u_spriteGrid"),
          this.spriteCols,
          this.spriteRows,
        );
      }
      if (this.rotateWithMap && !this.flat) {
        gl.uniform1f(
          gl.getUniformLocation(prog.program, "u_bearing"),
          this.bearingRad,
        );
      }
      if (this.flat) {
        gl.uniform1f(
          gl.getUniformLocation(prog.program, "u_glyphMercSize"),
          glyphMercSize,
        );
      }
      if (this.matrixMode && projData) {
        setProjectionUniforms(gl, prog.proj, projData);
      }
    };

    // outline first, fill on top (non-sprite mode only)
    if (this.outlinePrograms && this.opts.outlineWidth > 0) {
      const outline = this.outlinePrograms.get(shaderData);
      const boost =
        1.0 + (this.opts.outlineWidth * 2) / Math.max(1, this.opts.glyphSize);
      setCommon(outline, boost);
      gl.uniform3f(
        gl.getUniformLocation(outline.program, "u_color"),
        this.opts.outlineColor[0],
        this.opts.outlineColor[1],
        this.opts.outlineColor[2],
      );
      gl.drawArraysInstanced(
        gl.TRIANGLES,
        0,
        this.vertsPerGlyph,
        this.instanceCount,
      );
    }

    const fill = this.fillPrograms.get(shaderData);
    setCommon(fill, 1.0);
    gl.drawArraysInstanced(
      gl.TRIANGLES,
      0,
      this.vertsPerGlyph,
      this.instanceCount,
    );
  }

  private drawInternal(
    projData: GlobeProjectionData | null,
    shaderData: GlobeShaderData | null,
  ): void {
    if (this.disposed) return;
    if (this.canvasW <= 1 || this.canvasH <= 1) return;
    const tStart = performance.now();
    if (this.view.length === 0 || !this.layout) {
      this.clearCanvas();
      this.onFrame?.(performance.now() - tStart);
      return;
    }
    // sprite sheet still loading: nothing to draw yet
    if (this.spriteMode && !this.spriteTex) {
      this.clearCanvas();
      this.onFrame?.(performance.now() - tStart);
      return;
    }
    const atlas = this.prepareAtlas();
    if (!atlas || !atlas.ready) {
      this.clearCanvas();
      this.onFrame?.(performance.now() - tStart);
      return;
    }
    this.drawGlyphs(atlas.tex, projData, shaderData);
    this.onFrame?.(performance.now() - tStart);
  }
}
