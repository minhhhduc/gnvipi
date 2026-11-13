import { createContext } from "preact";
import { useContext } from "preact/hooks";
import type { Stats } from "./types";

export type Page = "stats" | "models" | "config" | "chromes";

export interface GatewayContextValue {
  stats: Stats | null;
  statsUpdated: number | null;
  refreshStats: () => void;
  navigate: (page: Page) => void;
}

export const GatewayContext = createContext<GatewayContextValue>({
  stats: null,
  statsUpdated: null,
  refreshStats: () => {},
  navigate: () => {},
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
