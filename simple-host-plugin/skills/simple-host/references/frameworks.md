# Framework builds

Read this file completely before building a project for Simple Host. Detect the
framework from `package.json`, scripts, dependencies, and root config files.

A site is served at `<owner-label>.<base>/<sitename>/`, and at its own host
(`<owner-label>--<sitename-label>.<base>/`, no `/<sitename>/` segment) once it
is restricted to named viewers. **Build with relative asset paths (`./`)**: a
page that loads `./assets/app.js` works at both, and never needs rebuilding when
the site is restricted or unrestricted. An absolute path (`/assets/app.js`, or
`/<sitename>/assets/app.js`) breaks on one of the two.

Use hash-based client routing (`#/page`) for a single-page app: a relative base
cannot serve history-mode deep links.

Do not patch a compiled bundle with string replacement. Configure the framework
and rebuild.

## Vite (plain, Vue, React, Svelte, Preact, Lit)

Detect `vite` or `vite.config.{js,ts,mjs,cjs}`.

```bash
npx vite build --base=./
```

Equivalent config: `export default { base: './' }`. Default output: `dist/`.
With Vue Router use `createWebHashHistory()`; with React Router use
`HashRouter`.

## Slidev

Detect `@slidev/cli`, or `slides.md` plus a Slidev package script. Set
`routerMode: hash` in the first slide's frontmatter, then:

```bash
npx slidev build --base ./
```

Default output: `dist/`.

## Create React App

Detect `react-scripts`. Set `"homepage": "."` in `package.json`, then
`npm run build`. Upload `build/`. Use `HashRouter` for routing.

## Vue CLI

Detect `@vue/cli-service` or `vue.config.js` without Vite. Set
`module.exports = { publicPath: './' }`, then `npm run build`. Upload `dist/`.
Use hash history for Vue Router.

## Angular

Detect `@angular/core` or `angular.json`.

```bash
ng build --base-href ./ --configuration=production
```

Use `withHashLocation()` (or `useHash: true`) for the router. Upload the
directory that contains `index.html`: commonly `dist/<project-name>/`, or
`dist/<project-name>/browser/`.

## SvelteKit

Detect `@sveltejs/kit` or `svelte.config.{js,ts}`. Use the static adapter, leave
`paths.base` unset, and keep `paths.relative` at its default (`true`):

```js
import adapter from '@sveltejs/adapter-static';
export default { kit: { adapter: adapter({ fallback: 'index.html' }), router: { type: 'hash' } } };
```

`npm run build`, then upload `build/`.

## Frameworks that need an absolute base

Next.js, Astro, Nuxt, and Gatsby cannot build reliably with relative paths.
Build them with the site's path on its owner's host, `/<sitename>` (the setting
below), and tell the user: if the site is later restricted to named viewers it
moves to its own host, and must be rebuilt with no base path and deployed again.

| Framework | Detect | Setting | Build | Upload |
|---|---|---|---|---|
| Next.js | `next` | `basePath: '/<sitename>'`, `output: 'export'`, `images: { unoptimized: true }`, `trailingSlash: true` | `npx next build` | `out/` |
| Astro | `astro` | `base: '/<sitename>'`, `output: 'static'` | `npx astro build` | `dist/` |
| Nuxt 3/4 | `nuxt` | `app: { baseURL: '/<sitename>/' }` | `npx nuxt generate` | `.output/public/` |
| Gatsby | `gatsby` | `pathPrefix: '/<sitename>'` | `npx gatsby build --prefix-paths` | `public/` |

API routes, server actions, middleware, and SSR are not deployable here.

## Plain static HTML

Treat a project as plain HTML only when it has no build system that owns asset
paths. Invoke the `fix-paths-for-subpath-hosting` skill on the site directory,
then run packaging and validation. Do not invoke it on framework source or
compiled chunks.

## Unrecognized framework

For Eleventy, Hugo, Jekyll, VitePress, Docusaurus, or another unlisted tool:

1. Identify the exact framework and version.
2. Check its official documentation for relative asset paths (often a
   `base`, `publicPath`, or `relativeURLs` setting). Use `./` where supported.
3. If it only supports an absolute base, treat it like the table above.
4. Upload only a fully static output directory.

If the framework cannot emit a static build, explain the blocker; do not
disguise a server application as a deployable static site.
