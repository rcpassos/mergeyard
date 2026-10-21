# Project website

The public landing page lives in `site/`, separate from the embedded local
dashboard in `web/`. It is plain HTML, CSS, JavaScript, and SVG, with no build
step, external fonts, analytics, or runtime dependencies.

## Theme

The site shares the dashboard's stock daisyUI `light` and `dark` themes and
follows the OS preference; there is no manual toggle. `site/theme.css` holds
only those theme variables, switched by `prefers-color-scheme`. The dashboard
asset build (`make assets`, or `npm run build` in `web/`) writes it from the
same daisyUI version the dashboard uses, along with `site/favicon.svg`, the
dashboard's theme-following favicon. Both files are generated and committed;
do not edit them by hand. CI rebuilds them and fails when the committed copies
differ, so after a daisyUI upgrade commit the regenerated files with it.

`site/style.css` is hand-written layout CSS. It sets no colors of its own: it
uses the theme variables, or `color-mix()` blends of them for muted text and
rules. Text uses the system sans and monospace stacks; there are no font files.

## Preview

From the repository root:

```sh
python3 -m http.server 8080 --directory site
```

Open `http://localhost:8080`. The workflow explorer and command copy control
enhance the static page; navigation, product explanations, and setup commands
also work without JavaScript. Clipboard access requires HTTPS or localhost and
may depend on browser permissions.

## GitHub Pages

In the repository's **Settings → Pages → Build and deployment**, select
**GitHub Actions** as the source. This is a one-time repository setting; the
workflow does not enable Pages or change repository settings itself.

After that, `.github/workflows/pages.yml` deploys changes to `site/` on `main`.
It can also be run manually using **Actions → Website → Run workflow** from
`main`. The `github-pages` environment must allow the deployment branch.

The expected project URL is `https://rcpassos.github.io/mergeyard/`. The
deployment's environment URL is the authoritative published address. All local
asset URLs are relative so the site works under the project path or a custom
domain. Only `site/` is uploaded; repository documentation stays on GitHub.

## Search and sharing

The canonical public URL is `https://rcpassos.github.io/mergeyard/`. The page
title, description, canonical link, Open Graph and Twitter card metadata are
included in the HTML. JSON-LD describes the website, page, software, preview
image, and author using only information visible on the page. It does not claim
ratings, prices, a release version, or eligibility for a Google rich result.

`site/sitemap.xml` lists the canonical landing page. Section fragments and
external GitHub documentation are not separate pages in this sitemap. No
synthetic modification dates are published.

The sharing image is `site/social-preview.png` (1200 × 630), with an editable
source in `site/social-preview.svg`, drawn in the light theme's colors with its
primary as the accent. After editing the SVG, export it to PNG at
its intrinsic size and visually verify the result. The SVG can be opened in a
browser and the root SVG captured at a 1200 × 630 viewport, device scale 1.
The PNG is committed; deployment needs no image renderer or build dependency.

If the public domain or path changes, update the canonical link, absolute
Open Graph/Twitter URLs, JSON-LD URLs, and sitemap together. CSS, JavaScript,
and favicon references remain relative for project-path compatibility.

### Crawling and production verification

This project is hosted under `/mergeyard/`. Crawlers read robots rules from
`https://rcpassos.github.io/robots.txt`, which this repository cannot publish.
A `site/robots.txt` would land at `/mergeyard/robots.txt` and have no effect,
so none is included. The page allows indexing through its robots meta tag;
there are no local crawling restrictions. Root-level robots rules, if present,
must also allow `/mergeyard/` and its assets.

After the Pages deployment succeeds:

1. Check that the canonical page, `sitemap.xml`, and `social-preview.png` return
   HTTP 200 over HTTPS and that the page has no `X-Robots-Tag: noindex` header.
2. Verify a URL-prefix property for `https://rcpassos.github.io/mergeyard/` in
   Google Search Console using the verification method provided for the account.
   No placeholder verification token is included in this repository.
3. Submit `https://rcpassos.github.io/mergeyard/sitemap.xml` and inspect the
   canonical page in Search Console. Request indexing and monitor the reported
   Google-selected canonical and indexing status there.
4. Check the live structured data with Schema.org's validator and check the
   sharing image with the target platform's preview tool. Google rich-result
   eligibility is separate from valid descriptive Schema.org markup.

Production crawlability, indexing, and social preview caches cannot be confirmed
from localhost. Sitemap submission and indexing require the live deployment;
Search Console also requires an authenticated, verified owner.

References: [canonical URLs](https://developers.google.com/search/docs/crawling-indexing/consolidate-duplicate-urls),
[sitemaps](https://developers.google.com/search/docs/crawling-indexing/sitemaps/build-sitemap),
[robots.txt location](https://developers.google.com/crawling/docs/robots-txt/create-robots-txt),
[structured data policies](https://developers.google.com/search/docs/appearance/structured-data/sd-policies),
and [Open Graph metadata](https://ogp.me/).

## Product content

Keep public product claims and setup commands aligned with the README and
current behavior. The initial page uses a source build because no release was
published when it was created. Switch to a binary installer only after its
release URL is available. The workflow illustration is an example, not a live
dashboard or a claim about a particular run.
