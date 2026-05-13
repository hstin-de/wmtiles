// LRU texture cache. Lowest `used` counter evicts first; get() bumps it.

interface CacheEntry {
  tex: WebGLTexture;
  used: number;
}

export class TileTextureCache {
  private readonly map = new Map<string, CacheEntry>();
  private frame = 0;

  constructor(
    private readonly gl: WebGL2RenderingContext,
    private readonly capacity: number,
  ) {}

  size(): number {
    return this.map.size;
  }

  has(key: string): boolean {
    return this.map.has(key);
  }

  get(key: string): WebGLTexture | null {
    const e = this.map.get(key);
    if (!e) return null;
    e.used = ++this.frame;
    return e.tex;
  }

  peek(key: string): boolean {
    return this.map.has(key);
  }

  set(key: string, tex: WebGLTexture): void {
    this.map.set(key, { tex, used: ++this.frame });
    this.evictIfNeeded();
  }

  private evictIfNeeded(): void {
    if (this.map.size <= this.capacity) return;
    const entries = [...this.map.entries()];
    entries.sort((a, b) => a[1].used - b[1].used);
    const drop = this.map.size - this.capacity;
    for (let i = 0; i < drop; i++) {
      this.gl.deleteTexture(entries[i][1].tex);
      this.map.delete(entries[i][0]);
    }
  }

  dispose(): void {
    for (const e of this.map.values()) this.gl.deleteTexture(e.tex);
    this.map.clear();
  }
}
