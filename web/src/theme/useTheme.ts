/**
 * Theme selection: dark (the default) or light.
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

export type ThemeName = "dark" | "light";

const STORAGE_KEY = "orbit.theme";

/** The theme to start in: the operator's last choice, else dark. */
export function initialTheme(): ThemeName {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "light" || saved === "dark") return saved;
  } catch {
    // Private mode or a blocked store: fall through to the default rather than
    // failing to render.
  }
  return "dark";
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

export function useTheme(): { theme: ThemeName; setTheme: (t: ThemeName) => void; toggle: () => void } {
  const [theme, setThemeState] = useState<ThemeName>(initialTheme);

  useEffect(() => {
    applyTheme(theme);
    try {
      localStorage.setItem(STORAGE_KEY, theme);
    } catch {
      // Persistence is a convenience; the session still works without it.
    }
  }, [theme]);

  const setTheme = useCallback((t: ThemeName) => setThemeState(t), []);
  const toggle = useCallback(() => setThemeState((t) => (t === "dark" ? "light" : "dark")), []);
  return { theme, setTheme, toggle };
}

/** The theme currently applied to the document. Read by chart instances at
 *  init, which is why it comes from the DOM rather than React state — the
 *  attribute is the single source of truth once applyTheme has run. */
export function currentTheme(): ThemeName {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}
