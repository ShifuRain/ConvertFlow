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
