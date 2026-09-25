// Entry point: hash router, the forget-token control, and tab visibility.
import { replace } from "./dom.js";
import { setBanner, clearBanner } from "./banners.js";
import { forgetToken, hasToken, onTokenChange, redeemSignIn } from "./api.js";
import * as dashboard from "./dashboard.js";
import * as eventsPage from "./events.js";
import * as configPage from "./config.js";

const routes = { dashboard, events: eventsPage, config: configPage };
let current = null;

function parseHash() {
  const parts = location.hash.replace(/^#\/?/, "").split("/").filter(Boolean);
  const name = routes[parts[0]] ? parts[0] : "dashboard";
  return { name, rest: parts.slice(1) };
}

function route() {
  const { name, rest } = parseHash();
  const main = document.getElementById("main");
  if (current && current.name === name && current.page.update) {
    current.page.update(rest);
    return;
  }
  if (current && current.page.unmount) current.page.unmount();
  for (const a of document.querySelectorAll("nav a")) {
    if (a.dataset.route === name) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  replace(main);
  const page = routes[name].mount(main, rest) || {};
  current = { name, page };
  document.title = (page.title ? page.title + " | " : "") + "Benchwarmer";
}

function syncTokenButton(has) {
  document.getElementById("forget-token").hidden = !has;
}

document.getElementById("forget-token").addEventListener("click", () => {
  forgetToken();
  setBanner("token", "info", "Management token forgotten. You will be asked again for the next change.");
  setTimeout(() => clearBanner("token"), 5000);
});
onTokenChange(syncTokenButton);
syncTokenButton(hasToken());

// Pages stop polling while the tab is hidden and refresh when it returns.
document.addEventListener("visibilitychange", () => {
  if (current && current.page.visibility) current.page.visibility(!document.hidden);
});
window.addEventListener("hashchange", route);

// The tray's "Sign in to change settings" opens #signin=<code>. Take the
// code out of the address bar and history first, then redeem it.
async function start() {
  const m = location.hash.match(/^#signin=([A-Za-z0-9_-]{32,128})$/);
  if (m) {
    history.replaceState(null, "", location.pathname + location.search + "#/");
    try {
      await redeemSignIn(m[1]);
      setBanner("token", "info", "Signed in as administrator for this browser tab.");
      setTimeout(() => clearBanner("token"), 5000);
    } catch (e) {
      setBanner("token", "error", "Sign-in failed: " + e.message + " Use Sign in to change settings in the tray again.");
    }
  }
  route();
}
start();
