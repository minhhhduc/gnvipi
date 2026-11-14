import { createContext } from "preact";
import { useContext } from "preact/hooks";
import type { Stats } from "./types";

export type Page = "stats" | "playground" | "models" | "config" | "chromes" | "tokens" | "users" | "audit";

export interface GatewayContextValue {
  stats: Stats | null;
  statsUpdated: number | null;
  refreshStats: () => void;
  navigate: (page: Page) => void;
  /** Increments on every nav click so a page can reset its own drill-down. */
  navSeq: number;
}

export const GatewayContext = createContext<GatewayContextValue>({
  stats: null,
  statsUpdated: null,
  refreshStats: () => {},
  navigate: () => {},
  navSeq: 0,
});

export function useGateway(): GatewayContextValue {
  return useContext(GatewayContext);
}

export function totals(stats: Stats | null) {
  const models = stats?.models || [];
  const requests = models.reduce((n, m) => n + (m.requests || 0), 0);
  const errors = models.reduce((n, m) => n + (m.errors || 0), 0);
  return { requests, errors };
}
