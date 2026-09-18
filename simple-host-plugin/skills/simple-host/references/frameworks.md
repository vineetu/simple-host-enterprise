# Framework builds for subpath hosting

Read this file completely before building a project for Simple Host. Detect the
framework from `package.json`, scripts, dependencies, and root config files.

Build with `/sites/<owner_username>/<sitename>/`, where `owner_username` is the
canonical resource owner—a person or a team, and not necessarily the
authenticated actor. Use both the leading and trailing slash unless a framework
rule below says otherwise.

**Compose to build, quote to report.** You assemble this base path yourself, from
the namespace you resolved, and every base path in this file is that composed
long path. A site is also served at `<owner-label>.<base>/<sitename>/`; a build
made for the long path works at *both* addresses and never needs rebuilding,
while a build made for the short one 404s on the base host. So always compile
long. Never build against the `url` or `public_path` the API returns — those are
values to show a human, and their spelling depends on which host the request
arrived on. Compose to build, quote to report.

Do not patch a compiled bundle with string replacement. Configure the framework
and rebuild.

## Vite (plain, Vue, React, Svelte, Preact, Lit)

Detect `vite` or `vite.config.{js,ts,mjs,cjs}`.

```bash
npx vite build --base=/sites/<owner_username>/<sitename>/
```

Equivalent config:

```js
export default { base: '/sites/<owner_username>/<sitename>/' }
```

Default output: `dist/`.

## Slidev

Detect `@slidev/cli`, or `slides.md` plus a Slidev package script.

```bash
npx slidev build --base /sites/<owner_username>/<sitename>/
```

Default output: `dist/`. The base starts and ends with `/`.

## Next.js

Detect `next` or `next.config.{js,mjs,ts}`. Simple Host requires a static export.

```js
module.exports = {
  basePath: '/sites/<owner_username>/<sitename>', // no trailing slash
  output: 'export',
  images: { unoptimized: true },
  trailingSlash: true,
}
```

```bash
npx next build
```

Upload `out/`. API routes, server actions requiring a server, middleware, and SSR
are not deployable here.

## Create React App

Detect `react-scripts`. Set:

```json
{"homepage":"/sites/<owner_username>/<sitename>"}
```

```bash
npm run build
```

Upload `build/`. For React Router, also configure `basename` with the same path
without a trailing slash.

## SvelteKit

Detect `@sveltejs/kit` or `svelte.config.{js,ts}`. Use the static adapter:

```js
import adapter from '@sveltejs/adapter-static';
export default {
  kit: {
    adapter: adapter({ fallback: 'index.html' }),
    paths: { base: '/sites/<owner_username>/<sitename>' }
  }
};
```

```bash
npm run build
```

Upload `build/`. `paths.base` has no trailing slash. In application code, use
`base` from `$app/paths` for internal links and assets.

## Astro

Detect `astro` or `astro.config.{mjs,ts,js}`.

```js
import { defineConfig } from 'astro/config';
export default defineConfig({
  base: '/sites/<owner_username>/<sitename>',
  output: 'static',
});
```

```bash
npx astro build
```

Upload `dist/`. Use `import.meta.env.BASE_URL` instead of hardcoding `/`.

## Nuxt 3 or 4

Detect `nuxt` or `nuxt.config.{ts,js,mjs}`.

```ts
export default defineNuxtConfig({
  app: { baseURL: '/sites/<owner_username>/<sitename>/' }
})
```

```bash
npx nuxt generate
```

Upload `.output/public/`. Do not upload the Node server bundle produced by
`nuxt build`.

## Angular

Detect `@angular/core` or `angular.json`.

```bash
ng build --base-href=/sites/<owner_username>/<sitename>/ --configuration=production
```

Upload the output directory that contains `index.html`: commonly
`dist/<project-name>/`, or `dist/<project-name>/browser/` with newer application
builders.

## Gatsby

Detect `gatsby` or `gatsby-config.{js,ts}`.

```js
module.exports = {
  pathPrefix: '/sites/<owner_username>/<sitename>',
}
```

```bash
npx gatsby build --prefix-paths
```

Upload `public/`. The `--prefix-paths` flag is required.

## Vue CLI

Detect `@vue/cli-service` or `vue.config.js` without Vite.

```js
module.exports = {
  publicPath: '/sites/<owner_username>/<sitename>/'
}
```

```bash
npm run build
```

Upload `dist/`.

## Plain static HTML

Treat a project as plain HTML only when it has no build system that owns asset
paths. Invoke the `fix-paths-for-subpath-hosting` skill on the site directory.
That skill rewrites genuinely raw HTML/CSS/JavaScript paths mechanically. Then
run packaging and validation.

Do not invoke the path-rewriter on framework source or compiled chunks.

## Unrecognized framework

For Eleventy, Hugo, Jekyll, Remix static export, Qwik, SolidStart, VitePress,
Docusaurus, or another unlisted tool:

1. Inspect the project and identify its exact framework/version.
2. Consult that framework's official documentation for its base path, subpath,
   path prefix, public path, or subdirectory deployment setting.
3. Configure `/sites/<owner_username>/<sitename>/`, applying that framework's
   trailing-slash rules, and rebuild.
4. Upload only a fully static output directory.
5. Fall back to `fix-paths-for-subpath-hosting` only if the output is genuinely
   plain HTML/CSS/JavaScript with no chunk loader or build-owned URL runtime.

If the framework cannot emit a static build for a subpath, explain the blocker;
do not disguise a server application as a deployable static site.
