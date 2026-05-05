import type { WMT } from "wmtiles";
import { renderTile } from "./colormap";

declare const L: typeof import("leaflet");

export interface LayerState {
  variable: string;
  t: number;
  vmin: number;
  vmax: number;
  gen: number;
}

export interface WMTLayer extends L.GridLayer {
  state: LayerState;
  setState(patch: Partial<LayerState>): void;
}

export function makeWMTLayer(wmt: WMT): WMTLayer {
  const Layer = L.GridLayer.extend({
    options: {
      tileSize: wmt.pixSize,
      minZoom: wmt.header.minZoom,
      maxZoom: wmt.header.maxZoom,
      noWrap: false,
      opacity: 0.85,
    },
    state: {
      variable: wmt.catalog[0].name,
      t: 0,
      vmin: 0,
      vmax: 1,
      gen: 0,
    } as LayerState,
    setState(this: WMTLayer, patch: Partial<LayerState>) {
      Object.assign(this.state, patch);
      this.state.gen++;
      this.redraw();
    },
    createTile(this: WMTLayer, coords: L.Coords, done: L.DoneCallback) {
      const canvas = document.createElement("canvas");
      canvas.width = wmt.pixSize;
      canvas.height = wmt.pixSize;
      const ctx = canvas.getContext("2d");
      if (!ctx) {
        done(new Error("no 2d context"), canvas);
        return canvas;
      }
      const myGen = this.state.gen;
      const { variable, t, vmin, vmax } = this.state;

      wmt
        .getTilePixels(variable, t, coords.z, coords.x, coords.y)
        .then((pixels) => {
          if (myGen !== this.state.gen) return;
          if (!pixels) {
            done(undefined, canvas);
            return;
          }
          renderTile(pixels, wmt.pixSize, vmin, vmax, ctx);
          done(undefined, canvas);
        })
        .catch((err: Error) => {
          console.error("tile decode failed", coords, err);
          done(err, canvas);
        });

      return canvas;
    },
  });
  return new Layer() as WMTLayer;
}
