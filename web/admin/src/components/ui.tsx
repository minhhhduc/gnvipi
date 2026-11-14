import { createContext } from "preact";
import { useContext, useEffect } from "preact/hooks";
import type { ComponentChildren } from "preact";

export interface UiApi {
  toast: (message: string, kind?: "ok" | "bad") => void;
  setBanner: (message: string | null, retry?: () => void, owner?: string) => void;
}

export const UiContext = createContext<UiApi>({
  toast: () => {},
  setBanner: () => {},
});

export function useUi(): UiApi {
  return useContext(UiContext);
}

export const ThemeContext = createContext<{ theme: "light" | "dark"; toggle: () => void }>({
  theme: "light",
  toggle: () => {},
});

export function IconChevronUp() {
  return (
    <svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true">
      <path d="M4 10l4-4 4 4" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
  );
}

export function IconChevronDown() {
  return (
    <svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true">
      <path d="M4 6l4 4 4-4" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
  );
}

export function IconX() {
  return (
    <svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true">
      <path d="M4 4l8 8M12 4l-8 8" stroke-linecap="round" />
    </svg>
  );
}

export function IconPlus() {
  return (
    <svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true">
      <path d="M8 3v10M3 8h10" stroke-linecap="round" />
    </svg>
  );
}

export function IconDots() {
  return (
    <svg viewBox="0 0 16 16" width="14" height="14" fill="currentColor" aria-hidden="true">
      <circle cx="3" cy="8" r="1.4" />
      <circle cx="8" cy="8" r="1.4" />
      <circle cx="13" cy="8" r="1.4" />
    </svg>
  );
}

export function IconWarn() {
  return (
    <svg viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">
      <path d="M8 2l6 11H2z" stroke-linejoin="round" />
      <path d="M8 6v3.2M8 11.4v.2" stroke-linecap="round" />
    </svg>
  );
}

export function Switch({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      class="switch"
      disabled={disabled}
      onClick={() => onChange(!checked)}
    />
  );
}

export function PageHeader({
  title,
  sub,
  children,
}: {
  title: string;
  sub: string;
  children?: ComponentChildren;
}) {
  const { theme, toggle } = useContext(ThemeContext);
  return (
    <div class="mb-4 flex items-start gap-4">
      <div>
        <h1 class="text-[23px] font-bold tracking-[-0.03em]">{title}</h1>
        <div class="mt-0.5 text-[13px] text-dim">{sub}</div>
      </div>
      <div class="ml-auto flex flex-wrap items-center justify-end gap-2.5">
        {children}
        <span class="flex items-center gap-1.5 text-xs text-dim">
          {theme === "dark" ? "Dark" : "Light"}
          <button
            type="button"
            class="switch"
            role="switch"
            aria-checked={theme === "dark"}
            aria-label="Toggle dark theme"
            title="Toggle dark theme"
            onClick={toggle}
          />
        </span>
      </div>
    </div>
  );
}

export function Card({ children, class: cls = "" }: { children: ComponentChildren; class?: string }) {
  return <div class={`card ${cls}`}>{children}</div>;
}

export function CardHead({ title, sub, children }: { title: string; sub?: string; children?: ComponentChildren }) {
  return (
    <div class="flex items-center gap-2.5 border-b border-line px-[15px] py-[13px]">
      <h2 class="text-sm font-semibold tracking-[-0.01em]">{title}</h2>
      {sub && <span class="text-xs text-dim">{sub}</span>}
      {children && <div class="ml-auto flex items-center gap-2">{children}</div>}
    </div>
  );
}

export function Empty({ title, body }: { title: string; body: string }) {
  return (
    <div class="flex flex-col items-center justify-center gap-1 px-4 py-11 text-center text-dim">
      <span class="text-[15px] font-semibold text-fg">{title}</span>
      <span class="text-xs">{body}</span>
    </div>
  );
}

export function Dialog({
  title,
  children,
  confirmLabel,
  onConfirm,
  onCancel,
  danger,
  busy,
  dismissible = true,
}: {
  title: string;
  children: ComponentChildren;
  confirmLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
  danger?: boolean;
  busy?: boolean;
  /** When false, only the confirm button closes the dialog (no backdrop/Escape). */
  dismissible?: boolean;
}) {
  useEffect(() => {
    if (!dismissible) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [dismissible, onCancel]);

  return (
    <div class="fixed inset-0 z-40 flex items-center justify-center bg-black/45 p-6" onClick={dismissible ? onCancel : undefined}>
      <div
        class="w-[420px] max-w-full rounded-xl border border-line bg-panel shadow-xl"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
      >
        <div class="border-b border-line px-4 py-3.5 font-bold">{title}</div>
        <div class="px-4 py-3.5 text-[13px] text-dim">{children}</div>
        <div class="flex justify-end gap-2 border-t border-line px-4 py-2.5">
          {dismissible && (
            <button class="btn" onClick={onCancel} disabled={busy}>
              Cancel
            </button>
          )}
          <button class={`btn ${danger ? "btn-danger" : "btn-primary"}`} onClick={onConfirm} disabled={busy}>
            {busy ? "Working…" : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

export function Pager({
  from,
  to,
  total,
  note,
  hasPrev,
  hasNext,
  onPrev,
  onNext,
}: {
  from: number;
  to: number;
  total?: number;
  note?: string;
  hasPrev: boolean;
  hasNext: boolean;
  onPrev: () => void;
  onNext: () => void;
}) {
  return (
    <div class="flex items-center gap-2 border-t border-line px-4 py-2.5 text-xs text-dim">
      <span>Showing {from}–{to}{total != null ? ` of ${total}` : ""}{note ? ` · ${note}` : ""}</span>
      <div class="ml-auto flex gap-2">
        <button class="btn btn-sm" disabled={!hasPrev} onClick={onPrev}>Previous</button>
        <button class="btn btn-sm" disabled={!hasNext} onClick={onNext}>Next</button>
      </div>
    </div>
  );
}
