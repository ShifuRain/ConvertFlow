import test from "node:test";
import assert from "node:assert/strict";
import {
  completeRetryable, parsePresigned, presignBody, problemText, storeErrorCode, storeErrorMessage, storeRetryable,
  uploadForm, uploadMode,
} from "../src/upload.ts";

const ID = "0b5c3a8e-6f1d-4c1e-9a55-2f7d2c1b9e10";
const created = (over = {}) => JSON.stringify({
  id: ID,
  upload: { url: "http://127.0.0.1:9000/convertflow", fields: { key: `in/${ID}`, policy: "p", "x-amz-signature": "s" } },
  expires_at: "2026-10-26T12:00:00Z",
  ...over,
});

test("upload mode: only an explicit 'presigned' selects the direct path", () => {
  assert.equal(uploadMode("presigned"), "presigned");
  assert.equal(uploadMode(" Presigned "), "presigned");
  for (const h of [null, undefined, "", "multipart", "presigned,multipart", "direct"]) assert.equal(uploadMode(h), "multipart");
});

test("create body carries preset or options, the filename and the size", () => {
  assert.deepEqual(JSON.parse(presignBody({ name: "preset", value: "audio-flac" }, { name: "a.wav", size: 42 })),
    { preset: "audio-flac", filename: "a.wav", size: 42 });
  const opts = { preset: "video-av1", crf: 30, max_height: 720 };
  assert.deepEqual(JSON.parse(presignBody({ name: "options", value: JSON.stringify(opts) }, { name: "b.mkv", size: 7 })),
    { options: opts, filename: "b.mkv", size: 7 });
});

test("presigned answer: accepted only with a UUID, an http(s) URL, string fields and this job's key", () => {
  const p = parsePresigned(created());
  assert.equal(p.id, ID);
  assert.equal(p.url, "http://127.0.0.1:9000/convertflow");
  assert.equal(p.fields.key, `in/${ID}`);
  for (const bad of [
    "not json", "null", "[]", JSON.stringify({ id: ID }),
    created({ id: "../x" }),
    created({ upload: { url: "javascript:alert(1)", fields: { key: `in/${ID}` } } }),
    created({ upload: { url: "/relative", fields: { key: `in/${ID}` } } }),
    created({ upload: { url: "https://s3.example", fields: { key: "in/other" } } }),
    created({ upload: { url: "https://s3.example", fields: { key: `in/${ID}`, n: 1 } } }),
    created({ upload: { url: "https://s3.example", fields: [] } }),
  ]) assert.equal(parsePresigned(bad), null, bad);
});

test("store form: signed fields first, the file last, a server 'file' field dropped", async () => {
  const fd = uploadForm({ key: `in/${ID}`, policy: "p", "x-amz-signature": "s", file: "evil" }, new Blob(["hello"]), "clip.wav");
  const entries = [...fd.entries()];
  assert.deepEqual(entries.map(([k]) => k), ["key", "policy", "x-amz-signature", "file"]);
  const last = entries.at(-1)[1];
  assert.equal(typeof last, "object");
  assert.equal(last.name, "clip.wav");
  assert.equal(await last.text(), "hello");
});

const s3err = (code) => `<?xml version="1.0" encoding="UTF-8"?>\n<Error><Code>${code}</Code><Message>Your proposed upload exceeds the maximum allowed size</Message><Key>in/x</Key></Error>`;

test("store errors map to clear messages without echoing server text", () => {
  assert.equal(storeErrorCode(s3err("EntityTooLarge")), "EntityTooLarge");
  assert.equal(storeErrorCode("<html>no</html>"), "");
  assert.match(storeErrorMessage(400, s3err("EntityTooLarge")), /larger than the server's upload limit/);
  assert.match(storeErrorMessage(400, s3err("EntityTooSmall")), /empty/);
  assert.match(storeErrorMessage(403, s3err("AccessDenied")), /refused the upload.*expired/);
  assert.match(storeErrorMessage(404, s3err("NoSuchBucket")), /bucket does not exist/);
  assert.match(storeErrorMessage(0, ""), /Could not reach the storage server/);
  assert.match(storeErrorMessage(413, ""), /upload limit/);
  assert.equal(storeErrorMessage(418, s3err("Teapot")), "Upload to storage failed (HTTP 418, Teapot)");
  assert.doesNotMatch(storeErrorMessage(400, s3err("EntityTooLarge")), /proposed upload/);
  // a hostile code cannot inject markup or long text
  assert.equal(storeErrorCode("<Code><img src=x></Code>"), "");
});

test("only transient store and API faults are retried", () => {
  assert.ok(storeRetryable(503, ""));
  assert.ok(storeRetryable(400, s3err("SlowDown")));
  assert.ok(!storeRetryable(400, s3err("EntityTooLarge")));
  assert.ok(!storeRetryable(403, s3err("AccessDenied")));
  for (const s of [0, 500, 502, 503, 504]) assert.ok(completeRetryable(s), String(s));
  for (const s of [200, 400, 404, 409, 413]) assert.ok(!completeRetryable(s), String(s));
});

test("API problem text: detail, then title, then a generic message", () => {
  assert.equal(problemText(409, JSON.stringify({ title: "Conflict", detail: "Upload missing" })), "Upload missing");
  assert.equal(problemText(409, JSON.stringify({ title: "Conflict" })), "Conflict");
  assert.equal(problemText(413, "<html>"), "File is larger than the server's upload limit.");
  assert.equal(problemText(500, ""), "Upload failed (HTTP 500)");
});
