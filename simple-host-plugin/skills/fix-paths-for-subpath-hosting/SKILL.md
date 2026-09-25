---
name: fix-paths-for-subpath-hosting
description: Make a static site's asset paths relative so it works wherever Simple Host serves it. First detects the framework — for any framework with a base-path build setting (Vite, Next.js, CRA, SvelteKit, Astro, Nuxt, Angular, Gatsby, Vue CLI), routes back to the simple-host skill's framework-specific build instructions. Mechanically rewrites root-relative paths to relative paths only for genuinely raw HTML projects with no build system. Use before deploying a plain HTML site, or when the deploy skill references this as a pre-deploy step.
---

# Fix Paths for Subpath Hosting

A Simple Host site is served at `<owner-host>/<name>/`, or at the root of its own host once it is restricted to named viewers. A root-relative path like `src="/assets/app.js"` 404s on the first; a path with the site name baked in 404s on the second. Relative paths (`assets/app.js`, `./`) work on both.

There are two ways to fix this, and **picking the right one is more important than how well you execute either**:

1. **For framework projects (Vite, Next, React, Svelte, Astro, Nuxt, Angular, Gatsby, etc.)**: set the framework's base path at build time. The build tool then bakes the correct paths into the output. **Do not mechanically rewrite the build output** — minified variable names shift build-to-build, dynamic-import chunk loaders prepend a configured base to every chunk, and string-replacement is fragile.
2. **For raw HTML/CSS/JS projects with no build step**: mechanically rewrite root-relative paths to relative paths in source. This is the only option for plain HTML, but it is a fallback, not a default.

## Step 1: Detect the framework first

Before doing anything else, check for these signals at the project root:

| Signal | Framework | Action |
|---|---|---|
| `package.json` has `vite` (and a `vite.config.*`) | Vite — also covers Vue/React/Svelte/Preact/Lit Vite templates | Use simple-host skill's "Vite" section |
| `package.json` has `@slidev/cli` | Slidev | Use simple-host skill's "Slidev" section |
| `package.json` has `next` (or `next.config.*`) | Next.js | Use simple-host skill's "Next.js" section |
| `package.json` has `react-scripts` | Create React App | Use simple-host skill's "CRA" section |
| `package.json` has `@sveltejs/kit` | SvelteKit | Use simple-host skill's "SvelteKit" section |
| `package.json` has `astro` (or `astro.config.*`) | Astro | Use simple-host skill's "Astro" section |
| `package.json` has `nuxt` (or `nuxt.config.*`) | Nuxt | Use simple-host skill's "Nuxt" section |
| `angular.json` exists | Angular | Use simple-host skill's "Angular" section |
| `package.json` has `gatsby` | Gatsby | Use simple-host skill's "Gatsby" section |
| `package.json` has `@vue/cli-service` | Vue CLI (legacy) | Use simple-host skill's "Vue CLI" section |
| Anything else with a `package.json` and a build script | Unrecognized framework | Search that framework's docs for "base path" / "subpath" / "public path" / "path prefix" — apply that config, rebuild, re-upload. **Do not** mechanically rewrite output. |
| **No `package.json`, OR `package.json` without a recognized framework dep AND without a build script** | Plain HTML | Continue with the mechanical rewrite below |

If a framework is detected, **stop here** and tell the user (or the calling agent) to use the simple-host skill's framework-specific section. That skill has the correct build flag, output directory, and pre-flight checks for every framework above. Trying to mechanically fix paths in a built bundle will silently break dynamic imports, code splitting, and asset loaders even if the static asset references look right at first glance.

## Step 2: Mechanical rewrite — only for plain HTML

Use this section ONLY if Step 1 concluded "Plain HTML." Convert root-relative paths to relative paths based on each file's directory depth within the site.

### Depth calculation

The **depth** of a file is the number of directory levels from the site root:

| File path | Depth | Prefix to use |
|-----------|-------|---------------|
| `index.html` | 0 | `./` (or just strip the leading `/`) |
| `about/index.html` | 1 | `../` |
| `tests/easy/index.html` | 2 | `../../` |

**Root-level files (depth 0):** Simply remove the leading `/`.
- `/css/style.css` becomes `css/style.css`
- `/favicon.svg` becomes `favicon.svg`

**Subdirectory files (depth 1+):** Prepend `../` for each level of depth.
- At depth 1: `/css/style.css` becomes `../css/style.css`
- At depth 2: `/css/style.css` becomes `../../css/style.css`

### 1. HTML Files

Find all root-relative `src`, `href`, `action`, and `content` attributes.

