// Tiny fetch helpers for the admin API. token is passed in (App holds it as
// reactive state and persists it to localStorage).

export function hdr(token, extra = {}) {
  return token ? { ...extra, Authorization: 'Bearer ' + token } : { ...extra };
}

export async function getJSON(token, url) {
  const r = await fetch(url, { headers: hdr(token) });
  if (!r.ok) throw new Error(url + ' → ' + r.status);
  return r.json();
}

export async function sendJSON(token, method, url, body) {
  const r = await fetch(url, {
    method,
    headers: hdr(token, { 'Content-Type': 'application/json' }),
    body: body == null ? undefined : JSON.stringify(body),
  });
  let j = {};
  try { j = await r.json(); } catch {}
  if (!r.ok) throw new Error(j.error || url + ' → ' + r.status);
  return j;
}
