// Page-wide banners, keyed so a repeated condition replaces its own message.
import { h, replace } from "./dom.js";

const banners = new Map();

// setBanner shows (or replaces) the banner for key. level is "info",
// "warn", or "error".
export function setBanner(key, level, ...content) {
  banners.set(key, h("div", { class: "banner " + level, role: level === "error" ? "alert" : "status" }, ...content));
  draw();
}

export function clearBanner(key) {
  if (banners.delete(key)) draw();
}

function draw() {
  replace(document.getElementById("banners"), [...banners.values()]);
}
