# Code Generator Reference

Detailed reference for how the build scripts consume OpenAPI schemas and produce generated code.

## Table of Contents

1. [Bundle OpenAPI](#bundle-openapi)
2. [Go Generator](#go-generator)
3. [TypeScript Generator](#typescript-generator)
4. [RTK Query Generator](#rtk-query-generator)
5. [Package Discovery](#package-discovery)
6. [Troubleshooting](#troubleshooting)

## Bundle OpenAPI

**Script**: `build/bundle-openapi.js`
**Dependencies**: `swagger-cli`, `@redocly/cli`

### What it does

1. Walks `schemas/constructs/` and discovers packages (directories with `api.yml`)
2. Bundles each `api.yml` with `swagger-cli bundle --dereference` into JSON
3. Merges all bundles (minus excluded packages) using `@redocly/cli join`
4. Filters the merged spec by `x-internal` tags into separate outputs

### Outputs

| File | Contents |
|------|----------|
| `_openapi_build/constructs/<version>/<package>/merged-openapi.json` | Per-package bundled JSON |
| `_openapi_build/merged_openapi.yml` | Complete merged spec |
| `_openapi_build/cloud_openapi.yml` | Endpoints tagged `x-internal: cloud` |
| `_openapi_build/meshery_openapi.yml` | Endpoints tagged `x-internal: meshery` |

### Excluded from merge

- `v1alpha1/core` — reusable definitions, not a standalone API
- `v1alpha1/capability` — capability schemas

## Go Generator

**Script**: `build/generate-golang.js`
**Dependency**: `oapi-codegen` v2.5.1
**Prerequisite**: `bundle-openapi.js` must run first

### What it does

1. Reads bundled JSON from `_openapi_build/constructs/`
2. Collects `x-oapi-codegen-extra-tags` from all schemas
3. Builds import mappings from external `$ref` targets (so cross-package references resolve)
4. Generates Go structs via `oapi-codegen` with both JSON and YAML struct tags
5. Applies extra tags (gorm, db, etc.) post-generation

### Output location

`models/<version>/<package>/<package>.go`

### Key behaviors

- **YAML tag mirroring**: For every `json:"fieldName,omitempty"` tag, a matching `yaml:"fieldName,omitempty"` tag is generated automatically
- **Extra tags**: `x-oapi-codegen-extra-tags` values are injected as additional struct tags
- **Import mappings**: External `$ref` targets are mapped to Go import paths so cross-package types compile
- **Union types**: `oneOf` schemas generate union types using `json.RawMessage` + helper methods
- **Package naming**: Directory name becomes Go package name (with overrides, e.g., `design` → `pattern`)

### Schema features that affect Go output

| Schema feature | Go result |
|---------------|-----------|
| `type: string, format: uuid` | `openapi_types.UUID` |
| `x-go-type: "core.Map"` | Uses the specified Go type directly |
| `x-go-type-import` | Adds the specified import |
| `x-go-type-skip-optional-pointer` | Skips `*` prefix on optional fields |
| `x-oapi-codegen-extra-tags` | Adds custom struct tags |
| `oneOf` / `anyOf` | Union type with RawMessage |
| `$ref` to external schema | Import from the referenced package |
| `nullable: true` | Pointer type |

## TypeScript Generator

**Script**: `build/generate-typescript.js`
**Dependency**: `openapi-typescript` (via npx)
**Prerequisite**: `bundle-openapi.js` must run first

### What it does

1. Reads bundled JSON from `_openapi_build/constructs/`
2. Generates TypeScript type definitions via `openapi-typescript`
3. Also exports the raw JSON schema as a TypeScript const

### Output files

| File | Contents |
|------|----------|
| `typescript/generated/<version>/<package>/<Package>.ts` | Type definitions |
| `typescript/generated/<version>/<package>/<Package>Schema.ts` | JSON schema as const |

### Important: typescript/index.ts

This file is **manually maintained** and defines the public API surface for the npm package. When adding a new construct, you must update this file:

```typescript
// Add type import
import { components as NewComponents } from "./generated/v1beta1/newconstruct/Newconstruct";

// Add to namespace
export namespace v1beta1 {
  export type NewConstruct = NewComponents["schemas"]["NewConstruct"];
}
```

Note: The property name in `["schemas"]["..."]` matches the schema component name, which may differ in casing from what you expect. Check the generated `.ts` file to confirm.

## RTK Query Generator

**Script**: `build/generate-rtk.js`
**Dependency**: `@rtk-query/codegen-openapi`
**Prerequisite**: `bundle-openapi.js` must run first (uses filtered merged specs)

### Config files

| Config | Input spec | Output |
|--------|-----------|--------|
| `typescript/rtk/cloud-rtk-config.ts` | `cloud_openapi.yml` | `typescript/rtk/cloud.ts` |
| `typescript/rtk/meshery-rtk-config.ts` | `meshery_openapi.yml` | `typescript/rtk/meshery.ts` |

### How x-internal affects RTK

- Endpoints tagged `x-internal: ["cloud"]` go into `cloud.ts` hooks
- Endpoints tagged `x-internal: ["meshery"]` go into `meshery.ts` hooks
- Endpoints with no `x-internal` tag appear in the general merged spec but won't generate RTK hooks

## Package Discovery

**Module**: `build/lib/config.js`

The algorithm:
1. Walk `schemas/constructs/<version>/`
2. Find directories containing `api.yml`
3. Apply exclusions (`core`, `capability`)
4. Apply name overrides (`design` → `pattern`)
5. Return `{version, package}` tuples

This means:
- **Adding a new construct**: just create the directory with `api.yml` — it's auto-discovered
- **The directory name matters**: it becomes the Go package name and TypeScript module path
- **No registration needed**: the build system finds new constructs automatically

## Troubleshooting

### "Schema validation error" during bundle

Usually means invalid `$ref` path. Check:
- Relative path is correct from the current file's location
- Referenced file and schema name exist
- No typos in the component name after `#/components/schemas/`

### Go compilation errors after generation

- Missing imports: add `x-go-type-import` to the schema field
- Type conflicts: two schemas might define the same type name — use `x-go-type` to disambiguate
- Extra tags not appearing: ensure `x-oapi-codegen-extra-tags` is at the property level, not the schema level

### TypeScript build errors

- Import path issues: don't use `.d.ts` extension in imports
- Missing exports: update `typescript/index.ts` manually
- Schema name mismatch: check the generated `.ts` file for the actual names

### Package not discovered by build

- Ensure `api.yml` exists in the construct directory (not just `<construct>.yaml`)
- Check if the directory is in the exclusion list in `build/lib/config.js`
