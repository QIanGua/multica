import nextConfig from "@multica/eslint-config/next";

export default [
  ...nextConfig,
  { ignores: [".next/", ".source/"] },
  {
    files: ["**/*.test.{ts,tsx}", "**/test/**/*.{ts,tsx}"],
    rules: {
      "react/display-name": "off",
    },
  },
  {
    // The service worker runs in ServiceWorkerGlobalScope, not a window, so
    // `self` and `caches` are its globals rather than undefined identifiers.
    files: ["public/sw.js"],
    languageOptions: {
      globals: {
        self: "readonly",
        caches: "readonly",
        clients: "readonly",
      },
    },
  },
];
