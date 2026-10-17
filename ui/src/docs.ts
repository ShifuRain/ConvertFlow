interface Op { summary?: string }

async function main() {
  const res = await fetch("/api/openapi.json");
  const doc = (await res.json()) as { paths: Record<string, Record<string, Op>> };
  const root = document.getElementById("endpoints")!;
  for (const [path, ops] of Object.entries(doc.paths)) {
    for (const [method, op] of Object.entries(ops)) {
      const row = document.createElement("div");
      row.className = "ep";
      const m = document.createElement("span");
      m.className = "method";
      m.textContent = method.toUpperCase();
      const code = document.createElement("code");
      code.textContent = path;
      const s = document.createElement("span");
      s.className = "muted";
      s.textContent = " " + (op.summary ?? "");
      row.append(m, code, s);
      root.append(row);
    }
  }
}
main().catch(() => { /* docs are optional */ });
