/**
 * Theme selection: follow the system, or pin dark or light.
 *
 * Two different things are tracked deliberately. The SETTING is what the
 * operator chose ("system" | "dark" | "light") and is what persists; the
 * resolved THEME is what is actually on screen. Storing only the resolved
 * theme would silently convert "follow my system" into a pin the first time
 * the page loaded, and the dashboard would then ignore the OS forever.
 *
 * The attribute goes on <html> so CSS custom properties resolve for the whole
 * document, including anything portalled outside the React root. Charts draw
 * to a canvas and cannot inherit CSS, so a theme change has to do three things
 * in order: set the attribute, drop the token cache, then register that
 * theme's ECharts palette from the freshly-resolved values. Doing them out of
 * order registers the previous theme's colours under the new name.
 */
import { useCallback, useEffect, useState } from "react";
import { registerOrbitTheme } from "./echarts";
import { resetTokens } from "./tokens";

/** What is on screen. */
export type ThemeName = "dark" | "light";
/** What the operator chose. "system" tracks the OS and keeps tracking it. */
export type ThemeSetting = ThemeName | "system";

const STORAGE_KEY = "orbit.theme";
const DARK_QUERY = "(prefers-color-scheme: dark)";

/** The OS preference, defaulting to dark where it cannot be determined —
 *  which matches this interface's resting state. */
export function systemTheme(): ThemeName {
  return window.matchMedia?.(DARK_QUERY).matches === false ? "light" : "dark";
}

/** The stored setting, defaulting to following the system. */
export function initialSetting(): ThemeSetting {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "light" || saved === "dark" || saved === "system") return saved;
  } catch {
    // Private mode or a blocked store: fall through to the default rather than
    // failing to render.
  }
  return "system";
}

export function resolveTheme(setting: ThemeSetting): ThemeName {
  return setting === "system" ? systemTheme() : setting;
}

/** Fired after a theme is fully applied. Charts listen for it: an ECharts
 *  instance binds its theme at init, so it must be rebuilt rather than
 *  restyled. */
export const THEME_EVENT = "orbit:theme";

/** Applies a theme to the document and prepares its chart palette. */
export function applyTheme(theme: ThemeName): void {
  document.documentElement.dataset.theme = theme;
  resetTokens();
  registerOrbitTheme(theme);
  window.dispatchEvent(new CustomEvent(THEME_EVENT, { detail: theme }));
}

/** Cycle order for the header control: system → light → dark → system. */
const NEXT: Record<ThemeSetting, ThemeSetting> = {
  system: "light",
  light: "dark",
  dark: "system",
};

export interface ThemeControl {
  /** What the operator chose. */
  setting: ThemeSetting;
  /** What is on screen right now. */
  theme: ThemeName;
  setSetting: (s: ThemeSetting) => void;
  /** Advances to the next setting. */
  cycle: () => void;
}

export function useTheme(): ThemeControl {
  const [setting, setSettingState] = useState<ThemeSetting>(initialSetting);
  const [theme, setTheme] = useState<ThemeName>(() => resolveTheme(initialSetting()));

  // Apply and persist the choice.
  useEffect(() => {
    const resolved = resolveTheme(setting);
    setTheme(resolved);
    applyTheme(resolved);
    try {
      localStorage.setItem(STORAGE_KEY, setting);
    } catch {
      // Persistence is a convenience; the session still works without it.
    }
  }, [setting]);

  // Follow the OS while the setting says to. Registered regardless and gated
  // inside, so switching back to "system" starts tracking again without
  // needing a reload.
  useEffect(() => {
    const mq = window.matchMedia?.(DARK_QUERY);
    if (!mq) return;
    const onChange = () => {
      if (setting !== "system") return;
      const resolved = systemTheme();
      setTheme(resolved);
      applyTheme(resolved);
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [setting]);

  const setSetting = useCallback((s: ThemeSetting) => setSettingState(s), []);
  const cycle = useCallback(() => setSettingState((s) => NEXT[s]), []);
  return { setting, theme, setSetting, cycle };
}

/** The theme currently applied to the document. Read by chart instances at
 *  init, which is why it comes from the DOM rather than React state — the
 *  attribute is the single source of truth once applyTheme has run. */
export function currentTheme(): ThemeName {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}
