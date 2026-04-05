module.exports = {
  rules: {
    "no-restricted-imports": [
      "error",
      {
        patterns: [
          {
            group: ["@contenox/ui", "../../dist", "../dist", "*/dist"],
            message: "Use relative imports inside the UI package.",
          },
        ],
      },
    ],
  },
};