**Root-level file (`index.html`, depth 0):**
```html
<!-- Before -->
<link rel="stylesheet" href="/css/style.css">
<script src="/assets/app.js"></script>
<link rel="manifest" href="/manifest.webmanifest">

<!-- After -->
<link rel="stylesheet" href="css/style.css">
<script src="assets/app.js"></script>
<link rel="manifest" href="manifest.webmanifest">
```

**Subdirectory file (`about/index.html`, depth 1):**
```html
<!-- Before -->
<link rel="stylesheet" href="/css/style.css">
<a href="/index.html">Home</a>

<!-- After -->
<link rel="stylesheet" href="../css/style.css">
<a href="../index.html">Home</a>
```

### 2. JavaScript Files

**CRITICAL:** Different JavaScript APIs resolve paths relative to different base URLs. You MUST identify which resolution rule applies to each path:

| API / Context | Resolves relative to | Use depth of |
|---------------|---------------------|--------------|
| `fetch()`, `new Image()`, `element.href=`, `element.src=`, `navigator.serviceWorker.register()`, `new Worker()` | The **HTML page** that loaded the script | The loading HTML page |
| ES module `import()` and static `import` | The **module file** itself | The JS file's own depth |
| Service worker `importScripts()`, `caches.match()`, `self.registration` | The **worker file** itself | The worker file's own depth |
| Web worker `importScripts()`, `fetch()` inside worker | The **worker file** itself | The worker file's own depth |

**This means a SINGLE JS file may need MIXED depth treatments:**

```javascript
// assets/app-bundle.js (filesystem depth 1), loaded by index.html (depth 0)

// DOM/browser APIs → use PAGE depth (0):
var sw = "sw.js";                          // NOT "../sw.js"
var icon = "favicon.svg";                  // NOT "../favicon.svg"
navigator.serviceWorker.register(sw, {scope: "./"});
fetch("api/config.json");
new Worker("workers/compute.js");
document.querySelector("link").href = icon;

// ES module import() → use MODULE depth (1):
const mod = await import("./page-home.js");     // relative to the module itself
// or equivalently: import("../assets/page-home.js")
```

**How to determine which rule applies:** Look at what USES the path value, not where the string is declared. A `var url = "..."` might be used by `fetch(url)` (page-relative) or `import(url)` (module-relative) — check the usage.

#### Scripts loaded by HTML pages (bundled or not)

Any JS file loaded via `<script src="...">` uses the loading **page's** depth for DOM/browser API paths:

```javascript
// assets/index.js OR js/app.js — loaded by index.html (depth 0)
// Use depth 0 for fetch(), DOM manipulation, SW registration, Worker creation

// Before:
const manifest = `/manifest.webmanifest`;
const swPath = `/sw.js`;
navigator.serviceWorker.register(swPath, {scope: `/`})
fetch("/api/data.json")
new Worker("/workers/compute.js")

// After (depth 0 — strip leading /):
const manifest = `manifest.webmanifest`;
const swPath = `sw.js`;
navigator.serviceWorker.register(swPath, {scope: `./`})
fetch("api/data.json")
new Worker("workers/compute.js")

// WRONG (using script's filesystem depth 1):
const swPath = `../sw.js`;   // escapes the site root!
```

**The same rule applies to non-bundled scripts** — `js/utils.js` loaded by `index.html` uses depth 0 for browser APIs, not depth 1.

#### Standalone JS files (own execution context)

Service workers and web workers run in their own context — use their own filesystem depth. (See Section 4 for service worker specifics.)

#### Shared JS files loaded from pages at different depths

**If a `<script>` is loaded from multiple HTML pages at different depths** (e.g., `shared/nav.js` loaded by both `index.html` at depth 0 and `admin/dashboard/index.html` at depth 2), you CANNOT use a fixed depth — it differs per page. Use `document.currentScript.src` to compute the base path at runtime:

```javascript
// At the top of the shared JS file:
const scriptUrl = document.currentScript?.src || '';
// Remove the filename and its parent directory to get the site root
// e.g., "https://owner.host/mysite/shared/nav.js" -> "https://owner.host/mysite/"
const siteRoot = scriptUrl.replace(/\/[^/]+\/[^/]+$/, '/');

// Then use siteRoot for all dynamic paths:
const links = [
  { name: 'Home', href: siteRoot + 'index.html' },
  { name: 'About', href: siteRoot + 'about/index.html' }
];
```

**Why this approach:** `document.currentScript.src` gives the absolute URL of the script file itself. By stripping the filename and its containing directory, you get the site root URL. This works regardless of which HTML page loaded the script and at what depth.

