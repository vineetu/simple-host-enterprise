# Framework builds

Read this file completely before building a project for Simple Host. Detect the
framework from `package.json`, scripts, dependencies, and root config files.

Every site is served at the root of its own host,
`<sitename>.<owner-label>.<base>/`, with no `/<sitename>/` segment. For a short
time after an owner's first site is created, a site may be served at
`<owner-label>.<base>/<sitename>/` instead. **Build with relative asset paths
(`./`)**: a page that loads `./assets/app.js` works at both. A root-absolute
path (`/assets/app.js`) works only on the site's own host, and a path with the
site name in it (`/<sitename>/assets/app.js`) only at the short-lived address.

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

## Frameworks that build for the root

Next.js, Astro, Nuxt, and Gatsby cannot build reliably with relative paths.
Build them for the root of the site's own host: leave the base path unset (no
`basePath`, `base`, `baseURL`, or `pathPrefix`). If the deploy response's `url`
is still the `<owner-label>.<base>/<sitename>/` form, their `/`-prefixed assets
do not load there yet: tell the user, check `url` again with `get_site` later,
and verify once it is the site's own host. Do not rebuild with a base path.

| Framework | Detect | Setting | Build | Upload |
|---|---|---|---|---|
| Next.js | `next` | `output: 'export'`, `images: { unoptimized: true }`, `trailingSlash: true`, no `basePath` | `npx next build` | `out/` |
| Astro | `astro` | `output: 'static'`, no `base` | `npx astro build` | `dist/` |
| Nuxt 3/4 | `nuxt` | no `app.baseURL` (defaults to `/`) | `npx nuxt generate` | `.output/public/` |
| Gatsby | `gatsby` | no `pathPrefix` | `npx gatsby build` | `public/` |

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
3. If it only supports an absolute base, use `/`, like the table above.
4. Upload only a fully static output directory.

If the framework cannot emit a static build, explain the blocker; do not
disguise a server application as a deployable static site.
