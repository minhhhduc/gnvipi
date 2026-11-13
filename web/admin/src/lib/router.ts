import type { Page } from "./context";

/** The ?page= value exactly as the URL carries it ("" when absent). */
export function rawPageFromUrl(search: string): string {
  return new URLSearchParams(search).get("page") ?? "";
}

export function pageFromUrl(search: string): Page {
  const value = rawPageFromUrl(search);
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