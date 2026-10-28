// The dashboard's lint: the Rules of Hooks and effect dependencies only
// (eslint-plugin-react-hooks), over the TypeScript sources. The parser is
// there to read TS and TSX; no other rule is on. Type errors are tsc's
// (npm run typecheck), formatting is prettier's.
import tsParser from "@typescript-eslint/parser";
import reactHooks from "eslint-plugin-react-hooks";

export default [
  { ignores: ["dist/**", "src/gen/**"] },
  {
    files: ["src/**/*.{ts,tsx}"],
    languageOptions: {
      parser: tsParser,
      parserOptions: { ecmaVersion: "latest", sourceType: "module", ecmaFeatures: { jsx: true } },
    },
    plugins: { "react-hooks": reactHooks },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "error",
    },
  },
];
