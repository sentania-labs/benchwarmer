// Entry point: hash router, the forget-token control, and tab visibility.
import { replace } from "./dom.js";
import { setBanner, clearBanner } from "./banners.js";
import { forgetToken, hasToken, onTokenChange } from "./api.js";
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
route();
