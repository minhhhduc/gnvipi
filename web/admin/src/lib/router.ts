import type { Page } from "./context";

export function pageFromUrl(search: string): Page {
  const value = new URLSearchParams(search).get("page");
  switch (value) {
    case "stats":
    case "models":
    case "config":
    case "chromes":
      return value;
    default:
      return "stats";
  }
}
