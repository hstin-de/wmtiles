import type { Variable, WMT } from "wmtiles";
import { renderTile } from "./colormap";

declare const L: typeof import("leaflet");

export interface LayerState {
  variable: Variable;
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
      tileSize: wmt.tileSize,
      minZoom: wmt.zoomRange.min,
      maxZoom: wmt.zoomRange.max,
      noWrap: false,
      opacity: 0.85,
    },
    state: {
      variable: wmt.variables[0],
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
      canvas.width = wmt.tileSize;
      canvas.height = wmt.tileSize;
      const ctx = canvas.getContext("2d");
      if (!ctx) {
        done(new Error("no 2d context"), canvas);
        return canvas;
      }
      const myGen = this.state.gen;
      const { variable, t, vmin, vmax } = this.state;

      variable
        .tile({ time: t, z: coords.z, x: coords.x, y: coords.y })
        .then((pixels) => {
          if (myGen !== this.state.gen) return;
          if (!pixels) {
            done(undefined, canvas);
            return;
          }
          renderTile(pixels, wmt.tileSize, vmin, vmax, ctx);
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
