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

import { quotaNotice, retryWait } from "../src/notice.ts";

test("quota notice: only for 429, with the server detail and the wait", () => {
  const body = JSON.stringify({ title: "Quota exceeded", status: 429, detail: "You have too many unfinished jobs; wait for one to finish or remove one." });
  assert.equal(quotaNotice(503, body, "30"), "");
  assert.equal(quotaNotice(413, body, null), "");
  assert.equal(quotaNotice(429, body, "60"),
    "Upload limit reached: You have too many unfinished jobs; wait for one to finish or remove one. Try again in about 60 seconds.");
  assert.equal(quotaNotice(429, "not json", null), "Upload limit reached.");
  assert.match(quotaNotice(429, body, "3599"), /about 60 minutes\.$/);
});

test("retry wait parsing", () => {
  assert.equal(retryWait("5"), "5 seconds");
  assert.equal(retryWait("90"), "2 minutes");
  assert.equal(retryWait("61"), "61 seconds");
  assert.equal(retryWait("120"), "2 minutes");
  for (const bad of [null, undefined, "", "0", "-3", "abc", "1.5", "Wed, 21 Oct 2015 07:28:00 GMT"]) assert.equal(retryWait(bad), "");
});
