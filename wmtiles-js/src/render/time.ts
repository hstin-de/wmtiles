import type { WMT } from "../reader.js";

// Derived from fractional time t on the WMT axis.
//   tF   floor(t), clamped. "from" frame, weight (1 - frac)
//   tC   ceil(t), clamped. "to" frame, weight frac. equals tF at last step
//   frac lerp weight in [0, 1). 0 when lerp disabled or tF is the last step
//   tP   prefetch target one step past tC/tF, -1 if disabled/oob/duplicate
export interface TimeWindow {
  tF: number;
  tC: number;
  frac: number;
  tP: number;
}

export interface TimeWindowOptions {
  disableTimeLerp?: boolean;
  prefetchNext?: boolean;
}

export function computeTimeWindow(
  wmt: WMT,
  t: number,
  opts: TimeWindowOptions,
): TimeWindow {
  const maxStep = wmt.timeStepCount - 1;
  const tF = Math.max(0, Math.min(maxStep, Math.floor(t)));
  const tCRaw = tF + 1;
  const tC = Math.min(maxStep, tCRaw);
  const frac = opts.disableTimeLerp
    ? 0
    : tCRaw > maxStep
      ? 0
      : t - tF;
  const tPRaw = (frac > 0 ? tC : tF) + 1;
  const tP =
    opts.prefetchNext &&
    tPRaw <= maxStep &&
    tPRaw !== tF &&
    tPRaw !== tC
      ? tPRaw
      : -1;
  return { tF, tC, frac, tP };
}
