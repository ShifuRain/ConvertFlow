// Pure helper (no DOM) so it can be unit tested with `npm test`.
export interface PresetLike { id: string }

/** Text of the compatibility note shown when AV1 is offered, or "" when none applies. Only presets that are
 *  actually listed are named, so the note never points at presets this installation does not have. */
export function av1Notice(presets: PresetLike[]): string {
  const has = (id: string) => presets.some(p => p.id === id);
  if (!has("video-av1")) return "";
  const mp4 = has("video-h264") || has("video-h265");
  const vp9 = has("video-vp9");
  const base = "Not all players can decode AV1.";
  if (vp9 && mp4) return `${base} VP9 and MP4 (H.264/H.265) presets remain available if you need wider compatibility.`;
  if (mp4) return `${base} MP4 (H.264/H.265) presets remain available if you need wider compatibility.`;
  if (vp9) return `${base} The VP9 preset remains available if you need wider compatibility.`;
  return base;
}

/** Wait in human terms for a Retry-After value in seconds ("" when absent or invalid). */
export function retryWait(retryAfter: string | null | undefined): string {
  const s = Number.parseInt((retryAfter ?? "").trim(), 10);
  if (!Number.isFinite(s) || s <= 0 || String(s) !== (retryAfter ?? "").trim()) return "";
  if (s < 90) return `${s} seconds`;
  const m = Math.ceil(s / 60);
  return m === 1 ? "1 minute" : `${m} minutes`;
}

/** Text shown when the server refused an upload because a quota is exhausted (HTTP 429), or "" for any
 *  other answer. The server's detail (problem+json, a fixed message) is shown as text only, with the
 *  Retry-After hint. A 503 "server busy" is not a quota: the upload queue retries those by itself. */
export function quotaNotice(status: number, body: string, retryAfter: string | null | undefined): string {
  if (status !== 429) return "";
  let detail = "";
  try {
    const p = JSON.parse(body);
    if (p && typeof p.detail === "string") detail = p.detail;
  } catch { /* not JSON */ }
  const wait = retryWait(retryAfter);
  return `Upload limit reached${detail ? `: ${detail}` : "."}${wait ? ` Try again in about ${wait}.` : ""}`;
}
