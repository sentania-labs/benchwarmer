// Management API client. The token (ADR 0007) is asked for only when a
// request returns 401, kept in memory and sessionStorage for this tab (never
// localStorage), sent only as an Authorization header, and never in a URL.

const KEY = "benchwarmer.management_token";
let token = null;
try {
  token = sessionStorage.getItem(KEY);
} catch {
  token = null;
}
const listeners = new Set();

export function hasToken() {
  return !!token;
}

export function onTokenChange(fn) {
  listeners.add(fn);
}

function setToken(t) {
  token = t || null;
  try {
    if (token) sessionStorage.setItem(KEY, token);
    else sessionStorage.removeItem(KEY);
  } catch {
    // Storage blocked: the in-memory copy still works for this page.
  }
  for (const fn of listeners) fn(!!token);
}

export function forgetToken() {
  setToken(null);
}

// ApiError carries the HTTP status and the parsed api.Error body.
export class ApiError extends Error {
  constructor(status, body) {
    super((body && body.error) || "HTTP " + status);
    this.status = status;
    this.body = body || {};
  }
}

// askToken shows the token dialog and resolves to the entered token, or null
// when cancelled.
export function askToken(reason) {
  const dlg = document.getElementById("token-dialog");
  const form = document.getElementById("token-form");
  const input = document.getElementById("token-input");
  const cancel = document.getElementById("token-cancel");
  document.getElementById("token-reason").textContent = reason ||
    "This action needs the Benchwarmer management token.";
  input.value = "";
  return new Promise((resolve) => {
    const done = (v) => {
      form.removeEventListener("submit", onSubmit);
      cancel.removeEventListener("click", onCancel);
      dlg.removeEventListener("cancel", onCancel);
      if (dlg.open) dlg.close();
      input.value = "";
      resolve(v);
    };
    const onSubmit = (e) => {
      e.preventDefault();
      const v = input.value.trim();
      if (v) done(v);
    };
    const onCancel = (e) => {
      e.preventDefault();
      done(null);
    };
    form.addEventListener("submit", onSubmit);
    cancel.addEventListener("click", onCancel);
    dlg.addEventListener("cancel", onCancel);
    dlg.showModal();
    input.focus();
  });
}

async function once(method, path, body) {
  const headers = { Accept: "application/json" };
  if (token) headers.Authorization = "Bearer " + token;
  const init = { method, headers, cache: "no-store", credentials: "omit", referrerPolicy: "no-referrer" };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const res = await fetch(path, init);
  let data = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { error: text.trim() };
    }
  }
  if (!res.ok) throw new ApiError(res.status, data);
  return data;
}

// request performs an API call. With interactive (the default for writes),
// a 401 opens the token dialog and retries until the token works or the user
// cancels. Non-interactive calls just throw the 401 so the caller can offer
// a sign-in control without a dialog popping up on every poll.
export async function request(method, path, body, opts = {}) {
  const interactive = opts.interactive ?? method !== "GET";
  let reason = null;
  for (;;) {
    try {
      return await once(method, path, body);
    } catch (e) {
      if (!(e instanceof ApiError) || e.status !== 401 || !interactive) throw e;
      if (token) {
        setToken(null);
        reason = "The saved token was rejected. Enter the current management token.";
      }
      const t = await askToken(reason);
      if (!t) throw new ApiError(401, { error: "Cancelled: a management token is required.", code: "unauthorized" });
      setToken(t);
    }
  }
}

// signIn asks for a token outside a failing request (for read-only pages
// when loopback trust is off).
export async function signIn(reason) {
  const t = await askToken(reason);
  if (t) setToken(t);
  return !!t;
}

// redeemSignIn exchanges a one-time code from the tray's "Sign in to change
// settings" for the management token, kept for this tab only.
export async function redeemSignIn(code) {
  const res = await once("POST", "api/v1/signin/redeem", { code });
  setToken(res.token);
}

export const get = (p, o) => request("GET", p, undefined, o);
export const put = (p, b, o) => request("PUT", p, b, o);
export const post = (p, b, o) => request("POST", p, b, o);
