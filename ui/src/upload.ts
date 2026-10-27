// Pure helpers for the two upload paths (no DOM, no network), so they can be unit tested with
// `npm test`:
//   multipart  POST /api/v1/jobs with multipart/form-data (the bytes go through the API)
//   presigned  POST /api/v1/jobs with JSON -> form POST of the returned fields, then the file, to
//              the object store -> POST /api/v1/jobs/{id}/complete (the bytes bypass the API)

export type UploadMode = "presigned" | "multipart";

/** Header on GET /api/v1/presets that tells the UI which path to use. */
export const UPLOAD_MODE_HEADER = "ConvertFlow-Upload-Mode";

/** Anything but an explicit "presigned" means the long-standing multipart path. */
export function uploadMode(header: string | null | undefined): UploadMode {
  return (header ?? "").trim().toLowerCase() === "presigned" ? "presigned" : "multipart";
}

/** The preset selection as the multipart form sends it: a plain preset or an options JSON object. */
export interface Field { name: string; value: string }

/** JSON body of a direct-upload create request. */
export function presignBody(field: Field, file: { name: string; size: number }): string {
  const body: Record<string, unknown> = {};
  if (field.name === "options") body.options = JSON.parse(field.value);
  else body.preset = field.value;
  body.filename = file.name;
  body.size = file.size;
  return JSON.stringify(body);
}

export interface Presigned { id: string; url: string; fields: Record<string, string> }

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** Parses the 201 answer of a direct-upload create. Anything unexpected (a non-http(s) URL, a key
 *  that is not this job's input, non-string fields) yields null, and the upload is not attempted. */
export function parsePresigned(text: string): Presigned | null {
  let p: unknown;
  try { p = JSON.parse(text); } catch { return null; }
  if (!p || typeof p !== "object") return null;
  const o = p as { id?: unknown; upload?: { url?: unknown; fields?: unknown } };
  if (typeof o.id !== "string" || !UUID.test(o.id) || !o.upload || typeof o.upload !== "object") return null;
  const { url, fields } = o.upload;
  if (typeof url !== "string" || !/^https?:\/\/[^/?#\s]+/i.test(url)) return null;
  if (!fields || typeof fields !== "object" || Array.isArray(fields)) return null;
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(fields as Record<string, unknown>)) {
    if (typeof v !== "string") return null;
    out[k] = v;
  }
  if (out.key !== `in/${o.id}`) return null;
  return { id: o.id, url, fields: out };
}

/** The form posted to the object store: every signed field first, the file LAST (S3 ignores
 *  fields that follow the file). A field named "file" from the server is never forwarded. */
export function uploadForm(fields: Record<string, string>, file: Blob, filename: string): FormData {
  const fd = new FormData();
  for (const [k, v] of Object.entries(fields)) {
    if (k.toLowerCase() !== "file") fd.append(k, v);
  }
  fd.append("file", file, filename);
  return fd;
}

/** The <Code> of an S3 XML error body ("" if there is none). */
export function storeErrorCode(body: string): string {
  const m = /<Code>\s*([A-Za-z0-9.]{1,64})\s*<\/Code>/.exec(body);
  return m ? m[1] : "";
}

/** Whether a failed form POST to the store is worth repeating with the same (still valid) form. */
export function storeRetryable(status: number, body: string): boolean {
  const code = storeErrorCode(body);
  return status === 503 || status === 500 || code === "SlowDown" || code === "ServiceUnavailable" || code === "InternalError";
}

/** A user-facing message for a failed form POST to the object store. Server text is never shown
 *  verbatim: only the S3 error code selects a message. */
export function storeErrorMessage(status: number, body: string): string {
  const code = storeErrorCode(body);
  switch (code) {
    case "EntityTooLarge": return "File is larger than the server's upload limit.";
    case "EntityTooSmall": return "File is empty.";
    case "AccessDenied":
    case "ExpiredToken":
    case "RequestExpired":
      return "The storage server refused the upload (the upload link may have expired). Try again.";
    case "SignatureDoesNotMatch":
    case "InvalidAccessKeyId":
      return "The storage server rejected the upload signature. Contact the administrator if this persists.";
    case "NoSuchBucket": return "The storage bucket does not exist. Contact the administrator.";
    case "SlowDown":
    case "ServiceUnavailable":
    case "InternalError":
      return "The storage server is busy or unavailable. Try again later.";
  }
  if (status === 0) return "Could not reach the storage server (network error, or the browser blocked the request).";
  if (status === 413) return "File is larger than the server's upload limit.";
  return `Upload to storage failed (HTTP ${status}${code ? `, ${code}` : ""})`;
}

/** Message for an API error answer (problem+json detail or title, else a generic text). */
export function problemText(status: number, body: string): string {
  try {
    const p = JSON.parse(body);
    const t = p && (p.detail || p.title);
    if (t) return String(t);
  } catch { /* not JSON */ }
  if (status === 413) return "File is larger than the server's upload limit.";
  return `Upload failed (HTTP ${status})`;
}

/** Whether /complete may be repeated (it is idempotent): network errors and server-side faults. */
export function completeRetryable(status: number): boolean {
  return status === 0 || status === 502 || status === 503 || status === 504 || status === 500;
}
