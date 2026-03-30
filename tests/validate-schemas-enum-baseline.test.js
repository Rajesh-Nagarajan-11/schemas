const test = require("node:test");
const assert = require("node:assert/strict");

const { findNewNonLowercaseEnumValues } = require("../build/lib/enum-validation");

function makeDoc(enumValues) {
  return {
    components: {
      schemas: {
        Example: {
          type: "object",
          properties: {
            status: {
              type: "string",
              enum: enumValues,
            },
          },
        },
      },
    },
  };
}

test("preserves existing published mixed-case enum values from the baseline", () => {
  const findings = findNewNonLowercaseEnumValues(
    makeDoc(["ComponentsInDesign", "enabled"]),
    makeDoc(["ComponentsInDesign"]),
  );

  assert.deepEqual(findings, []);
});

test("flags only newly introduced non-lowercase enum values", () => {
  const findings = findNewNonLowercaseEnumValues(
    makeDoc(["ComponentsInDesign", "enabled", "NewFeatureGate"]),
    makeDoc(["ComponentsInDesign", "enabled"]),
  );

  assert.deepEqual(findings, [
    {
      path: 'Schema "Example".status',
      value: "NewFeatureGate",
      suggestedValue: "newfeaturegate",
    },
  ]);
});

test("does not flag lowercase enum additions", () => {
  const findings = findNewNonLowercaseEnumValues(
    makeDoc(["enabled", "ignored"]),
    makeDoc(["enabled"]),
  );

  assert.deepEqual(findings, []);
});