import type { ComponentChildren } from "preact";
import { totals, useGateway, type Page } from "../lib/context";

const NAV: { page: Page; label: string }[] = [
  { page: "stats", label: "Stats" },
  { page: "models", label: "Models" },
  { page: "config", label: "Config" },
  { page: "chromes", label: "Chrome" },
];

export function HealthPill() {
  const { stats } = useGateway();
  const { requests, errors } = totals(stats);
  let cls = "pill pill-mute";
  let text = "No traffic yet";
  if (requests > 0 && requests < 20) {
    text = `Low traffic · ${requests} requests`;
  } else if (requests >= 20) {
    const rate = errors / requests;
    if (rate === 0) {
      cls = "pill pill-ok";
      text = "Healthy · 100% success";
    } else if (rate < 0.05) {
      cls = "pill pill-warn";
      text = `Degraded · ${(rate * 100).toFixed(1)}% errors`;
    } else {
      cls = "pill pill-bad";
      text = `Failing · ${(rate * 100).toFixed(1)}% errors`;
    }
  }
  return <span class={cls}>{text}</span>;
}

export function Shell({
  page,
  children,
}: {
  page: Page;
  children: ComponentChildren;
}) {
  const { navigate } = useGateway();
  return (
    <div class="flex min-h-screen">
      <aside class="sticky top-0 flex h-screen w-[220px] flex-none flex-col gap-1 self-start overflow-y-auto border-r border-line bg-panel px-[14px] py-[18px]">
        <div class="mb-4 flex items-center gap-2 px-1.5 pt-0.5">
          <span class="grid size-[30px] place-items-center rounded-[9px] bg-accent text-xs font-extrabold text-white">
            GW
          </span>
          <b class="text-[15px] tracking-[-0.02em]">Gateway</b>
        </div>
        <nav class="flex flex-col gap-[3px]">
          {NAV.map((item) => (
            <a
              key={item.page}
              href={`?page=${item.page}`}
              class={`rounded-lg px-2.5 py-2 text-[13px] no-underline ${
                page === item.page
                  ? "bg-accent-soft font-semibold text-accent"
                  : "font-medium text-dim hover:text-fg"
              }`}
              onClick={(e) => {
                e.preventDefault();
                navigate(item.page);
              }}
            >
              {item.label}
            </a>
          ))}
        </nav>
      </aside>
      <main class="min-w-0 max-w-[1240px] flex-1 px-[26px] pt-[22px] pb-[60px]">{children}</main>
    </div>
  );
}