**Adjust the replace pattern based on the script's depth within the site:**
- Script at `shared/nav.js` (depth 1): strip 2 segments (`/shared/nav.js`) -> `replace(/\/[^/]+\/[^/]+$/, '/')`
- Script at `js/lib/utils.js` (depth 2): strip 3 segments -> `replace(/\/[^/]+\/[^/]+\/[^/]+$/, '/')`

### 3. CSS Files

Convert `url()` references the same way as HTML, based on the CSS file's depth:

```css
/* Before (css/style.css, depth 1): */
background-image: url('/images/bg.png');

/* After: */
background-image: url('../images/bg.png');
```

### 4. Service Workers

Simple Host refuses to serve service-worker scripts, so a site that registers one
should drop the registration instead of fixing its paths. The notes below apply
only to ordinary web workers and to sites hosted elsewhere.

Service workers are a special case because they use root-relative paths for caching and scope registration.

**In the service worker file itself** (typically `sw.js` at depth 0), convert cached asset paths:

```javascript
// Before:
const SHELL_ASSETS = ['/', '/index.html', '/manifest.webmanifest', '/favicon.svg'];

// After:
const SHELL_ASSETS = ['./', 'index.html', 'manifest.webmanifest', 'favicon.svg'];
```

Also fix any `caches.match('/')` or `cache.put('/index.html', ...)` calls:
```javascript
// Before:
caches.match('/index.html')
caches.match('/')

// After:
caches.match('index.html')
caches.match('./')
```

**In the JS that registers the service worker**, fix the scope:

```javascript
// Before:
navigator.serviceWorker.register('/sw.js', { scope: '/' });

// After:
navigator.serviceWorker.register('sw.js', { scope: './' });
```

**Note:** Using `./` for scope makes the service worker scope relative to its registration location, which will be the site's subpath.

### 5. Web App Manifest (`manifest.webmanifest` / `manifest.json`)

Fix `start_url`, `scope`, and icon paths:

```json
{
  "start_url": "./",
  "scope": "./",
  "icons": [
    { "src": "icon-192.png", "sizes": "192x192" },
    { "src": "icon-512.png", "sizes": "512x512" }
  ]
}
```

**Key:** `start_url` and `scope` should be `"./"` (relative to the manifest file's location) rather than `"/"` (domain root).

## Implementation Steps

1. **Inventory:** Scan ALL files (HTML, JS, CSS, JSON, webmanifest) for root-relative paths starting with `/`
2. **Compute depth:** For each file, count directory separators in its path relative to the site root
3. **Apply fixes by file type:**
   - HTML: fix `src`, `href`, `action`, `content` attributes
   - JS: fix string literals; for shared scripts loaded at multiple depths, use `document.currentScript.src`
   - CSS: fix `url()` references
   - Service worker: fix cached paths, `caches.match()` calls, and scope
   - Manifest: fix `start_url`, `scope`, and icon `src` values
4. **Verify:** Scan for remaining root-relative paths:
   - macOS/Linux: `grep -rn '"/[a-zA-Z]' .` and `grep -rn "'/[a-zA-Z]" .`
   - Windows PowerShell: `Get-ChildItem -Recurse -File | Select-String -Pattern '"/[a-zA-Z]', "'/[a-zA-Z]"`
5. **Test:** Open the `url` the deploy returned and check browser console for 404s

## Common Pitfalls

- **Mixed resolution in one file:** A bundled ES module may use BOTH `fetch()` (page-relative, use page depth) AND `import()` (module-relative, use file depth). Check what USES each path, not just where the string is defined.
- **Script depth mistake:** ANY JS file loaded via `<script>` must use the loading HTML page's depth for browser APIs (`fetch`, `register`, `new Worker`, DOM assignments), not the script's filesystem depth. Using `../` from the wrong depth escapes the site root.
- **Shared script loaded at multiple depths:** If `shared/app.js` is loaded by both `index.html` (depth 0) and `admin/panel/index.html` (depth 2), a fixed relative path won't work for both. Use `document.currentScript.src` to compute the base at runtime.
- **Iframe pages have their own depth:** An iframe at `frames/widgets/tool.html` (depth 2) loading `<script src="../../js/helper.js">` — the script's browser API calls resolve relative to the iframe page (depth 2), not the parent page.
- **Forgetting the manifest:** `start_url: "/"` silently breaks PWA install — the app opens at domain root instead of the subpath
- **Service worker scope:** A service worker registered with `scope: "/"` won't control pages under the subpath
- **Bundled JS:** Build tools (Vite, Webpack) may inline root-relative paths at build time — check the output bundle, not just source files
- **`<base>` tag caution:** Avoid `<base href>` as it affects ALL relative URLs on the page, including anchor fragments (`#section`) and form actions
