import test from "node:test";
import assert from "node:assert/strict";
import { av1Notice } from "../src/notice.ts";

const ids = (...a) => a.map(id => ({ id }));

test("no AV1 preset: no note", () => {
  assert.equal(av1Notice(ids("video-vp9", "audio-wav")), "");
  assert.equal(av1Notice([]), "");
});

test("lgpl flavor (AV1 + VP9, no H.264/H.265): never mentions H.264/H.265", () => {
  const t = av1Notice(ids("video-av1", "video-vp9", "audio-flac"));
  assert.match(t, /AV1/);
  assert.match(t, /VP9/);
  assert.doesNotMatch(t, /H\.26[45]|MP4/);
});

test("full flavor lists H.264/H.265 when offered", () => {
  assert.match(av1Notice(ids("video-av1", "video-vp9", "video-h264")), /MP4 \(H\.264\/H\.265\)/);
  assert.match(av1Notice(ids("video-av1", "video-h265")), /MP4 \(H\.264\/H\.265\)/);
});

test("AV1 only: just the base sentence", () => {
  assert.equal(av1Notice(ids("video-av1")), "Not all players can decode AV1.");
});
