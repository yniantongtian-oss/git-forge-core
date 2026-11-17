// A logical clock. Every timestamp the forge writes is the fixed epoch plus a counter that advances once per
// use, so the state two runs of the same request sequence build is byte-identical: nothing reads the wall clock.

/** 2025-01-01T00:00:00Z, the day one of the suite's scenario. */
export const EPOCH_SEC = 1_735_689_600;

let ticks = 0;

/** The next logical time, in unix seconds. */
export function nextTime(): number {
  ticks += 1;
  return EPOCH_SEC + ticks;
}

/** An ISO-8601 UTC timestamp without milliseconds, as GitHub writes them. */
export function iso(sec: number): string {
  return new Date(sec * 1000).toISOString().replace('.000Z', 'Z');
}

/** The next logical time as an ISO timestamp. */
export function nextIso(): string {
  return iso(nextTime());
}
